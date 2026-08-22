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
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: Zotero's upload protocol identifies file content by MD5; not a security primitive here.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
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

// usesConnectorReparentRoute is the single definition of "this invocation takes
// the connector re-parent route". Both the command and the storage precondition
// that waives itself for that route ask this one function, so the waiver and the
// behaviour cannot drift apart in separate files.
func usesConnectorReparentRoute(mode string, flags *rootFlags) bool {
	return strings.TrimSpace(mode) == "stored" && strings.TrimSpace(flags.via) == "connector"
}

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
		Short: "Attach a local file to an existing item",
		Long: `Attach a local file (typically a PDF) to an existing item. Mode "stored"
uploads an imported_file child that syncs to all devices. Mode "linked-file"
records the absolute local path without consuming Zotero storage quota; the
bytes remain local to this machine.

A stored upload through the Zotero Web API always lands in Zotero's OWN cloud
storage and is billed against that storage plan. When Zotero desktop keeps files
elsewhere (a personal WebDAV server, or file syncing turned off) that upload is
refused rather than silently misrouted.

--via connector is the local route for that case. Zotero's connector cannot
attach to an item that already exists, so this creates a temporary parent and
the file in one connector session — putting the bytes in whatever file store the
desktop actually uses — then moves the attachment onto your item and trashes the
temporary parent. Moving it relocates no bytes: every storage name derives from
the attachment's own key, not its parent's. Requires Zotero desktop running.

The route is opt-in and never chosen by --via auto, because it creates and
trashes a temporary item in your library. Pass --allow-zotero-cloud instead to
accept the cloud upload.

All routes are retry-safe. Stored files reconcile by filename and registered
MD5, and linked files by absolute path. The connector route reconciles on
content hash alone, because Zotero names the stored file after the parent item
rather than after your file: an identical retry no-ops, and a run interrupted
before the move is resumed rather than duplicated. Two limits are worth knowing:
a retry issued before Zotero has registered the hash can still add a second
copy, and two simultaneous runs against one item can both add one.

By default this previews the planned attachment; apply with --yes.`,
		Args: cobra.ExactArgs(2),
		Annotations: map[string]string{
			"zotio:method": "POST",
			"zotio:path":   "/items/{key}/file",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			mode = strings.TrimSpace(mode)
			if mode != "stored" && mode != "linked-file" {
				return usageErr(fmt.Errorf("--mode must be stored or linked-file"))
			}
			parentKey := strings.TrimSpace(args[0])
			if !zoteroItemKeyRE.MatchString(parentKey) {
				return usageErr(fmt.Errorf("invalid parent item key %q", parentKey))
			}
			var req storedUploadRequest
			var err error
			if mode == "linked-file" {
				req, err = newLinkedFileRequest(parentKey, args[1], title)
			} else {
				req, err = newStoredUploadRequest(parentKey, args[1], title)
			}
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

			// --via connector selects the local route: create a temporary parent
			// plus the file in one connector session, move the attachment onto
			// the real item, trash the temporary parent. It is never selected by
			// "auto", because creating and trashing an item in the operator's
			// library is not a routing detail they should discover afterwards.
			viaConnector := usesConnectorReparentRoute(mode, flags)
			if viaConnector && flags.group != "" {
				return usageErr(fmt.Errorf("--via connector cannot honor --group; the desktop connector has no group parameter"))
			}

			kind := "attachment_upload"
			var change string
			if mode == "linked-file" {
				kind = "attachment_link"
				change = "linked-file -> " + req.Path
			} else {
				change = fmt.Sprintf("stored -> %s (%d bytes, md5 %s)", req.Filename, req.Size, req.MD5[:8])
				if viaConnector {
					change += " via desktop connector, then re-parented"
				}
			}
			op := mutation.Op{
				ID:      "attachments.add:001:" + mode,
				Key:     parentKey,
				Kind:    kind,
				Changes: []mutation.Change{{Field: "attachment", Add: change}},
				Apply: func() (string, any, error) {
					switch {
					case mode == "linked-file":
						return applyLinkedAttachment(c, req, flags)
					case viaConnector:
						return applyConnectorReparentUpload(cmd.Context(), cmd, flags, c, req)
					default:
						return applyStoredUpload(cmd.Context(), c, req, flags)
					}
				},
			}
			env, runErr := runMutation(cmd.Context(), flags, "attachments.add", []mutation.Op{op})
			if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
				return renderErr
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "stored", "Attachment handling: stored (synced) or linked-file (local path)")
	cmd.Flags().StringVar(&title, "title", "", "Attachment title (default: the file name)")
	return cmd
}

