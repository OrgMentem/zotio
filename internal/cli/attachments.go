// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// `attachments add` — attach a local file to an EXISTING item as a synced
// stored (imported_file) child through the Zotero Web API's three-step
// file-upload protocol: create child -> authorize upload -> upload + register.
// https://www.zotero.org/support/dev/web_api/v3/file_upload
//
// Retry safety: before creating anything, the parent's imported_file children
// are reconciled by filename + registered MD5. An identical retry no-ops, a
// crashed run's pending child (no registered file) is resumed, and different
// content under the same filename is a conflict — never a silent overwrite.

package cli

import (
	"context"
	"crypto/md5" //nolint:gosec // G501: Zotero's upload protocol identifies file content by MD5; not a security primitive here.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/mutation"
)

// zoteroItemKeyRE keeps user-supplied keys out of URL path tricks; real Zotero
// keys are 8 uppercase alphanumerics, tolerated up to 32 for forks/tests.
var zoteroItemKeyRE = regexp.MustCompile(`^[A-Za-z0-9]{1,32}$`)

func newAttachmentsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attachments",
		Short: "Attachment file operations",
	}
	cmd.AddCommand(newAttachmentsAddCmd(flags))
	return cmd
}

// newAttachmentsAddCmd uploads a local file as a stored child of an existing item.
func newAttachmentsAddCmd(flags *rootFlags) *cobra.Command {
	var mode string
	var title string

	cmd := &cobra.Command{
		Use:   "add <parent-key> <file>",
		Short: "Attach a local file to an existing item as a synced stored attachment",
		Long: `Upload a local file (typically a PDF) as a stored (imported_file) child of an
existing item through the Zotero Web API file-upload protocol. Stored
attachments sync to all devices, unlike linked-file children.

Retry-safe: if the parent already has an imported_file child with the same
filename and identical content, the command no-ops; a child left behind by an
interrupted run is resumed; same filename with different content is reported
as a conflict for manual review instead of being duplicated or overwritten.

By default this previews the planned upload; apply with --yes.`,
		Example: "  zotio attachments add AB3DE6F8 ./paper.pdf --yes",
		Args:    cobra.ExactArgs(2),
		Annotations: map[string]string{
			"zotio:method": "POST",
			"zotio:path":   "/items/{key}/file",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if mode != "stored" {
				return usageErr(fmt.Errorf("--mode must be stored (linked-file children are created by `import apply --attach-mode linked-file`)"))
			}
			parentKey := strings.TrimSpace(args[0])
			if !zoteroItemKeyRE.MatchString(parentKey) {
				return usageErr(fmt.Errorf("invalid parent item key %q", parentKey))
			}
			req, err := newStoredUploadRequest(parentKey, args[1], title)
			if err != nil {
				return err
			}

			var c *client.Client
			if resolveMutationMode(flags).Apply {
				c, err = flags.newWriteClient()
				if err != nil {
					return err
				}
			}

			op := mutation.Op{
				ID:   "attachments.add:001:stored",
				Key:  parentKey,
				Kind: "attachment_upload",
				Changes: []mutation.Change{{
					Field: "attachment",
					Add:   fmt.Sprintf("stored -> %s (%d bytes, md5 %s)", req.Filename, len(req.Data), req.MD5[:8]),
				}},
				Apply: func() (string, any, error) {
					return applyStoredUpload(cmd.Context(), c, req, flags)
				},
			}
			env, runErr := runMutation(cmd.Context(), flags, "attachments.add", []mutation.Op{op})
			if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
				return renderErr
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "stored", "Attachment handling: stored (synced imported_file upload)")
	cmd.Flags().StringVar(&title, "title", "", "Attachment title (default: the file name)")
	return cmd
}

// applyStoredUpload maps upload outcomes onto mutation-engine statuses so both
// `attachments add` and `import apply --attach-mode stored` report identically.
func applyStoredUpload(ctx context.Context, c *client.Client, req storedUploadRequest, flags *rootFlags) (string, any, error) {
	if c == nil {
		return "failed", nil, fmt.Errorf("missing write client")
	}
	outcome, err := uploadStoredAttachment(ctx, c, req, flags)
	var conflict *storedConflictError
	if errors.As(err, &conflict) {
		return "conflict", conflict.Error(), nil
	}
	if err != nil {
		return "failed", nil, err
	}
	if outcome.Status == storedUploadReused {
		return "no_op", map[string]any{"item_key": outcome.Key, "note": "identical stored attachment already present"}, nil
	}
	return "applied", map[string]any{"item_key": outcome.Key, "upload": outcome.Status}, nil
}

// storedUploadRequest carries everything the upload protocol needs for one file.
type storedUploadRequest struct {
	ParentKey   string
	Path        string
	Filename    string
	Title       string
	ContentType string
	Data        []byte
	MD5         string
	MtimeMS     int64
	WriteToken  string
}

// Upload outcome statuses.
const (
	storedUploadUploaded = "uploaded" // fresh child created and file registered
	storedUploadResumed  = "resumed"  // pending child from an earlier run completed
	storedUploadReused   = "reused"   // identical file already attached; no write
)

// storedUploadOutcome reports which path the protocol took.
type storedUploadOutcome struct {
	Key    string
	Status string
}

// storedConflictError marks outcomes that need human review; callers report
// them as mutation status "conflict" rather than a hard failure.
type storedConflictError struct{ msg string }

func (e *storedConflictError) Error() string { return e.msg }

// newStoredUploadRequest validates the local file and precomputes the protocol
// inputs (MD5, mtime, content type, deterministic write token). Reading the
// file here keeps preview network-free while showing real plan evidence.
func newStoredUploadRequest(parentKey, path, title string) (storedUploadRequest, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return storedUploadRequest{}, usageErr(fmt.Errorf("resolving attachment path: %w", err))
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return storedUploadRequest{}, usageErr(fmt.Errorf("attachment file: %w", err))
	}
	if !info.Mode().IsRegular() {
		return storedUploadRequest{}, usageErr(fmt.Errorf("attachment %s is not a regular file", absPath))
	}
	if info.Size() == 0 {
		return storedUploadRequest{}, usageErr(fmt.Errorf("attachment %s is empty", absPath))
	}
	data, err := os.ReadFile(absPath) //nolint:gosec // G304: uploading a user-named local file is the command's purpose.
	if err != nil {
		return storedUploadRequest{}, fmt.Errorf("reading attachment: %w", err)
	}
	sum := md5.Sum(data) //nolint:gosec // G401: Zotero's upload authorization is keyed by MD5.
	md5hex := hex.EncodeToString(sum[:])
	filename := filepath.Base(absPath)
	if strings.TrimSpace(title) == "" {
		title = filename
	}
	// Deterministic write token: an identical retry replays the same token, so
	// Zotero rejects a duplicate create (412) instead of making a second child.
	tok := sha256.Sum256([]byte("zotio.attachments.add\x00" + parentKey + "\x00" + filename + "\x00" + md5hex))
	return storedUploadRequest{
		ParentKey:   parentKey,
		Path:        absPath,
		Filename:    filename,
		Title:       title,
		ContentType: storedAttachmentContentType(filename, data),
		Data:        data,
		MD5:         md5hex,
		MtimeMS:     info.ModTime().UnixMilli(),
		WriteToken:  hex.EncodeToString(tok[:16]),
	}, nil
}

