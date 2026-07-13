// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Fake Zotero Web API file-upload server covering the stored-attachment
// protocol: authorization, {exists:1}, full upload/register, write-token
// replay (412), quota (413), rate limit (429), and idempotent retries.

package cli

import (
	"bytes"
	"crypto/md5" //nolint:gosec // G501: mirrors the Zotero upload protocol's MD5 addressing.
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"zotio/internal/mutation"
)

type fakeUploadChild struct {
	Key      string
	Filename string
	MD5      string
}

// fakeZoteroUpload implements just enough of the Zotero Web API for the
// stored-upload protocol, with counters for exactly-once assertions.
type fakeZoteroUpload struct {
	t  *testing.T
	mu sync.Mutex

	parentKey string
	children  []fakeUploadChild
	nextKey   int

	// behavior switches
	quotaOnAuth   bool // 413 on upload authorization
	rateLimitOnce bool // one 429 before a successful authorization
	replayCreate  bool // 412 on POST /items (write token already submitted)
	existsOnAuth  bool // respond {exists:1} instead of an upload target

	// observed traffic
	creates      int
	createTokens []string
	authForms    []map[string]string
	uploads      int
	registers    int

	authorizedMD5 map[string]string // attachment key -> md5 pending registration
	srv           *httptest.Server
}