// applyStoredUpload maps upload outcomes onto mutation-engine statuses so both
// `attachments add` and `import apply --attach-mode stored` report identically.
func applyStoredUpload(ctx context.Context, c *client.Client, req storedUploadRequest, flags *rootFlags) (string, any, error) {
	// Backstop for the routes preflight cannot decide in advance: import
	// apply's per-entry fallback to the Web route, and import pdf's
	// retro-attach onto an already-existing duplicate.
	if err := guardStoredUpload(ctx, flags); err != nil {
		return "failed", nil, err
	}
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

func applyLinkedAttachment(c *client.Client, req storedUploadRequest, flags *rootFlags) (string, any, error) {
	if c == nil {
		return "failed", nil, fmt.Errorf("missing write client")
	}
	key, created, err := ensureLinkedAttachment(c, req, flags)
	var conflict *storedConflictError
	if errors.As(err, &conflict) {
		return "conflict", conflict.Error(), nil
	}
	if err != nil {
		return "failed", nil, err
	}
	detail := map[string]any{"item_key": key, "path": req.Path}
	if !created {
		detail["note"] = "identical linked-file attachment already present"
		return "no_op", detail, nil
	}
	return "applied", detail, nil
}

func ensureLinkedAttachment(c *client.Client, req storedUploadRequest, flags *rootFlags) (string, bool, error) {
	if key, err := findLinkedAttachment(c, req.ParentKey, req.Path); err != nil {
		return "", false, err
	} else if key != "" {
		return key, false, nil
	}
	tokenHash := sha256.Sum256([]byte("zotio.attachments.add.linked\x00" + req.ParentKey + "\x00" + req.Path))
	token := hex.EncodeToString(tokenHash[:16])
	item := map[string]any{
		"itemType": "attachment", "linkMode": "linked_file", "parentItem": req.ParentKey,
		"title": req.Title, "path": req.Path, "contentType": req.ContentType,
	}
	resp, _, err := c.PostWithHeaders("/items", []map[string]any{item}, map[string]string{"Zotero-Write-Token": token})
	if err != nil {
		var respErr *client.APIError
		if errors.As(err, &respErr) && respErr.StatusCode == http.StatusPreconditionFailed {
			if key, reconcileErr := findLinkedAttachment(c, req.ParentKey, req.Path); reconcileErr == nil && key != "" {
				return key, false, nil
			}
			return "", false, &storedConflictError{fmt.Sprintf(
				"write token for linked file %q under %s was already submitted but no matching attachment was found; review the item manually",
				req.Path, req.ParentKey)}
		}
		return "", false, classifyAPIError(err, flags)
	}
	key, ok := createdItemKey(resp)
	if !ok {
		return "", false, fmt.Errorf("could not read created linked-file attachment key from /items response")
	}
	return key, true, nil
}

func findLinkedAttachment(c *client.Client, parentKey, path string) (string, error) {
	data, _, err := c.GetWithVersion("/items/"+parentKey+"/children", map[string]string{"itemType": "attachment"})
	if err != nil {
		var respErr *client.APIError
		if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
			return "", notFoundErr(fmt.Errorf("parent item %s not found", parentKey))
		}
		return "", fmt.Errorf("listing children of %s: %w", parentKey, err)
	}
	var rows []struct {
		Key  string `json:"key"`
		Data struct {
			ItemType string `json:"itemType"`
			LinkMode string `json:"linkMode"`
			Path     string `json:"path"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return "", fmt.Errorf("parsing children of %s: %w", parentKey, err)
	}
	for _, row := range rows {
		if row.Data.ItemType == "attachment" && row.Data.LinkMode == "linked_file" && row.Data.Path == path {
			return row.Key, nil
		}
	}
	return "", nil
}

// storedUploadRequest carries everything the upload protocol needs for one file.
type storedUploadRequest struct {
	ParentKey   string
	Path        string
	Filename    string
	Title       string
	ContentType string
	Size        int64
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
//
// The digest streams and only the content-type probe is retained. Holding the
// whole file meant a stored upload cost its size in resident memory from plan
// through apply, and the upload then copied it again; size and mtime are
// enough to describe the payload, and the bytes are re-read from disk at the
// moment they are sent.
func newStoredUploadRequest(parentKey, path, title string) (storedUploadRequest, error) {
	absPath, info, filename, title, err := attachmentFileDetails(path, title)
	if err != nil {
		return storedUploadRequest{}, err
	}
	md5hex, err := fileMD5(absPath)
	if err != nil {
		return storedUploadRequest{}, fmt.Errorf("reading attachment: %w", err)
	}
	probe, err := readAttachmentContentProbe(absPath)
	if err != nil {
		return storedUploadRequest{}, fmt.Errorf("reading attachment: %w", err)
	}
	// Deterministic write token: an identical retry replays the same token, so
	// Zotero rejects a duplicate create (412) instead of making a second child.
	tok := sha256.Sum256([]byte("zotio.attachments.add\x00" + parentKey + "\x00" + filename + "\x00" + md5hex))
	return storedUploadRequest{
		ParentKey:   parentKey,
		Path:        absPath,
		Filename:    filename,
		Title:       title,
		ContentType: storedAttachmentContentType(filename, probe),
		Size:        info.Size(),
		MD5:         md5hex,
		MtimeMS:     info.ModTime().UnixMilli(),
		WriteToken:  hex.EncodeToString(tok[:16]),
	}, nil
}

// fileMD5 streams the digest so hashing does not scale memory with file size.
// Zotero's upload authorization is keyed by MD5, so the algorithm is fixed by
// the protocol.
func fileMD5(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // G304: uploading a user-named local file is the command's purpose.
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	digest := md5.New() //nolint:gosec // G401: Zotero's upload authorization is keyed by MD5.
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// newLinkedFileRequest records only file metadata. Linked files are never
// uploaded, so do not read or retain their full contents.
func newLinkedFileRequest(parentKey, path, title string) (storedUploadRequest, error) {
	absPath, info, filename, title, err := attachmentFileDetails(path, title)
	if err != nil {
		return storedUploadRequest{}, err
	}
	probe, err := readAttachmentContentProbe(absPath)
	if err != nil {
		return storedUploadRequest{}, fmt.Errorf("reading attachment: %w", err)
	}
	return storedUploadRequest{
		ParentKey:   parentKey,
		Path:        absPath,
		Filename:    filename,
		Title:       title,
		ContentType: storedAttachmentContentType(filename, probe),
		MtimeMS:     info.ModTime().UnixMilli(),
	}, nil
}

func attachmentFileDetails(path, title string) (string, os.FileInfo, string, string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", nil, "", "", usageErr(fmt.Errorf("resolving attachment path: %w", err))
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return "", nil, "", "", usageErr(fmt.Errorf("attachment file: %w", err))
	}
	if !info.Mode().IsRegular() {
		return "", nil, "", "", usageErr(fmt.Errorf("attachment %s is not a regular file", absPath))
	}
	if info.Size() == 0 {
		return "", nil, "", "", usageErr(fmt.Errorf("attachment %s is empty", absPath))
	}
	filename := filepath.Base(absPath)
	if strings.TrimSpace(title) == "" {
		title = filename
	}
	return absPath, info, filename, title, nil
}

const attachmentContentProbeBytes = 512

func readAttachmentContentProbe(path string) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // G304: reading a user-named local file is the command's purpose.
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, attachmentContentProbeBytes))
}

func storedAttachmentContentType(filename string, probe []byte) string {
	if strings.EqualFold(filepath.Ext(filename), ".pdf") && bytes.HasPrefix(probe, []byte("%PDF-")) {
		return "application/pdf"
	}
	return http.DetectContentType(probe)
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
	// This is a server-side MD5 match, so no local bytes are sent or
	// registered on this idempotent path. A later local edit cannot alter the
	// already-associated server content.
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
	form.Set("filesize", strconv.FormatInt(req.Size, 10))
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
		// Zotero keyed this response by req.MD5 and has already associated those
		// bytes. No local body is sent or registration follows, so a later local
		// edit cannot affect the server-side association.
		return nil
	}
	if auth.URL == "" || auth.UploadKey == "" {
		return fmt.Errorf("upload authorization missing url/uploadKey")
	}
	if err := postUploadPayload(ctx, c, auth.URL, auth.ContentType, auth.Prefix, auth.Suffix, req); err != nil {
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
//
// The file is read from disk as it is sent, so an upload costs a buffer, not
// the file's size, and each attempt reopens rather than replaying a retained
// copy. That makes staleness detectable and worth detecting: the digest the
// server authorized was computed at plan time, so a file edited in between
// would upload bytes that do not match their own MD5. Fail instead.
func postUploadPayload(ctx context.Context, c *client.Client, uploadURL, contentType, prefix, suffix string, req storedUploadRequest) error {
	u, err := url.Parse(uploadURL)
	if err != nil || !uploadURLTrusted(u) {
		host := "<unparseable>"
		if u != nil {
			host = u.Scheme + "://" + u.Host
		}
		return fmt.Errorf("refusing file upload to untrusted storage host %s", host)
	}
	if err := assertUploadSourceUnchanged(req); err != nil {
		return err
	}
	envelope := func() (io.ReadCloser, error) {
		file, err := os.Open(req.Path) //nolint:gosec // G304: uploading a user-named local file is the command's purpose.
		if err != nil {
			return nil, fmt.Errorf("reopening attachment for upload: %w", err)
		}
		digest := md5.New() //nolint:gosec // G401: Zotero's upload authorization is keyed by MD5.
		limited := &io.LimitedReader{R: file, N: req.Size}
		return readerWithCloser{
			Reader: io.MultiReader(
				strings.NewReader(prefix),
				&verifiedUploadSource{
					Reader:     io.TeeReader(limited, digest),
					digest:     digest,
					expected:   req.MD5,
					filename:   req.Filename,
					limited:    limited,
					authorized: req.Size,
					trailing:   file,
				},
				strings.NewReader(suffix),
			),
			closer: file,
		}, nil
	}
	body, err := envelope()
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, body)
	if err != nil {
		return fmt.Errorf("building upload request: %w", err)
	}
	// net/http cannot size a MultiReader, and storage backends reject a chunked
	// upload here, so declare the length. GetBody reopens the file so a
	// redirect or retry replays from disk instead of a retained copy.
	httpReq.ContentLength = int64(len(prefix)) + req.Size + int64(len(suffix))
	httpReq.GetBody = envelope
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	// Re-resolve and vet the storage host at dial time so a signed HTTPS URL
	// cannot reach a private address through DNS rebinding.
	httpClient := externalFetchHTTPClient(c.HTTPClient, false)
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

// readerWithCloser pairs an upload envelope with its source file, so the
// descriptor is released whether the transport finishes, retries, or
// abandons the body.
type readerWithCloser struct {
	io.Reader
	closer io.Closer
}

func (r readerWithCloser) Close() error { return r.closer.Close() }

// verifiedUploadSource hashes exactly the authorized file bytes supplied for
// one file body. It withholds one byte until that extent is complete and still
// matches its authorized digest, retaining only constant memory.
type verifiedUploadSource struct {
	io.Reader
	digest     hash.Hash
	expected   string
	filename   string
	limited    *io.LimitedReader
	authorized int64
	trailing   io.Reader

	held      byte
	hasHeld   bool
	exhausted bool
}

func (r *verifiedUploadSource) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.exhausted {
		return r.release(p)
	}
	if !r.hasHeld {
		var next [1]byte
		n, err := r.Reader.Read(next[:])
		if n > 0 {
			r.held, r.hasHeld = next[0], true
		}
		if err == io.EOF {
			if err := r.verify(); err != nil {
				r.hasHeld = false
				return 0, err
			}
			return r.release(p)
		}
		if err != nil {
			r.hasHeld = false
			return 0, err
		}
		if n == 0 {
			return 0, nil
		}
	}

	if len(p) == 1 {
		var next [1]byte
		n, err := r.Reader.Read(next[:])
		if n == 0 {
			if err == io.EOF {
				if err := r.verify(); err != nil {
					r.hasHeld = false
					return 0, err
				}
				return r.release(p)
			}
			return 0, err
		}
		if err == io.EOF {
			if err := r.verify(); err != nil {
				r.hasHeld = false
				return 0, err
			}
			p[0], r.held, r.hasHeld = r.held, next[0], true
			return 1, nil
		}
		if err != nil {
			r.hasHeld = false
			return 0, err
		}
		p[0], r.held = r.held, next[0]
		return 1, nil
	}

	n, err := r.Reader.Read(p[1:])
	if n == 0 {
		if err == io.EOF {
			if err := r.verify(); err != nil {
				r.hasHeld = false
				return 0, err
			}
			return r.release(p)
		}
		return 0, err
	}
	if err == io.EOF {
		if err := r.verify(); err != nil {
			r.hasHeld = false
			return 0, err
		}
		p[0] = r.held
		r.hasHeld = false
		return n + 1, nil
	}
	if err != nil {
		r.hasHeld = false
		return 0, err
	}
	p[0], r.held = r.held, p[n]
	return n, nil
}

func (r *verifiedUploadSource) verify() error {
	r.exhausted = true
	if r.limited != nil && r.limited.N != 0 {
		return fmt.Errorf("attachment %s shrank after it was hashed: expected %d bytes but reached EOF after %d; rerun to upload the current file",
			r.filename, r.authorized, r.authorized-r.limited.N)
	}
	if r.trailing != nil {
		var extra [1]byte
		n, err := r.trailing.Read(extra[:])
		if n > 0 {
			return fmt.Errorf("attachment %s grew after it was hashed: expected %d bytes; rerun to upload the current file", r.filename, r.authorized)
		}
		if err != nil && err != io.EOF {
			return fmt.Errorf("checking attachment %s after upload: %w", r.filename, err)
		}
	}
	got := hex.EncodeToString(r.digest.Sum(nil))
	if got != r.expected {
		return fmt.Errorf("attachment %s changed after it was hashed: uploaded bytes MD5 %s does not match authorized MD5 %s; rerun to upload the current file", r.filename, got, r.expected)
	}
	return nil
}

func (r *verifiedUploadSource) release(p []byte) (int, error) {
	if !r.hasHeld {
		return 0, io.EOF
	}
	p[0] = r.held
	r.hasHeld = false
	return 1, nil
}

// assertUploadSourceUnchanged refuses to send a file that moved under the plan.
// The MD5 and size in req were measured before authorization; uploading
// different bytes under that digest would register a file the server believes
// it verified.
func assertUploadSourceUnchanged(req storedUploadRequest) error {
	info, err := os.Stat(req.Path)
	if err != nil {
		return fmt.Errorf("re-checking attachment before upload: %w", err)
	}
	if info.Size() != req.Size || info.ModTime().UnixMilli() != req.MtimeMS {
		return fmt.Errorf("attachment %s changed after it was hashed (size %d->%d, mtime %d->%d); rerun to upload the current file",
			req.Filename, req.Size, info.Size(), req.MtimeMS, info.ModTime().UnixMilli())
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