func storedAttachmentContentType(filename string, data []byte) string {
	if strings.EqualFold(filepath.Ext(filename), ".pdf") {
		return "application/pdf"
	}
	return http.DetectContentType(data)
}

// uploadStoredAttachment runs the full reconcile-create-authorize-upload-register
// protocol for one file against one existing parent item.
func uploadStoredAttachment(ctx context.Context, c *client.Client, req storedUploadRequest, flags *rootFlags) (storedUploadOutcome, error) {
	sibling, err := findStoredSibling(c, req.ParentKey, req.Filename, req.MD5)
	if err != nil {
		return storedUploadOutcome{}, err
	}
	if sibling.ConflictKey != "" {
		return storedUploadOutcome{}, &storedConflictError{fmt.Sprintf(
			"item %s already has stored attachment %q (key %s) with different content (md5 %s, ours %s); refusing to duplicate or overwrite — review manually",
			req.ParentKey, req.Filename, sibling.ConflictKey, sibling.ConflictMD5, req.MD5)}
	}
	if sibling.ExistingKey != "" {
		return storedUploadOutcome{Key: sibling.ExistingKey, Status: storedUploadReused}, nil
	}

	key := sibling.PendingKey
	status := storedUploadResumed
	if key == "" {
		status = storedUploadUploaded
		key, err = createStoredAttachmentChild(c, req, flags)
		if err != nil {
			return storedUploadOutcome{}, err
		}
	}
	if err := registerStoredFile(ctx, c, key, req, flags); err != nil {
		return storedUploadOutcome{}, fmt.Errorf("attachment item %s created but its file is not registered (a retry resumes it): %w", key, err)
	}
	return storedUploadOutcome{Key: key, Status: status}, nil
}