func newFakeZoteroUpload(t *testing.T, parentKey string) *fakeZoteroUpload {
	t.Helper()
	f := &fakeZoteroUpload{t: t, parentKey: parentKey, authorizedMD5: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeZoteroUpload) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := strings.TrimPrefix(r.URL.Path, "/users/0")
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/children"):
		parent := strings.TrimSuffix(strings.TrimPrefix(path, "/items/"), "/children")
		if parent != f.parentKey {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		rows := make([]map[string]any, 0, len(f.children))
		for _, c := range f.children {
			var md5Val any
			if c.MD5 != "" {
				md5Val = c.MD5
			}
			rows = append(rows, map[string]any{
				"key": c.Key,
				"data": map[string]any{
					"itemType": "attachment",
					"linkMode": "imported_file",
					"filename": c.Filename,
					"md5":      md5Val,
				},
			})
		}
		_ = json.NewEncoder(w).Encode(rows)

	case r.Method == http.MethodPost && path == "/items":
		token := r.Header.Get("Zotero-Write-Token")
		if f.replayCreate {
			http.Error(w, `{"error":"write token already submitted"}`, http.StatusPreconditionFailed)
			return
		}
		var items []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&items); err != nil || len(items) != 1 {
			http.Error(w, `{"error":"bad body"}`, http.StatusBadRequest)
			return
		}
		item := items[0]
		if item["itemType"] != "attachment" || item["linkMode"] != "imported_file" || item["parentItem"] != f.parentKey {
			http.Error(w, `{"error":"unexpected item"}`, http.StatusBadRequest)
			return
		}
		f.creates++
		f.createTokens = append(f.createTokens, token)
		f.nextKey++
		key := fmt.Sprintf("ATT%d", f.nextKey)
		filename, _ := item["filename"].(string)
		f.children = append(f.children, fakeUploadChild{Key: key, Filename: filename})
		_, _ = fmt.Fprintf(w, `{"success":{"0":%q}}`, key)

	case r.Method == http.MethodPost && strings.HasPrefix(path, "/items/") && strings.HasSuffix(path, "/file"):
		key := strings.TrimSuffix(strings.TrimPrefix(path, "/items/"), "/file")
		child := f.findChild(key)
		if child == nil {
			http.Error(w, `{"error":"no such attachment"}`, http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, `{"error":"bad form"}`, http.StatusBadRequest)
			return
		}
		if r.Header.Get("If-None-Match") != "*" {
			http.Error(w, `{"error":"precondition required"}`, http.StatusPreconditionRequired)
			return
		}
		if upload := r.PostForm.Get("upload"); upload != "" {
			// registration
			pending, ok := f.authorizedMD5[key]
			if !ok || upload != "UK-"+key {
				http.Error(w, `{"error":"unknown upload key"}`, http.StatusBadRequest)
				return
			}
			child.MD5 = pending
			delete(f.authorizedMD5, key)
			f.registers++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// authorization
		if child.MD5 != "" {
			// If-None-Match: * against an attachment that already has a file.
			http.Error(w, `{"error":"file exists"}`, http.StatusPreconditionFailed)
			return
		}
		form := map[string]string{}
		for k := range r.PostForm {
			form[k] = r.PostForm.Get(k)
		}
		f.authForms = append(f.authForms, form)
		if f.rateLimitOnce {
			f.rateLimitOnce = false
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"error":"slow down"}`, http.StatusTooManyRequests)
			return
		}
		if f.quotaOnAuth {
			http.Error(w, `{"error":"quota exceeded"}`, http.StatusRequestEntityTooLarge)
			return
		}
		if f.existsOnAuth {
			child.MD5 = r.PostForm.Get("md5")
			_, _ = w.Write([]byte(`{"exists":1}`))
			return
		}
		f.authorizedMD5[key] = r.PostForm.Get("md5")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"url":         f.srv.URL + "/storage/" + key,
			"contentType": "application/octet-stream",
			"prefix":      "PRE:",
			"suffix":      ":SUF",
			"uploadKey":   "UK-" + key,
		})

	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/storage/"):
		key := strings.TrimPrefix(r.URL.Path, "/storage/")
		if _, ok := f.authorizedMD5[key]; !ok {
			http.Error(w, "upload not authorized", http.StatusForbidden)
			return
		}
		if got := r.Header.Get("Content-Type"); got != "application/octet-stream" {
			f.t.Errorf("storage upload Content-Type = %q, want application/octet-stream", got)
		}
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)
		payload := body.Bytes()
		if !bytes.HasPrefix(payload, []byte("PRE:")) || !bytes.HasSuffix(payload, []byte(":SUF")) {
			f.t.Errorf("storage payload missing prefix/suffix: %q", payload)
		}
		f.uploads++
		w.WriteHeader(http.StatusCreated)

	default:
		http.Error(w, fmt.Sprintf(`{"error":"unexpected %s %s"}`, r.Method, r.URL.Path), http.StatusInternalServerError)
	}
}

func (f *fakeZoteroUpload) findChild(key string) *fakeUploadChild {
	for i := range f.children {
		if f.children[i].Key == key {
			return &f.children[i]
		}
	}
	return nil
}

func (f *fakeZoteroUpload) snapshot() (creates, uploads, registers int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, f.uploads, f.registers
}

func writeUploadFixture(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func setUploadTestEnv(t *testing.T, f *fakeZoteroUpload) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", f.srv.URL+"/users/0")
	t.Setenv("ZOTERO_API_KEY", "")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
}

func runAttachmentsAdd(t *testing.T, flags *rootFlags, args []string) (mutation.Envelope, string, error) {
	t.Helper()
	cmd := newAttachmentsCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	var env mutation.Envelope
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode mutation envelope %q: %v", out.String(), decodeErr)
		}
	}
	return env, errOut.String(), err
}

func applyFlags() *rootFlags {
	return &rootFlags{asJSON: true, yes: true, maxChanges: -1}
}

const uploadFixturePDF = "%PDF-1.4\nstored upload fixture body\n%%EOF\n"

func TestAttachmentsAddPreviewPlansWithoutDialing(t *testing.T) {
	f := newFakeZoteroUpload(t, "PARENT1")
	setUploadTestEnv(t, f)
	path := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))

	env, stderr, err := runAttachmentsAdd(t, &rootFlags{asJSON: true}, []string{"add", "PARENT1", path})
	if err != nil {
		t.Fatalf("preview: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "preview" || env.Result != nil || len(env.Plan.Operations) != 1 {
		t.Fatalf("env = %+v, want one previewed op", env)
	}
	got := fmt.Sprint(env.Plan.Operations[0].Changes[0].Add)
	if !strings.Contains(got, "stored -> paper.pdf") || !strings.Contains(got, "md5 ") {
		t.Fatalf("plan change = %q, want stored filename with md5 evidence", got)
	}
	creates, uploads, registers := f.snapshot()
	if creates+uploads+registers != 0 {
		t.Fatalf("preview dialed the server: creates=%d uploads=%d registers=%d", creates, uploads, registers)
	}
}

func TestAttachmentsAddStoredUploadsExactlyOnceAndRetryNoOps(t *testing.T) {
	f := newFakeZoteroUpload(t, "PARENT1")
	setUploadTestEnv(t, f)
	pdf := []byte(uploadFixturePDF)
	path := writeUploadFixture(t, "paper.pdf", pdf)

	env, stderr, err := runAttachmentsAdd(t, applyFlags(), []string{"add", "PARENT1", path})
	if err != nil {
		t.Fatalf("apply: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("env = %+v, want one applied upload", env)
	}
	item := env.Result.Items[0]
	reason, _ := item.Reason.(map[string]any)
	if item.Status != "applied" || reason["item_key"] != "ATT1" || reason["upload"] != "uploaded" {
		t.Fatalf("result item = %+v, want applied ATT1 uploaded", item)
	}
	creates, uploads, registers := f.snapshot()
	if creates != 1 || uploads != 1 || registers != 1 {
		t.Fatalf("traffic = creates:%d uploads:%d registers:%d, want 1/1/1", creates, uploads, registers)
	}
	if len(f.createTokens) != 1 || len(f.createTokens[0]) != 32 {
		t.Fatalf("write token = %v, want one 32-char token", f.createTokens)
	}
	sum := md5.Sum(pdf) //nolint:gosec // G401: asserting the protocol's md5 form field.
	if got := f.authForms[0]["md5"]; got != hex.EncodeToString(sum[:]) {
		t.Fatalf("authorized md5 = %q, want fixture md5", got)
	}
	if f.authForms[0]["filesize"] != fmt.Sprint(len(pdf)) || f.authForms[0]["filename"] != "paper.pdf" || f.authForms[0]["mtime"] == "" {
		t.Fatalf("auth form = %+v, want filename/filesize/mtime", f.authForms[0])
	}

	// Identical retry must reconcile to a no-op: no new child, no new upload.
	env2, stderr2, err2 := runAttachmentsAdd(t, applyFlags(), []string{"add", "PARENT1", path})
	if err2 != nil {
		t.Fatalf("retry: %v; stderr=%s", err2, stderr2)
	}
	if !env2.OK || env2.Result == nil || env2.Result.Summary.NoOp != 1 || env2.Result.Summary.Applied != 0 {
		t.Fatalf("retry env = %+v, want one no_op", env2)
	}
	retryReason, _ := env2.Result.Items[0].Reason.(map[string]any)
	if env2.Result.Items[0].Status != "no_op" || retryReason["item_key"] != "ATT1" {
		t.Fatalf("retry item = %+v, want no_op reusing ATT1", env2.Result.Items[0])
	}
	creates, uploads, registers = f.snapshot()
	if creates != 1 || uploads != 1 || registers != 1 {
		t.Fatalf("retry traffic = creates:%d uploads:%d registers:%d, want unchanged 1/1/1", creates, uploads, registers)
	}
}

func TestAttachmentsAddResumesPendingChildWithoutDuplicateCreate(t *testing.T) {
	f := newFakeZoteroUpload(t, "PARENT1")
	// A crashed earlier run created the child but never registered a file.
	f.children = append(f.children, fakeUploadChild{Key: "ATT9", Filename: "paper.pdf"})
	setUploadTestEnv(t, f)
	path := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))

	env, stderr, err := runAttachmentsAdd(t, applyFlags(), []string{"add", "PARENT1", path})
	if err != nil {
		t.Fatalf("resume: %v; stderr=%s", err, stderr)
	}
	item := env.Result.Items[0]
	reason, _ := item.Reason.(map[string]any)
	if item.Status != "applied" || reason["item_key"] != "ATT9" || reason["upload"] != "resumed" {
		t.Fatalf("result item = %+v, want resumed ATT9", item)
	}
	creates, uploads, registers := f.snapshot()
	if creates != 0 || uploads != 1 || registers != 1 {
		t.Fatalf("traffic = creates:%d uploads:%d registers:%d, want 0/1/1", creates, uploads, registers)
	}
}

func TestAttachmentsAddExistsShortCircuitsPayloadUpload(t *testing.T) {
	f := newFakeZoteroUpload(t, "PARENT1")
	f.existsOnAuth = true
	setUploadTestEnv(t, f)
	path := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))

	env, stderr, err := runAttachmentsAdd(t, applyFlags(), []string{"add", "PARENT1", path})
	if err != nil {
		t.Fatalf("exists apply: %v; stderr=%s", err, stderr)
	}
	if env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("env = %+v, want applied via exists short-circuit", env)
	}
	creates, uploads, registers := f.snapshot()
	if creates != 1 || uploads != 0 || registers != 0 {
		t.Fatalf("traffic = creates:%d uploads:%d registers:%d, want 1/0/0 (no payload after exists:1)", creates, uploads, registers)
	}
}

func TestAttachmentsAddConflictOnDifferentContentSameFilename(t *testing.T) {
	f := newFakeZoteroUpload(t, "PARENT1")
	f.children = append(f.children, fakeUploadChild{Key: "USER1", Filename: "paper.pdf", MD5: "feedfacefeedfacefeedfacefeedface"})
	setUploadTestEnv(t, f)
	path := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))

	env, _, err := runAttachmentsAdd(t, applyFlags(), []string{"add", "PARENT1", path})
	if err == nil || !strings.Contains(err.Error(), "mutation incomplete") {
		t.Fatalf("err = %v, want mutation incomplete from conflict", err)
	}
	item := env.Result.Items[0]
	if item.Status != "conflict" || !strings.Contains(fmt.Sprint(item.Reason), "different content") || !strings.Contains(fmt.Sprint(item.Reason), "USER1") {
		t.Fatalf("result item = %+v, want conflict naming USER1", item)
	}
	creates, uploads, registers := f.snapshot()
	if creates+uploads+registers != 0 {
		t.Fatalf("conflict still wrote: creates=%d uploads=%d registers=%d", creates, uploads, registers)
	}
}

func TestAttachmentsAddQuotaFailureIsActionable(t *testing.T) {
	f := newFakeZoteroUpload(t, "PARENT1")
	f.quotaOnAuth = true
	setUploadTestEnv(t, f)
	path := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))

	env, _, err := runAttachmentsAdd(t, applyFlags(), []string{"add", "PARENT1", path})
	if err == nil {
		t.Fatalf("want quota failure, got success: %+v", env)
	}
	item := env.Result.Items[0]
	if item.Status != "failed" || !strings.Contains(fmt.Sprint(item.Reason), "storage quota") {
		t.Fatalf("result item = %+v, want failed with storage-quota message", item)
	}
	// The child exists but carries no file; the message must point at resumption.
	if !strings.Contains(fmt.Sprint(item.Reason), "retry resumes") {
		t.Fatalf("reason %q missing resume guidance", fmt.Sprint(item.Reason))
	}
}

func TestAttachmentsAddRateLimitedAuthorizationRetries(t *testing.T) {
	f := newFakeZoteroUpload(t, "PARENT1")
	f.rateLimitOnce = true
	setUploadTestEnv(t, f)
	path := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))

	env, stderr, err := runAttachmentsAdd(t, applyFlags(), []string{"add", "PARENT1", path})
	if err != nil {
		t.Fatalf("rate-limited apply: %v; stderr=%s", err, stderr)
	}
	if env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("env = %+v, want applied after 429 retry", env)
	}
	creates, uploads, registers := f.snapshot()
	if creates != 1 || uploads != 1 || registers != 1 {
		t.Fatalf("traffic = creates:%d uploads:%d registers:%d, want 1/1/1", creates, uploads, registers)
	}
}

func TestAttachmentsAddWriteTokenReplayReconcilesPendingChild(t *testing.T) {
	f := newFakeZoteroUpload(t, "PARENT1")
	setUploadTestEnv(t, f)
	path := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))

	// First create succeeded server-side but the response was lost; the child
	// exists AND the write token is burned. The retry must reconcile, not 412-fail.
	f.mu.Lock()
	f.children = append(f.children, fakeUploadChild{Key: "ATT7", Filename: "other.pdf"})
	f.replayCreate = true
	f.mu.Unlock()

	// other.pdf does not match paper.pdf, so reconcile finds nothing, create
	// hits the replayed token, and the second reconcile still finds nothing:
	// this must surface as a conflict for review, never a duplicate create.
	env, _, err := runAttachmentsAdd(t, applyFlags(), []string{"add", "PARENT1", path})
	if err == nil {
		t.Fatalf("want conflict from burned write token, got %+v", env)
	}
	item := env.Result.Items[0]
	if item.Status != "conflict" || !strings.Contains(fmt.Sprint(item.Reason), "already submitted") {
		t.Fatalf("result item = %+v, want write-token conflict", item)
	}

	// With a matching pending child present, the same replay resolves to it.
	f.mu.Lock()
	f.children = append(f.children, fakeUploadChild{Key: "ATT8", Filename: "paper.pdf"})
	f.mu.Unlock()
	env2, stderr2, err2 := runAttachmentsAdd(t, applyFlags(), []string{"add", "PARENT1", path})
	if err2 != nil {
		t.Fatalf("replay resume: %v; stderr=%s", err2, stderr2)
	}
	item2 := env2.Result.Items[0]
	reason2, _ := item2.Reason.(map[string]any)
	if item2.Status != "applied" || reason2["item_key"] != "ATT8" {
		t.Fatalf("result item = %+v, want resumed ATT8", item2)
	}
	creates, _, _ := f.snapshot()
	if creates != 0 {
		t.Fatalf("creates = %d, want 0 (replay must never create)", creates)
	}
}

func TestAttachmentsAddMissingParentFailsWithNotFound(t *testing.T) {
	f := newFakeZoteroUpload(t, "PARENT1")
	setUploadTestEnv(t, f)
	path := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))

	env, _, err := runAttachmentsAdd(t, applyFlags(), []string{"add", "NOPE1234", path})
	if err == nil {
		t.Fatalf("want missing-parent failure, got %+v", env)
	}
	item := env.Result.Items[0]
	if item.Status != "failed" || !strings.Contains(fmt.Sprint(item.Reason), "parent item NOPE1234 not found") {
		t.Fatalf("result item = %+v, want parent-not-found failure", item)
	}
}

func TestAttachmentsAddUsageValidation(t *testing.T) {
	f := newFakeZoteroUpload(t, "PARENT1")
	setUploadTestEnv(t, f)
	path := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))

	if _, _, err := runAttachmentsAdd(t, &rootFlags{asJSON: true}, []string{"add", "PARENT1", path, "--mode", "linked-file"}); err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "--mode must be stored") {
		t.Fatalf("mode err = %v, want stored-only usage error", err)
	}
	if _, _, err := runAttachmentsAdd(t, &rootFlags{asJSON: true}, []string{"add", "../etc", path}); err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "invalid parent item key") {
		t.Fatalf("key err = %v, want invalid-key usage error", err)
	}
	if _, _, err := runAttachmentsAdd(t, &rootFlags{asJSON: true}, []string{"add", "PARENT1", filepath.Join(t.TempDir(), "missing.pdf")}); err == nil || ExitCode(err) != 2 {
		t.Fatalf("missing file err = %v, want usage error", err)
	}
	empty := writeUploadFixture(t, "empty.pdf", nil)
	if _, _, err := runAttachmentsAdd(t, &rootFlags{asJSON: true}, []string{"add", "PARENT1", empty}); err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("empty file err = %v, want usage error", err)
	}
}

// The import-apply stored-attach route shares the uploader end to end.
func TestImportApplyStoredAttachUploadsAndRetryNoOps(t *testing.T) {
	f := newFakeZoteroUpload(t, "MATCH1")
	setUploadTestEnv(t, f)
	pdfPath := writeUploadFixture(t, "attach.pdf", []byte(uploadFixturePDF))

	manifest := importApplyTestManifest()
	manifest.Entries = []importManifestEntry{{
		Path:       pdfPath,
		Action:     "attach",
		Status:     "resolved",
		MatchedKey: "MATCH1",
		Title:      "Attach Paper",
	}}
	manifestPath := writeImportApplyTestManifest(t, manifest)

	env, stderr, err := runImportApplyTestCmdWithFlags(t, applyFlags(), []string{"--attach-mode", "stored", manifestPath})
	if err != nil {
		t.Fatalf("stored attach apply: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("env = %+v, want one applied stored attach", env)
	}
	reason, _ := env.Result.Items[0].Reason.(map[string]any)
	if reason["item_key"] != "ATT1" {
		t.Fatalf("result = %+v, want uploaded child ATT1", env.Result.Items[0])
	}
	creates, uploads, registers := f.snapshot()
	if creates != 1 || uploads != 1 || registers != 1 {
		t.Fatalf("traffic = creates:%d uploads:%d registers:%d, want 1/1/1", creates, uploads, registers)
	}

	env2, stderr2, err2 := runImportApplyTestCmdWithFlags(t, applyFlags(), []string{"--attach-mode", "stored", manifestPath})
	if err2 != nil {
		t.Fatalf("stored attach retry: %v; stderr=%s", err2, stderr2)
	}
	if env2.Result == nil || env2.Result.Summary.NoOp != 1 || env2.Result.Summary.Applied != 0 {
		t.Fatalf("retry env = %+v, want one no_op", env2)
	}
	creates, uploads, registers = f.snapshot()
	if creates != 1 || uploads != 1 || registers != 1 {
		t.Fatalf("retry traffic = creates:%d uploads:%d registers:%d, want unchanged", creates, uploads, registers)
	}
}
