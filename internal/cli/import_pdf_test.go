// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// import pdf: connector preflight, --on-duplicate (skip/attach/create), and
// --collection filing. Covers field-report-2026-08-08 finding 7, finding 8, and
// field-report-2026-08-08-library-hygiene finding 7.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/connector"
	"zotio/internal/mutation"
	"zotio/internal/store"
)

// fakeImportPDFAPI serves both the desktop Connector API (/connector/*) and the
// read/write API (/api/users/0/*) an `import pdf` run touches: standalone
// attachment save + recognition, /items/top key resolution, item children,
// collection lookup/create, and item collection PATCH. One server plays both
// roles because a directly-constructed *connector.Client bypasses the
// port-23119 restriction flags.newConnector() enforces, so tests never depend
// on port 23119 (frequently occupied by a real, running Zotero desktop).
type fakeImportPDFAPI struct {
	t  *testing.T
	mu sync.Mutex

	recognizedTitle string
	recognizedType  string
	topItems        []map[string]any
	children        map[string][]map[string]any

	collections     map[string]string // name -> key
	nextCollection  int
	itemCollections map[string][]string
	itemVersion     map[string]int

	saveStandaloneCalls int
	patchedItems        []string

	srv *httptest.Server
}

func newFakeImportPDFAPI(t *testing.T) *fakeImportPDFAPI {
	t.Helper()
	f := &fakeImportPDFAPI{
		t:               t,
		children:        map[string][]map[string]any{},
		collections:     map[string]string{},
		itemCollections: map[string][]string{},
		itemVersion:     map[string]int{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeImportPDFAPI) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/connector/saveStandaloneAttachment":
		f.saveStandaloneCalls++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"canRecognize":true}`))
	case r.Method == http.MethodPost && r.URL.Path == "/connector/getRecognizedItem":
		_ = json.NewEncoder(w).Encode(map[string]string{"title": f.recognizedTitle, "itemType": f.recognizedType})
	case r.Method == http.MethodGet && r.URL.Path == "/api/users/0/items/top":
		_ = json.NewEncoder(w).Encode(f.topItems)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/children"):
		key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/users/0/items/"), "/children")
		_ = json.NewEncoder(w).Encode(f.children[key])
	case r.Method == http.MethodGet && r.URL.Path == "/api/users/0/collections":
		rows := make([]map[string]any, 0, len(f.collections))
		for name, key := range f.collections {
			rows = append(rows, map[string]any{"key": key, "data": map[string]any{"name": name}})
		}
		_ = json.NewEncoder(w).Encode(rows)
	case r.Method == http.MethodPost && r.URL.Path == "/api/users/0/collections":
		var items []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&items); err != nil || len(items) != 1 {
			http.Error(w, `{"error":"bad body"}`, http.StatusBadRequest)
			return
		}
		name, _ := items[0]["name"].(string)
		f.nextCollection++
		key := fmt.Sprintf("COL%d", f.nextCollection)
		f.collections[name] = key
		_, _ = fmt.Fprintf(w, `{"success":{"0":%q}}`, key)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/users/0/items/"):
		key := strings.TrimPrefix(r.URL.Path, "/api/users/0/items/")
		version := f.itemVersion[key]
		w.Header().Set("Last-Modified-Version", strconv.Itoa(version))
		body, _ := json.Marshal(map[string]any{"key": key, "version": version, "data": map[string]any{"key": key, "collections": f.itemCollections[key]}})
		_, _ = w.Write(body)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/api/users/0/items/"):
		key := strings.TrimPrefix(r.URL.Path, "/api/users/0/items/")
		var body struct {
			Collections []string `json:"collections"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"bad body"}`, http.StatusBadRequest)
			return
		}
		if r.Header.Get("If-Unmodified-Since-Version") == "" {
			http.Error(w, `{"error":"missing precondition"}`, http.StatusPreconditionRequired)
			return
		}
		f.itemCollections[key] = body.Collections
		f.itemVersion[key]++
		f.patchedItems = append(f.patchedItems, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, fmt.Sprintf(`{"error":"unexpected %s %s"}`, r.Method, r.URL.Path), http.StatusInternalServerError)
	}
}

// conn returns a Connector client rooted at the fake server, bypassing
// flags.newConnector()'s port-23119 requirement entirely.
func (f *fakeImportPDFAPI) conn() *connector.Client {
	return connector.New(f.srv.URL+"/connector", 5*time.Second)
}

// flags returns rootFlags pointed at the fake server for reads and writes.
func (f *fakeImportPDFAPI) flags(t *testing.T) *rootFlags {
	return &rootFlags{asJSON: true, maxChanges: -1, configPath: testConfigFile(t, f.srv.URL+"/api/users/0")}
}

func writeImportPDFFixture(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("%PDF-1.4\nfixture\n%%EOF\n"), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", name, err)
	}
	return path
}

// testApplyCmd gives Apply closures a *cobra.Command with a live Context()
// without going through cobra's Execute/RunE wiring.
func testApplyCmd() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	return cmd
}

// --- --on-duplicate validation --------------------------------------------

func TestImportPDFRejectsInvalidOnDuplicateValue(t *testing.T) {
	flags := &rootFlags{asJSON: true, configPath: testConfigFile(t, "http://localhost:23119/api/users/0")}
	cmd := newImportPDFCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--on-duplicate", "bogus", "whatever.pdf"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "skip, attach, create") {
		t.Fatalf("err = %v, want --on-duplicate validation error", err)
	}
}

// --- finding 8: preflight must fail the plan when the connector is unreachable

func TestImportPDFPreflightFailsPlanWhenConnectorUnreachable(t *testing.T) {
	root, _, out, _ := newPreflightTestRoot(t)
	// isolateDemoEnv (inside newPreflightTestRoot) stubs ZOTERO_BASE_URL to a
	// non-local port so unrelated preflight checks never dial anything; point
	// it back at the standard local port so the connector precondition itself
	// is what's under test here.
	t.Setenv("ZOTERO_BASE_URL", "http://localhost:23119/api/users/0")
	oldPing := connectorPing
	t.Cleanup(func() { connectorPing = oldPing })
	connectorPing = func(context.Context, *connector.Client) error {
		return fmt.Errorf("dial tcp 127.0.0.1:23119: connect: connection refused")
	}

	dir := t.TempDir()
	path := writeImportPDFFixture(t, dir, "paper.pdf")
	root.SetArgs([]string{"--json", "--dry-run", "import", "pdf", path})
	err := root.Execute()
	if err == nil {
		t.Fatal("import pdf --dry-run with an unreachable connector succeeded, want a precondition error")
	}
	assertPreconditionExitCode(t, err, 9)

	var env preconditionUnmetEnvelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode precondition envelope: %v; output=%q", err, out.String())
	}
	if env.Capability != "import pdf" || env.Precondition != preconditionDesktopConnector {
		t.Fatalf("envelope = %+v, want import pdf / desktop_connector", env)
	}
	if !strings.Contains(env.Detail, "import doi") {
		t.Fatalf("detail = %q, want it to name 'import doi' as the no-desktop alternative", env.Detail)
	}
	// No plan at all leaked out: the preview never got the chance to look clean.
	if bytes.Contains(out.Bytes(), []byte(`"operations"`)) {
		t.Fatalf("output = %s, want no plan when the connector precondition failed", out.String())
	}
}

// --- report #2 finding 7: --on-duplicate skip/create ------------------------

// seedImportPDFDuplicateStore writes a synced-store item that already carries
// doi and a live PDF attachment, matching import scan's "duplicate" classification.
func seedImportPDFDuplicateStore(t *testing.T, doi, existingKey string) {
	t.Helper()
	isolateDemoEnv(t, "0")
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	items := []json.RawMessage{
		json.RawMessage(fmt.Sprintf(`{"key":%q,"data":{"key":%q,"itemType":"journalArticle","title":"Trust and Leadership","DOI":%q}}`, existingKey, existingKey, doi)),
		json.RawMessage(fmt.Sprintf(`{"key":"ATTEXIST","data":{"key":"ATTEXIST","itemType":"attachment","parentItem":%q,"contentType":"application/pdf"}}`, existingKey)),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed duplicate store: %v", err)
	}
}

// doiFilename slash-encodes a DOI into a filename the way extractPDFDOI decodes
// it back (filenames cannot contain '/').
func doiFilename(doi string) string {
	return strings.ReplaceAll(doi, "/", "%2F") + ".pdf"
}

func TestImportPDFOnDuplicateSkipDoesNotCreateSecondItemAndSaysWhy(t *testing.T) {
	const doi = "10.1037/0021-9010.87.4.611"
	const existingKey = "PHMIJWH3"
	seedImportPDFDuplicateStore(t, doi, existingKey)
	// isolateDemoEnv stubs ZOTERO_BASE_URL to an unused address; import pdf's
	// own connector never gets dialed on the skip path, but resolveCreateVia and
	// flags.newConnector() still need a syntactically local base URL.
	t.Setenv("ZOTERO_BASE_URL", "")

	oldPing := connectorPing
	t.Cleanup(func() { connectorPing = oldPing })
	connectorPing = func(context.Context, *connector.Client) error { return nil }

	dir := t.TempDir()
	path := writeImportPDFFixture(t, dir, doiFilename(doi))

	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1, configPath: testConfigFile(t, "http://localhost:23119/api/users/0")}
	cmd := newImportPDFCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{path})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import pdf --on-duplicate skip (default): %v; out=%s", err, out.String())
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v; out=%s", err, out.String())
	}
	if env.Plan.Operations[0].Kind != "import_pdf_duplicate_skip" {
		t.Fatalf("kind = %q, want import_pdf_duplicate_skip", env.Plan.Operations[0].Kind)
	}
	if !env.OK || env.Result == nil || env.Result.Summary.Skipped != 1 || env.Result.Summary.Applied != 0 {
		t.Fatalf("env = %+v, want one skipped op and zero applied", env)
	}
	reason, ok := env.Result.Items[0].Reason.(map[string]any)
	if !ok {
		t.Fatalf("reason = %#v, want a decoded importPDFResult object", env.Result.Items[0].Reason)
	}
	if reason["duplicate_of"] != existingKey {
		t.Fatalf("duplicate_of = %v, want %s", reason["duplicate_of"], existingKey)
	}
	note, _ := reason["duplicate_note"].(string)
	if !strings.Contains(note, existingKey) || !strings.Contains(note, "already has a PDF") {
		t.Fatalf("duplicate_note = %q, want it to name the existing item and explain why", note)
	}
}

func TestImportPDFOnDuplicateCreatePreservesTodaysBehavior(t *testing.T) {
	f := newFakeImportPDFAPI(t)
	f.recognizedTitle = "Trust and Leadership: Meta-Analytic Findings"
	f.recognizedType = "journalArticle"
	added := time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339)
	f.topItems = []map[string]any{{
		"key": "NEW1",
		"data": map[string]any{
			"key": "NEW1", "itemType": "journalArticle", "title": f.recognizedTitle,
			"dateAdded": added, "DOI": "10.1037/0021-9010.87.4.611",
		},
	}}
	f.children["NEW1"] = []map[string]any{{
		"key":  "ATTNEW",
		"data": map[string]any{"key": "ATTNEW", "itemType": "attachment", "contentType": "application/pdf"},
	}}

	dir := t.TempDir()
	path := writeImportPDFFixture(t, dir, "trust.pdf")
	cmd := testApplyCmd()

	opts := importPDFOptions{
		OnDuplicate: "create",
		Duplicate:   scanResult{Status: "duplicate", ItemKey: "PHMIJWH3", DOI: "10.1037/0021-9010.87.4.611"},
	}
	op := importPDFOpWithOptions(cmd, f.flags(t), f.conn(), path, filepath.Base(path), 1, opts)
	if op.Kind != "import_pdf" {
		t.Fatalf("kind = %q, want import_pdf (create overrides duplicate handling)", op.Kind)
	}
	status, reason, err := op.Apply()
	if err != nil {
		t.Fatalf("apply --on-duplicate create: %v; reason=%#v", err, reason)
	}
	if status != "applied" {
		t.Fatalf("status = %q, want applied", status)
	}
	result, ok := reason.(importPDFResult)
	if !ok {
		t.Fatalf("reason = %#v, want importPDFResult", reason)
	}
	if result.Status != "recognized" || result.ItemKey != "NEW1" || result.AttachmentKey != "ATTNEW" {
		t.Fatalf("result = %+v, want a normal recognized create despite the duplicate match", result)
	}
	if result.DOI != "10.1037/0021-9010.87.4.611" || result.DuplicateOf != "PHMIJWH3" || !strings.Contains(result.DuplicateNote, "created a new item") {
		t.Fatalf("result = %+v, want create payload to record the matched DOI and existing key", result)
	}
	if f.saveStandaloneCalls != 1 {
		t.Fatalf("saveStandaloneAttachment calls = %d, want 1 (create must still hit the connector)", f.saveStandaloneCalls)
	}
}

func TestImportPDFOnAttachCandidateUsesExistingItem(t *testing.T) {
	const existingKey = "PHMIJWH3"
	fu := newFakeZoteroUpload(t, existingKey)
	setUploadTestEnv(t, fu)

	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "candidate.pdf")
	if err := os.WriteFile(pdfPath, []byte(uploadFixturePDF), 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	cmd := testApplyCmd()
	opts := importPDFOptions{
		OnDuplicate: "attach",
		Duplicate: scanResult{
			Status: "attach_candidate", ItemKey: existingKey, DOI: "10.1037/0021-9010.87.4.611",
		},
	}
	op := importPDFOpWithOptions(cmd, &rootFlags{asJSON: true}, nil, pdfPath, filepath.Base(pdfPath), 1, opts)
	if op.Kind != "import_pdf_duplicate_attach" {
		t.Fatalf("kind = %q, want import_pdf_duplicate_attach", op.Kind)
	}
	status, reason, err := op.Apply()
	if err != nil || status != "applied" {
		t.Fatalf("apply attach_candidate = %q, %v; reason=%#v", status, err, reason)
	}
	result, ok := reason.(importPDFResult)
	if !ok || result.ItemKey != existingKey || result.DuplicateOf != existingKey {
		t.Fatalf("result = %#v, want attachment on existing item %s", reason, existingKey)
	}
	creates, uploads, registers := fu.snapshot()
	if creates != 1 || uploads != 1 || registers != 1 || fu.parentSnapshot() != 0 {
		t.Fatalf("upload protocol = creates:%d uploads:%d registers:%d parents:%d, want one attachment and no parent item", creates, uploads, registers, fu.parentSnapshot())
	}
}

// importPDFOp (used by import apply's "recognize" step) must behave exactly as
// before: no duplicate handling, no collection filing.
func TestImportPDFOpBackwardCompatibleForImportApplyRecognize(t *testing.T) {
	f := newFakeImportPDFAPI(t)
	f.recognizedTitle = "Some Paper"
	f.recognizedType = "journalArticle"
	added := time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339)
	f.topItems = []map[string]any{{
		"key":  "P1",
		"data": map[string]any{"key": "P1", "itemType": "journalArticle", "title": f.recognizedTitle, "dateAdded": added},
	}}
	f.children["P1"] = []map[string]any{{
		"key":  "A1",
		"data": map[string]any{"key": "A1", "itemType": "attachment", "contentType": "application/pdf"},
	}}

	dir := t.TempDir()
	path := writeImportPDFFixture(t, dir, "some-paper.pdf")
	cmd := testApplyCmd()
	op := importPDFOp(cmd, f.flags(t), f.conn(), path, filepath.Base(path), 1)
	if op.Kind != "import_pdf" || len(op.Changes) != 1 {
		t.Fatalf("op = %+v, want a bare import_pdf op with one change", op)
	}
	status, reason, err := op.Apply()
	if err != nil || status != "applied" {
		t.Fatalf("apply = %q, %v, want applied", status, err)
	}
	result := reason.(importPDFResult)
	if result.ItemKey != "P1" || result.AttachmentKey != "A1" || result.CollectionKey != "" || result.DuplicateOf != "" {
		t.Fatalf("result = %+v, want plain recognition with no collection/duplicate fields", result)
	}
}

// --- report #1 finding 7: --collection --------------------------------------

func TestImportPDFCollectionFilesIntoResolvedKey(t *testing.T) {
	f := newFakeImportPDFAPI(t)
	f.recognizedTitle = "Filed Paper"
	f.recognizedType = "journalArticle"
	added := time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339)
	f.topItems = []map[string]any{{
		"key":  "ITEM001",
		"data": map[string]any{"key": "ITEM001", "itemType": "journalArticle", "title": f.recognizedTitle, "dateAdded": added},
	}}
	f.children["ITEM001"] = []map[string]any{{
		"key":  "ATT001",
		"data": map[string]any{"key": "ATT001", "itemType": "attachment", "contentType": "application/pdf"},
	}}
	f.itemVersion["ITEM001"] = 4

	dir := t.TempDir()
	path := writeImportPDFFixture(t, dir, "filed.pdf")
	cmd := testApplyCmd()
	op := importPDFOpWithOptions(cmd, f.flags(t), f.conn(), path, filepath.Base(path), 1, importPDFOptions{Collection: "COLKEY01", OnDuplicate: "create"})

	var collectionChange bool
	for _, c := range op.Changes {
		if c.Field == "collection" {
			collectionChange = true
		}
	}
	if !collectionChange {
		t.Fatalf("changes = %+v, want a collection change so --dry-run previews filing", op.Changes)
	}

	status, reason, err := op.Apply()
	if err != nil || status != "applied" {
		t.Fatalf("apply = %q, %v, want applied", status, err)
	}
	result := reason.(importPDFResult)
	if result.CollectionKey != "COLKEY01" {
		t.Fatalf("collection_key = %q, want COLKEY01", result.CollectionKey)
	}
	if !strings.Contains(result.CollectionNote, "filed") {
		t.Fatalf("collection_note = %q, want it to say the item was filed", result.CollectionNote)
	}
	if len(f.patchedItems) != 1 || f.patchedItems[0] != "ITEM001" {
		t.Fatalf("patched items = %v, want exactly ITEM001 patched once", f.patchedItems)
	}
	if got := f.itemCollections["ITEM001"]; len(got) != 1 || got[0] != "COLKEY01" {
		t.Fatalf("item collections = %v, want [COLKEY01]", got)
	}
}

// A collection name with no existing match is created, exactly like
// 'items add-to-collection --collection-name' creates one.
func TestImportPDFCollectionCreatesMissingNamedCollection(t *testing.T) {
	f := newFakeImportPDFAPI(t)
	f.recognizedTitle = "Named Collection Paper"
	f.recognizedType = "journalArticle"
	added := time.Now().UTC().Add(30 * time.Second).Format(time.RFC3339)
	f.topItems = []map[string]any{{
		"key":  "ITEM002",
		"data": map[string]any{"key": "ITEM002", "itemType": "journalArticle", "title": f.recognizedTitle, "dateAdded": added},
	}}
	f.children["ITEM002"] = []map[string]any{{
		"key":  "ATT002",
		"data": map[string]any{"key": "ATT002", "itemType": "attachment", "contentType": "application/pdf"},
	}}
	f.itemVersion["ITEM002"] = 1

	dir := t.TempDir()
	path := writeImportPDFFixture(t, dir, "named.pdf")
	cmd := testApplyCmd()
	op := importPDFOpWithOptions(cmd, f.flags(t), f.conn(), path, filepath.Base(path), 1, importPDFOptions{Collection: "New Papers", OnDuplicate: "create"})
	status, reason, err := op.Apply()
	if err != nil || status != "applied" {
		t.Fatalf("apply = %q, %v, want applied", status, err)
	}
	result := reason.(importPDFResult)
	if result.CollectionKey == "" || result.CollectionKey != f.collections["New Papers"] {
		t.Fatalf("collection_key = %q, want the newly created key for 'New Papers' (%q)", result.CollectionKey, f.collections["New Papers"])
	}
}

// When the created item's key cannot be resolved, --collection must report
// that clearly instead of silently doing nothing.
func TestImportPDFCollectionReportsWhenKeysUnresolved(t *testing.T) {
	f := newFakeImportPDFAPI(t)
	f.recognizedTitle = "Ambiguous Paper"
	f.recognizedType = "journalArticle"
	f.topItems = nil // nothing matches -> resolveImportPDFKeys leaves ItemKey empty

	dir := t.TempDir()
	path := writeImportPDFFixture(t, dir, "ambiguous.pdf")
	cmd := testApplyCmd()
	op := importPDFOpWithOptions(cmd, f.flags(t), f.conn(), path, filepath.Base(path), 1, importPDFOptions{Collection: "COLKEY01", OnDuplicate: "create"})
	status, reason, err := op.Apply()
	if err != nil || status != "applied" {
		t.Fatalf("apply = %q, %v, want applied (the import itself still succeeded)", status, err)
	}
	result := reason.(importPDFResult)
	if result.ItemKey != "" {
		t.Fatalf("item_key = %q, want empty (this test simulates unresolved keys)", result.ItemKey)
	}
	if result.CollectionKey != "" || !strings.Contains(result.CollectionNote, "not filed") || !strings.Contains(result.CollectionNote, "keys_note") {
		t.Fatalf("collection result = key:%q note:%q, want an explicit not-filed explanation", result.CollectionKey, result.CollectionNote)
	}
	if len(f.patchedItems) != 0 {
		t.Fatalf("patched items = %v, want none (nothing to file)", f.patchedItems)
	}
}

// --- report #2 finding 7: --on-duplicate attach -----------------------------

func TestImportPDFOnDuplicateAttachAttachesInsteadOfCreating(t *testing.T) {
	const existingKey = "PHMIJWH3"
	fu := newFakeZoteroUpload(t, existingKey)
	setUploadTestEnv(t, fu)

	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "dup.pdf")
	if err := os.WriteFile(pdfPath, []byte(uploadFixturePDF), 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}

	flags := &rootFlags{asJSON: true}
	scan := scanResult{Status: "duplicate", ItemKey: existingKey, DOI: "10.1037/0021-9010.87.4.611"}
	status, reason, err := applyImportPDFAttach(context.Background(), flags, pdfPath, scan)
	if err != nil {
		t.Fatalf("attach: %v; reason=%#v", err, reason)
	}
	if status != "applied" {
		t.Fatalf("status = %q, want applied", status)
	}
	result, ok := reason.(importPDFResult)
	if !ok {
		t.Fatalf("reason = %#v, want importPDFResult", reason)
	}
	if result.ItemKey != existingKey || result.DuplicateOf != existingKey {
		t.Fatalf("result = %+v, want item_key/duplicate_of = %s (attach must not mint a new item)", result, existingKey)
	}
	if result.AttachmentKey == "" {
		t.Fatalf("result = %+v, want a non-empty attachment_key", result)
	}
	creates, uploads, registers := fu.snapshot()
	if creates != 1 || uploads != 1 || registers != 1 {
		t.Fatalf("upload protocol calls = creates:%d uploads:%d registers:%d, want one each", creates, uploads, registers)
	}
	if fu.parentSnapshot() != 0 {
		t.Fatalf("parent creates = %d, want 0 (attach reuses the existing item)", fu.parentSnapshot())
	}
}

// --- duplicate-index loading is best-effort ---------------------------------

func TestLoadImportPDFDuplicateIndexDisabledWithoutSyncedStore(t *testing.T) {
	isolateDemoEnv(t, "0")
	idx, warning := loadImportPDFDuplicateIndex(context.Background())
	if len(idx.byDOI) != 0 {
		t.Fatalf("idx = %+v, want empty without a synced store", idx)
	}
	if !strings.Contains(warning, "duplicate detection disabled") || !strings.Contains(warning, "sync") {
		t.Fatalf("warning = %q, want it to name the store gap and the fix", warning)
	}
}

func TestLoadImportPDFDuplicateIndexUsesSyncedStore(t *testing.T) {
	seedImportPDFDuplicateStore(t, "10.1/x", "EXIST01")
	idx, warning := loadImportPDFDuplicateIndex(context.Background())
	if warning != "" {
		t.Fatalf("warning = %q, want none with a healthy store", warning)
	}
	li, ok := idx.byDOI["10.1/x"]
	if !ok || li.key != "EXIST01" || !li.hasPDF {
		t.Fatalf("idx[10.1/x] = %+v (ok=%v), want EXIST01 with hasPDF true", li, ok)
	}
}

// --- zotio-f28769caf83cbfdf: precondition error must surface underlying cause ---

func TestImportPDFViaConnectorSurfacesUnderlyingError(t *testing.T) {
	// Explicit --via connector with a non-local base must include the cause.
	flags := &rootFlags{asJSON: true, via: "connector", configPath: testConfigFile(t, "https://api.zotero.org/users/1")}
	cmd := newImportPDFCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"dummy.pdf"})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("import pdf with non-local base and --via connector succeeded, want precondition error")
	}
	helpersTestAssertCLIError(t, err, 9)
	msg := err.Error()
	if !strings.Contains(msg, "desktop connector") {
		t.Fatalf("error = %q, want 'desktop connector'", msg)
	}
	// The underlying cause (non-local base / local URL requirement) must be visible.
	if !strings.Contains(msg, "local") {
		t.Fatalf("error = %q, want underlying cause to mention 'local'", msg)
	}
}

func TestImportPDFViaConnectorUnreachableSurfacesCause(t *testing.T) {
	oldPing := connectorPing
	t.Cleanup(func() { connectorPing = oldPing })
	connectorPing = func(context.Context, *connector.Client) error {
		return fmt.Errorf("dial tcp 127.0.0.1:23119: connect: connection refused")
	}
	flags := &rootFlags{asJSON: true, via: "connector", configPath: testConfigFile(t, "http://localhost:23119/api/users/0")}
	cmd := newImportPDFCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"dummy.pdf"})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("import pdf with unreachable connector succeeded, want precondition error")
	}
	helpersTestAssertCLIError(t, err, 9)
	msg := err.Error()
	if !strings.Contains(msg, "not reachable") {
		t.Fatalf("error = %q, want 'not reachable' cause", msg)
	}
}

func TestImportPDFAutoWithoutConnectorIsGeneric(t *testing.T) {
	// Auto mode with non-local base: via resolves to "web", so the precondition
	// error is the generic fallback and must not leak a nil-cause wrap.
	flags := &rootFlags{asJSON: true, via: "auto", configPath: testConfigFile(t, "https://api.zotero.org/users/1")}
	cmd := newImportPDFCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"dummy.pdf"})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("import pdf auto non-local succeeded, want precondition error")
	}
	helpersTestAssertCLIError(t, err, 9)
	msg := err.Error()
	if !strings.Contains(msg, "local base URL") {
		t.Fatalf("error = %q, want generic 'local base URL' message", msg)
	}
}