// storedSibling classifies the parent's imported_file children sharing the
// requested filename so retries resume or no-op instead of duplicating.
type storedSibling struct {
	ExistingKey string // same filename, same registered md5 (fully uploaded)
	PendingKey  string // same filename, no registered md5 (upload never registered)
	ConflictKey string // same filename, different registered md5 (user content)
	ConflictMD5 string
}

func findStoredSibling(c *client.Client, parentKey, filename, md5hex string) (storedSibling, error) {
	// Live read (bypasses the read cache) so a retry observes a child created
	// moments ago by a crashed run.
	data, _, err := c.GetWithVersion("/items/"+parentKey+"/children", map[string]string{"itemType": "attachment"})
	if err != nil {
		var respErr *client.APIError
		if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
			return storedSibling{}, notFoundErr(fmt.Errorf("parent item %s not found", parentKey))
		}
		return storedSibling{}, fmt.Errorf("listing children of %s: %w", parentKey, err)
	}
	var rows []struct {
		Key  string `json:"key"`
		Data struct {
			ItemType string `json:"itemType"`
			LinkMode string `json:"linkMode"`
			Filename string `json:"filename"`
			MD5      string `json:"md5"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return storedSibling{}, fmt.Errorf("parsing children of %s: %w", parentKey, err)
	}
	var sibling storedSibling
	for _, row := range rows {
		if row.Data.ItemType != "attachment" || row.Data.LinkMode != "imported_file" || row.Data.Filename != filename {
			continue
		}
		switch row.Data.MD5 {
		case md5hex:
			sibling.ExistingKey = row.Key
			return sibling, nil
		case "":
			if sibling.PendingKey == "" {
				sibling.PendingKey = row.Key
			}
		default:
			sibling.ConflictKey = row.Key
			sibling.ConflictMD5 = row.Data.MD5
		}
	}
	return sibling, nil
}

// createStoredAttachmentChild POSTs the imported_file child under the parent.
func createStoredAttachmentChild(c *client.Client, req storedUploadRequest, flags *rootFlags) (string, error) {
	item := map[string]any{
		"itemType":    "attachment",
		"linkMode":    "imported_file",
		"parentItem":  req.ParentKey,
		"title":       req.Title,
		"filename":    req.Filename,
		"contentType": req.ContentType,
	}
	resp, _, err := c.PostWithHeaders("/items", []map[string]any{item}, map[string]string{"Zotero-Write-Token": req.WriteToken})
	if err != nil {
		var respErr *client.APIError
		if errors.As(err, &respErr) && respErr.StatusCode == http.StatusPreconditionFailed {
			// The deterministic write token was already accepted once (lost
			// response). Reconcile instead of creating a second child.
			sibling, rerr := findStoredSibling(c, req.ParentKey, req.Filename, req.MD5)
			if rerr == nil {
				if sibling.ExistingKey != "" {
					return sibling.ExistingKey, nil
				}
				if sibling.PendingKey != "" {
					return sibling.PendingKey, nil
				}
			}
			return "", &storedConflictError{fmt.Sprintf(
				"write token for %q under %s was already submitted but no matching attachment was found; review the item manually",
				req.Filename, req.ParentKey)}
		}
		return "", classifyStoredUploadError("creating stored attachment item", err, flags)
	}
	key, ok := createdItemKey(resp)
	if !ok {
		return "", fmt.Errorf("could not read created attachment key from /items response")
	}
	return key, nil
}

// registerStoredFile runs upload authorization, the payload upload, and upload
// registration for an existing child attachment that has no registered file.
func registerStoredFile(ctx context.Context, c *client.Client, key string, req storedUploadRequest, flags *rootFlags) error {
	form := url.Values{}
	form.Set("md5", req.MD5)
	form.Set("filename", req.Filename)
	form.Set("filesize", strconv.Itoa(len(req.Data)))
	form.Set("mtime", strconv.FormatInt(req.MtimeMS, 10))
	resp, _, err := c.PostFormWithHeaders("/items/"+key+"/file", form, map[string]string{"If-None-Match": "*"})
	if err != nil {
		return classifyStoredUploadError("authorizing upload", err, flags)
	}
	var auth struct {
		Exists      int    `json:"exists"`
		URL         string `json:"url"`
		ContentType string `json:"contentType"`
		Prefix      string `json:"prefix"`
		Suffix      string `json:"suffix"`
		UploadKey   string `json:"uploadKey"`
	}
	if err := json.Unmarshal(resp, &auth); err != nil {
		return fmt.Errorf("parsing upload authorization: %w", err)
	}
	if auth.Exists == 1 {
		return nil // the server already holds these bytes; association is complete
	}
	if auth.URL == "" || auth.UploadKey == "" {
		return fmt.Errorf("upload authorization missing url/uploadKey")
	}
	if err := postUploadPayload(ctx, c, auth.URL, auth.ContentType, auth.Prefix, auth.Suffix, req.Data); err != nil {
		return err
	}
	reg := url.Values{}
	reg.Set("upload", auth.UploadKey)
	if _, _, err := c.PostFormWithHeaders("/items/"+key+"/file", reg, map[string]string{"If-None-Match": "*"}); err != nil {
		return classifyStoredUploadError("registering upload", err, flags)
	}
	return nil
}

// postUploadPayload POSTs prefix+file+suffix to the storage URL returned by
// upload authorization. The URL is bearer-signed: never log or persist it.
func postUploadPayload(ctx context.Context, c *client.Client, uploadURL, contentType, prefix, suffix string, data []byte) error {
	u, err := url.Parse(uploadURL)
	if err != nil || !uploadURLTrusted(u) {
		host := "<unparseable>"
		if u != nil {
			host = u.Scheme + "://" + u.Host
		}
		return fmt.Errorf("refusing file upload to untrusted storage host %s", host)
	}
	body := make([]byte, 0, len(prefix)+len(data)+len(suffix))
	body = append(body, prefix...)
	body = append(body, data...)
	body = append(body, suffix...)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("building upload request: %w", err)
	}
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("uploading file payload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("file payload upload returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// uploadURLTrusted permits Zotero's HTTPS storage endpoints plus loopback test
// servers, mirroring the client's base-URL trust rule.
func uploadURLTrusted(u *url.URL) bool {
	if u == nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// classifyStoredUploadError maps the upload protocol's documented failure codes
// to actionable messages, falling back to the generic API classifier.
func classifyStoredUploadError(stage string, err error, flags *rootFlags) error {
	var respErr *client.APIError
	if errors.As(err, &respErr) {
		switch respErr.StatusCode {
		case http.StatusForbidden:
			return authErr(fmt.Errorf("%s: file editing is denied for this library/API key (HTTP 403): %w", stage, err))
		case http.StatusConflict:
			return apiErr(fmt.Errorf("%s: the target library is locked (HTTP 409); retry once sync settles: %w", stage, err))
		case http.StatusPreconditionFailed:
			return apiErr(fmt.Errorf("%s: the attachment changed remotely since it was read (HTTP 412); re-run to reconcile: %w", stage, err))
		case http.StatusRequestEntityTooLarge:
			return apiErr(fmt.Errorf("%s: the upload exceeds the library owner's Zotero storage quota (HTTP 413): %w", stage, err))
		case http.StatusPreconditionRequired:
			return apiErr(fmt.Errorf("%s: missing If-Match/If-None-Match precondition (HTTP 428) — this is a zotio bug: %w", stage, err))
		}
	}
	return classifyAPIError(fmt.Errorf("%s: %w", stage, err), flags)
}
