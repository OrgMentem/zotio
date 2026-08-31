// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"zotio/internal/connector"
	"zotio/internal/mutation"
)

func connectorAttachmentURLFromRequest(r *http.Request) (string, error) {
	var metadata struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(r.Header.Get("X-Metadata")), &metadata); err != nil {
		return "", err
	}
	return metadata.URL, nil
}

type connectorAttachmentTestState struct {
	mu  sync.Mutex
	url string
}

func (s *connectorAttachmentTestState) record(r *http.Request) error {
	value, err := connectorAttachmentURLFromRequest(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.url = value
	s.mu.Unlock()
	return nil
}

func (s *connectorAttachmentTestState) snapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.url
}

func TestImportApplyPreviewPlansCreateAndLinkedAttach(t *testing.T) {
	manifestPath := writeImportApplyTestManifest(t, importApplyTestManifest())
	env, stderr, err := runImportApplyTestCmd(t, []string{"--attach-mode", "linked-file", manifestPath})
	if err != nil {
		t.Fatalf("import apply: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "preview" || env.Result != nil {
		t.Fatalf("env = %+v, want successful preview", env)
	}
	if env.Plan.Summary.Planned != 2 || env.Plan.Summary.NoOp != 1 || len(env.Plan.Operations) != 3 {
		t.Fatalf("plan = %+v, ops=%+v; want two planned ops and one duplicate no-op", env.Plan.Summary, env.Plan.Operations)
	}
	if env.Plan.Operations[0].Kind != "import_create" || env.Plan.Operations[1].Kind != "import_attach" || env.Plan.Operations[2].Kind != "import_duplicate" {
		t.Fatalf("operations = %+v; want create, attach, duplicate", env.Plan.Operations)
	}
}

func TestImportApplyDefaultAttachModeNoneMakesAttachNoOp(t *testing.T) {
	manifestPath := writeImportApplyTestManifest(t, importApplyTestManifest())
	env, stderr, err := runImportApplyTestCmd(t, []string{manifestPath})
	if err != nil {
		t.Fatalf("import apply: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "preview" || env.Result != nil {
		t.Fatalf("env = %+v, want successful preview", env)
	}
	if env.Plan.Summary.Planned != 1 || env.Plan.Summary.NoOp != 2 || len(env.Plan.Operations) != 3 {
		t.Fatalf("plan = %+v, ops=%+v; want create planned plus attach and duplicate no-ops", env.Plan.Summary, env.Plan.Operations)
	}
	attach, duplicate := env.Plan.Operations[1], env.Plan.Operations[2]
	if attach.Kind != "import_attach" || attach.Key != "MATCH1" || len(attach.Changes) != 0 ||
		duplicate.Kind != "import_duplicate" || duplicate.Key != "DUP1" || len(duplicate.Changes) != 0 {
		t.Fatalf("attach=%+v duplicate=%+v, want explicit no-ops", attach, duplicate)
	}
}

func TestImportApplyDuplicateIsAuditableNoOp(t *testing.T) {
	manifest := importApplyTestManifest()
	manifest.Entries = manifest.Entries[2:]
	manifestPath := writeImportApplyTestManifest(t, manifest)
	env, stderr, err := runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true, via: "web", yes: true}, []string{manifestPath})
	if err != nil {
		t.Fatalf("duplicate apply: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.NoOp != 1 || env.Result.Summary.Applied != 0 {
		t.Fatalf("env = %+v, want one applied duplicate no-op", env)
	}
	if len(env.Result.Items) != 1 || env.Result.Items[0].Key != "DUP1" || env.Result.Items[0].Status != "no_op" {
		t.Fatalf("items = %+v, want duplicate parent no-op", env.Result.Items)
	}
}

func TestImportApplyStoredPreviewSupportsWebCreates(t *testing.T) {
	manifestPath := writeImportApplyTestManifest(t, importApplyTestManifest())
	env, stderr, err := runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true, via: "web"}, []string{"--attach-mode", "stored", manifestPath})
	if err != nil {
		t.Fatalf("stored Web preview: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "preview" || env.Plan.Summary.Planned != 2 {
		t.Fatalf("env = %+v, want two-operation stored Web preview", env)
	}
}

// A stored manifest with only attach entries needs the Web API, not the connector.
func TestImportApplyStoredAttachOnlyManifestSkipsConnector(t *testing.T) {
	manifest := importApplyTestManifest()
	entries := manifest.Entries[:0]
	for _, entry := range manifest.Entries {
		if entry.Action == "attach" {
			entries = append(entries, entry)
		}
	}
	manifest.Entries = entries
	manifestPath := writeImportApplyTestManifest(t, manifest)
	env, stderr, err := runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true, via: "web"}, []string{"--attach-mode", "stored", manifestPath})
	if err != nil {
		t.Fatalf("stored attach-only preview: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "preview" || len(env.Plan.Operations) != 1 || env.Plan.Operations[0].Kind != "import_attach" {
		t.Fatalf("env = %+v ops=%+v, want one previewed import_attach op", env, env.Plan.Operations)
	}
}
func TestManifestUnidentifiedDefaultsToRecognize(t *testing.T) {
	if got := manifestActionForStatus("unidentified"); got != "recognize" {
		t.Fatalf("manifestActionForStatus(unidentified) = %q, want recognize", got)
	}
}

func TestImportApplyRecognizeRequiresConnector(t *testing.T) {
	manifest := importApplyTestManifest()
	manifest.Entries = []importManifestEntry{{
		Path:   filepath.Join(string(filepath.Separator), "tmp", "unknown.pdf"),
		Action: "recognize",
		Status: "resolved",
		Title:  "Unknown PDF",
	}}
	manifestPath := writeImportApplyTestManifest(t, manifest)
	_, _, err := runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true, via: "web"}, []string{manifestPath})
	if err == nil || !strings.Contains(err.Error(), "action recognize requires the desktop connector") {
		t.Fatalf("err = %v, want recognize connector precondition error", err)
	}
}

func TestImportApplyRecognizePlansImportPDF(t *testing.T) {
	oldPing := connectorPing
	defer func() { connectorPing = oldPing }()
	connectorPing = func(ctx context.Context, c *connector.Client) error { return nil }

	manifest := importApplyTestManifest()
	manifest.Entries = []importManifestEntry{{
		Path:   filepath.Join(string(filepath.Separator), "tmp", "unknown.pdf"),
		Action: "recognize",
		Status: "resolved",
		Title:  "Unknown PDF",
	}}
	manifestPath := writeImportApplyTestManifest(t, manifest)
	flags := &rootFlags{asJSON: true, via: "connector", configPath: testConfigFile(t, "http://localhost:23119/api/users/0")}
	env, stderr, err := runImportApplyTestCmdWithFlags(t, flags, []string{manifestPath})
	if err != nil {
		t.Fatalf("import apply recognize preview: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Plan.Summary.Planned != 1 || len(env.Plan.Operations) != 1 || env.Plan.Operations[0].Kind != "import_pdf" {
		t.Fatalf("env = %+v ops=%+v, want one import_pdf preview op", env, env.Plan.Operations)
	}
}

func TestImportApplyStoredPreviewPlansConnectorCreateAndWebAttach(t *testing.T) {
	oldPing := connectorPing
	defer func() { connectorPing = oldPing }()
	connectorPing = func(ctx context.Context, c *connector.Client) error { return nil }

	manifestPath := writeImportApplyTestManifest(t, importApplyTestManifest())
	flags := &rootFlags{asJSON: true, via: "connector", configPath: testConfigFile(t, "http://localhost:23119/api/users/0")}
	env, stderr, err := runImportApplyTestCmdWithFlags(t, flags, []string{"--attach-mode", "stored", manifestPath})
	if err != nil {
		t.Fatalf("import apply stored preview: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "preview" || env.Result != nil {
		t.Fatalf("env = %+v, want successful stored preview", env)
	}
	if env.Plan.Summary.Planned != 2 || env.Plan.Summary.NoOp != 1 || len(env.Plan.Operations) != 3 {
		t.Fatalf("plan = %+v, ops=%+v; want connector create, web-upload attach, and duplicate no-op", env.Plan.Summary, env.Plan.Operations)
	}
}

func TestImportApplyStoredWebCreateAppliesParentAndAttachment(t *testing.T) {
	fastRetryBackoff(t)
	fake := newFakeZoteroUpload(t, "")
	pdf := writeUploadFixture(t, "created-paper.pdf", []byte("%PDF-1.4\ncreated\n%%EOF"))
	manifest := importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           filepath.Dir(pdf),
		Entries: []importManifestEntry{{
			Path: pdf, Action: "create", Status: "resolved", Title: "Created Paper",
			Item: map[string]any{"itemType": "journalArticle", "title": "Created Paper", "DOI": "10.1002/example"},
		}},
	}
	manifestPath := writeImportApplyTestManifest(t, manifest)
	flags := &rootFlags{
		asJSON: true, yes: true, via: "web", maxChanges: -1,
		configPath: testConfigFile(t, fake.srv.URL+"/users/0"),
	}
	env, stderr, err := runImportApplyTestCmdWithFlags(t, flags, []string{"--attach-mode", "stored", manifestPath})
	if err != nil {
		t.Fatalf("stored Web create apply: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Summary.Failed != 0 {
		t.Fatalf("env = %+v, want one applied create+attachment operation", env)
	}
	reason, ok := env.Result.Items[0].Reason.(map[string]any)
	if !ok || reason["parent_key"] != "PARENT1" || reason["attachment_key"] != "ATT1" {
		t.Fatalf("reason = %#v, want returned parent and attachment keys", env.Result.Items[0].Reason)
	}
	creates, uploads, registers := fake.snapshot()
	if fake.parentSnapshot() != 1 || creates != 1 || uploads != 1 || registers != 1 {
		t.Fatalf("traffic parent=%d attachment=%d upload=%d register=%d, want 1 each", fake.parentSnapshot(), creates, uploads, registers)
	}

	child := fake.findChild("ATT1")
	if child == nil || child.Title != filepath.Base(pdf) {
		var got string
		if child != nil {
			got = child.Title
		}
		t.Fatalf("stored Web attachment title = %q, want filename %q", got, filepath.Base(pdf))
	}
}

func TestImportApplyStoredConnectorCreatePreservesUnresolvedSaveItemsEvidence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })
	listener, err := net.Listen("tcp", "127.0.0.1:23119")
	if err != nil {
		t.Skipf("port 23119 is unavailable: %v", err)
	}
	var saveItems, saveAttachments int
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connector/ping":
			w.WriteHeader(http.StatusOK)
		case "/connector/saveItems":
			saveItems++
			http.Error(w, "simulated post-commit connector failure", http.StatusInternalServerError)
		case "/connector/saveAttachment":
			saveAttachments++
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	pdf := writeUploadFixture(t, "unresolved-parent.pdf", []byte("%PDF-1.4\nunresolved\n%%EOF"))
	manifestPath := writeImportApplyTestManifest(t, importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           filepath.Dir(pdf),
		Entries: []importManifestEntry{{
			Path: pdf, Action: "create", Status: "resolved", Title: "Unresolved Parent",
			Item: map[string]any{"itemType": "journalArticle", "title": "Unresolved Parent"},
		}},
	})
	flags := &rootFlags{
		asJSON: true, yes: true, via: "connector", timeout: time.Second, maxChanges: -1,
		configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0"),
	}
	env, _, err := runImportApplyTestCmdWithFlags(t, flags, []string{"--attach-mode", "stored", manifestPath})
	if err == nil || env.Result == nil || env.Result.Summary.Conflicts != 1 {
		t.Fatalf("unresolved SaveItems result = %+v err=%v, want one committed conflict", env, err)
	}
	reason, ok := env.Result.Items[0].Reason.(map[string]any)
	if !ok || reason["committed"] != true || reason["session"] == "" ||
		reason["connector_key"] == "" || reason["title"] != "Unresolved Parent" {
		t.Fatalf("reason = %#v, want recoverable connector evidence", env.Result.Items[0].Reason)
	}
	if _, guessed := reason["parent_key"]; guessed || saveItems != 1 || saveAttachments != 0 {
		t.Fatalf("reason=%#v writes=%d/%d, want no guessed key or automatic retry", reason, saveItems, saveAttachments)
	}
	entries, listErr := mutation.ListEntries(helpersTestJournalDir(t))
	if listErr != nil || len(entries) != 1 || entries[0].Ops[0].Status != "conflict" {
		t.Fatalf("journal=%+v err=%v, want unresolved committed write recorded", entries, listErr)
	}
}

func TestImportApplyStoredConnectorCreateRejectsPreexistingSamePDF(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:23119")
	if err != nil {
		t.Skipf("port 23119 is unavailable: %v", err)
	}
	oldRecoveryWindow := connectorCreateRecoveryWindow
	oldRecoveryInterval := connectorCreateRecoveryInterval
	oldVisibilityTimeout := storedConnectorRecoveryTimeout
	oldReparentInterval := connectorReparentPollInterval
	connectorCreateRecoveryWindow = 0
	connectorCreateRecoveryInterval = time.Millisecond
	storedConnectorRecoveryTimeout = 0
	connectorReparentPollInterval = time.Millisecond
	t.Cleanup(func() {
		connectorCreateRecoveryWindow = oldRecoveryWindow
		connectorCreateRecoveryInterval = oldRecoveryInterval
		storedConnectorRecoveryTimeout = oldVisibilityTimeout
		connectorReparentPollInterval = oldReparentInterval
	})
	var saveItems, saveAttachments int
	var wantMD5 string
	attachmentState := &connectorAttachmentTestState{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connector/ping":
			w.WriteHeader(http.StatusOK)
		case "/connector/saveItems":
			saveItems++
			w.WriteHeader(http.StatusCreated)
		case "/connector/saveAttachment":
			saveAttachments++
			w.WriteHeader(http.StatusCreated)
			if err := attachmentState.record(r); err != nil {
				t.Errorf("record connector attachment URL: %v", err)
			}
		case "/api/users/0/items":
			w.Header().Set("Last-Modified-Version", "1")
			_ = json.NewEncoder(w).Encode([]any{})
		case "/api/users/0/items/top":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{
				"key": "STALE123", "title": "Same PDF Paper",
				"itemType": "journalArticle", "dateAdded": time.Now().UTC().Format(time.RFC3339),
			}})
		case "/api/users/0/items/STALE123/children":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{
				"key": "OLDPDF23", "data": map[string]any{
					"itemType": "attachment", "md5": wantMD5, "url": "https://doi.org/old-unmarked-copy",
				},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	pdf := writeUploadFixture(t, "same-paper.pdf", []byte("%PDF-1.4\nsame\n%%EOF"))
	wantMD5, err = fileMD5(pdf)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := writeImportApplyTestManifest(t, importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           filepath.Dir(pdf),
		Entries: []importManifestEntry{{
			Path: pdf, Action: "create", Status: "resolved", Title: "Same PDF Paper",
			Item: map[string]any{"itemType": "journalArticle", "title": "Same PDF Paper"},
		}},
	})
	flags := &rootFlags{
		asJSON: true, yes: true, via: "connector", timeout: time.Second, maxChanges: -1,
		configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0"),
	}
	env, _, err := runImportApplyTestCmdWithFlags(t, flags, []string{"--attach-mode", "stored", manifestPath})
	if err == nil || env.Result == nil || env.Result.Summary.Conflicts != 1 {
		t.Fatalf("same-PDF stale parent result = %+v err=%v, want one conflict", env, err)
	}
	reason, ok := env.Result.Items[0].Reason.(map[string]any)
	if !ok || reason["parent_matches"] != float64(1) || reason["file_matches"] != float64(0) {
		t.Fatalf("reason = %#v, want the same-PDF parent rejected without this write's URL marker", env.Result.Items[0].Reason)
	}
	if _, guessed := reason["parent_key"]; guessed || saveItems != 1 || saveAttachments != 1 {
		t.Fatalf("reason=%#v writes=%d/%d, want no stale key and one committed write", reason, saveItems, saveAttachments)
	}
	marker, _ := reason["attachment_marker"].(string)
	if markedURL := attachmentState.snapshot(); marker == "" || !attachmentURLHasMarker(markedURL, marker) {
		t.Fatalf("marked URL=%q marker=%q, want unresolved write evidence retained", markedURL, marker)
	}
}

func TestImportApplyStoredConnectorCreateRefusesAmbiguousParent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })
	listener, err := net.Listen("tcp", "127.0.0.1:23119")
	if err != nil {
		t.Skipf("port 23119 is unavailable: %v", err)
	}
	oldRecoveryWindow := connectorCreateRecoveryWindow
	oldRecoveryInterval := connectorCreateRecoveryInterval
	connectorCreateRecoveryWindow = 0
	connectorCreateRecoveryInterval = time.Millisecond
	t.Cleanup(func() {
		connectorCreateRecoveryWindow = oldRecoveryWindow
		connectorCreateRecoveryInterval = oldRecoveryInterval
	})
	var saveItemRequests, saveAttachmentRequests int
	var wantMD5 string
	attachmentState := &connectorAttachmentTestState{}
	connectorServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connector/ping":
			w.WriteHeader(http.StatusOK)
		case "/connector/saveItems":
			saveItemRequests++
			w.WriteHeader(http.StatusCreated)
		case "/connector/saveAttachment":
			saveAttachmentRequests++
			w.WriteHeader(http.StatusCreated)
			if err := attachmentState.record(r); err != nil {
				t.Errorf("record connector attachment URL: %v", err)
			}
		case "/api/users/0/items":
			w.Header().Set("Last-Modified-Version", "1")
			_ = json.NewEncoder(w).Encode([]any{})
		case "/api/users/0/items/top":
			if saveAttachmentRequests == 0 {
				_ = json.NewEncoder(w).Encode([]any{})
				return
			}
			added := time.Now().UTC().Format(time.RFC3339)
			_ = json.NewEncoder(w).Encode([]any{
				map[string]any{"key": "PARENT23", "title": "Ambiguous Paper", "itemType": "journalArticle", "dateAdded": added},
				map[string]any{"key": "PARENT24", "title": "Ambiguous Paper", "itemType": "journalArticle", "dateAdded": added},
			})
		case "/api/users/0/items/PARENT23/children":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{
				"key": "ATTACH23", "data": map[string]any{
					"itemType": "attachment", "md5": wantMD5, "url": attachmentState.snapshot(),
				},
			}})
		case "/api/users/0/items/PARENT24/children":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{
				"key": "ATTACH24", "data": map[string]any{
					"itemType": "attachment", "md5": wantMD5, "url": attachmentState.snapshot(),
				},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	connectorServer.Listener = listener
	connectorServer.Start()
	t.Cleanup(connectorServer.Close)

	pdf := writeUploadFixture(t, "ambiguous-paper.pdf", []byte("%PDF-1.4\nambiguous\n%%EOF"))
	wantMD5, err = fileMD5(pdf)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := writeImportApplyTestManifest(t, importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           filepath.Dir(pdf),
		Entries: []importManifestEntry{{
			Path: pdf, Action: "create", Status: "resolved", Title: "Ambiguous Paper",
			Item: map[string]any{"itemType": "journalArticle", "title": "Ambiguous Paper"},
		}},
	})
	flags := &rootFlags{
		asJSON: true, yes: true, via: "connector", timeout: time.Second, maxChanges: -1,
		configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0"),
	}
	env, _, err := runImportApplyTestCmdWithFlags(t, flags, []string{"--attach-mode", "stored", manifestPath})
	if err == nil {
		t.Fatalf("ambiguous stored connector apply succeeded: %+v", env)
	}
	if env.Result == nil || env.Result.Summary.Conflicts != 1 || len(env.Result.Items) != 1 {
		t.Fatalf("env = %+v, want one connector conflict", env)
	}
	item := env.Result.Items[0]
	reason, ok := item.Reason.(map[string]any)
	if !ok || item.Status != "conflict" ||
		reason["recovery_target"] != "parent_attachment_keys" ||
		reason["parent_matches"] != float64(2) || reason["file_matches"] != float64(2) ||
		reason["committed"] != true || reason["title"] != "Ambiguous Paper" ||
		reason["session"] == "" || reason["connector_key"] == "" {
		t.Fatalf("item = %+v, want committed ambiguous key-recovery conflict", item)
	}
	if _, returned := reason["parent_key"]; returned || saveItemRequests != 1 || saveAttachmentRequests != 1 {
		t.Fatalf("reason=%#v requests saveItems=%d saveAttachment=%d, want no guessed key and one write each",
			reason, saveItemRequests, saveAttachmentRequests)
	}
	entries, listErr := mutation.ListEntries(helpersTestJournalDir(t))
	if listErr != nil {
		t.Fatalf("list committed conflict journal: %v", listErr)
	}
	if len(entries) != 1 || len(entries[0].Ops) != 1 || entries[0].Ops[0].Status != "conflict" {
		t.Fatalf("journal = %+v, want one committed conflict operation", entries)
	}
	journalReason, ok := entries[0].Ops[0].Reason.(map[string]any)
	if !ok || journalReason["title"] != "Ambiguous Paper" || journalReason["session"] == "" ||
		journalReason["connector_key"] == "" || journalReason["committed"] != true ||
		journalReason["attachment_marker"] == "" {
		t.Fatalf("journal reason = %#v, want recoverable committed-write evidence", entries[0].Ops[0].Reason)
	}
	marker, _ := reason["attachment_marker"].(string)
	if markedURL := attachmentState.snapshot(); marker == "" || !attachmentURLHasMarker(markedURL, marker) {
		t.Fatalf("marked URL=%q marker=%q, want ambiguous write evidence retained", markedURL, marker)
	}
}

// A stored create commits the parent before it uploads the file, and nothing
// rolls that back. When the upload then fails, the operator is left with an
// item that has no file, so the failure must hand back enough to find it.
// Previously this returned a bare {"parent_key": ...} map, which the human
// renderer printed as a Go map literal with no explanation.
//
// This exercises the real apply path against the fake Zotero Web API, so it
// runs everywhere — unlike the connector-route sibling, which needs port 23119
// and skips on any machine where Zotero desktop is running.
func TestImportApplyStoredWebCreateReportsOrphanedParentOnUploadFailure(t *testing.T) {
	fake := newFakeZoteroUpload(t, "")
	fake.quotaOnAuth = true // 413 at upload authorization, after the parent exists
	pdf := writeUploadFixture(t, "orphan-paper.pdf", []byte("%PDF-1.4\norphan\n%%EOF"))
	manifest := importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           filepath.Dir(pdf),
		Entries: []importManifestEntry{{
			Path: pdf, Action: "create", Status: "resolved", Title: "Orphan Paper",
			Item: map[string]any{"itemType": "journalArticle", "title": "Orphan Paper"},
		}},
	}
	manifestPath := writeImportApplyTestManifest(t, manifest)
	flags := &rootFlags{
		asJSON: true, yes: true, via: "web", maxChanges: -1,
		configPath: testConfigFile(t, fake.srv.URL+"/users/0"),
	}
	env, stderr, err := runImportApplyTestCmdWithFlags(t, flags, []string{"--attach-mode", "stored", manifestPath})
	if err == nil {
		t.Fatalf("upload failure after parent create succeeded; env=%+v stderr=%s", env, stderr)
	}
	// The premise: the parent really was committed, so this is an orphan and
	// not a clean no-op.
	if got := fake.parentSnapshot(); got != 1 {
		t.Fatalf("parent create requests = %d, want 1; the fixture no longer reproduces an orphan", got)
	}
	if env.Result == nil || len(env.Result.Items) != 1 {
		t.Fatalf("env = %+v, want one failed result", env)
	}
	reason, ok := env.Result.Items[0].Reason.(map[string]any)
	if !ok {
		t.Fatalf("reason = %#v, want a structured detail map", env.Result.Items[0].Reason)
	}
	if reason["parent_key"] != "PARENT1" {
		t.Fatalf("reason = %#v, want the created parent's key", reason)
	}
	if reason["title"] != "Orphan Paper" {
		t.Fatalf("reason = %#v, want the title to search Zotero by", reason)
	}
	message, _ := reason["message"].(string)
	if !strings.Contains(message, "created but the file was not attached") {
		t.Fatalf("message = %q, want the orphaned-parent condition stated", message)
	}
	if !strings.Contains(message, "delete the item and re-run") {
		t.Fatalf("message = %q, want a deterministic next step", message)
	}
	// reasonText prints only "message"; without it a person sees a Go map.
	if rendered := mutation.Rows(env); !strings.Contains(strings.Join(rendered, " "), "created but the file was not attached") {
		t.Fatalf("human rows = %q, want the orphan explanation rendered", rendered)
	}
}

// The connector-route integration test above needs port 23119 and therefore
// skips on any machine where Zotero desktop is running, which is most
// developer machines. This pins the evidence shape unconditionally so the
// connector contract is not defended only on CI.
func TestOrphanedConnectorParentDetailCarriesFindableEvidence(t *testing.T) {
	cause := errors.New("connector saveAttachment: HTTP 500")
	res := itemCreateResult{Via: "connector", Session: "sess-1", ConnKey: "conn-1"}

	detail := orphanedConnectorParentDetail(res, "Scanned Paper", cause)
	for key, want := range map[string]any{
		"via": "connector", "session": "sess-1", "connector_key": "conn-1", "title": "Scanned Paper",
	} {
		if detail[key] != want {
			t.Fatalf("detail[%q] = %v, want %v", key, detail[key], want)
		}
	}
	// The connector protocol returns no Zotero item key, so claiming one would
	// send the operator looking for a key that does not exist.
	if _, present := detail["parent_key"]; present {
		t.Fatalf("detail = %#v, must not invent a parent_key the connector never returned", detail)
	}
	res.WebKey = "ABCD1234"
	if _, present := orphanedConnectorParentDetail(res, "Scanned Paper", cause)["parent_key"]; present {
		t.Fatal("title-only WebKey was reported as committed parent evidence")
	}
	// The whole point of "message": reasonText prints only that key, so its
	// absence renders the failure as a bare Go map. With the integration test
	// skipped whenever Zotero holds :23119, nothing else pins it.
	msg, ok := detail["message"].(string)
	if !ok || !strings.Contains(msg, "created but the file was not attached") {
		t.Fatalf("detail[message] = %v, want the orphan sentence the human renderer prints", detail["message"])
	}
	if rendered := mutation.Rows(mutation.Envelope{Result: &mutation.Result{
		Items: []mutation.ResultItem{{Status: "failed", Reason: detail}},
	}}); !strings.Contains(strings.Join(rendered, " "), "created but the file was not attached") {
		t.Fatalf("human rows = %q, want the connector orphan explanation rendered", rendered)
	}

	// The dispatcher used by failure sites above the route switch, where the
	// route is known only from the result.
	if got := orphanedParentDetail(itemCreateResult{Via: "web", WebKey: "WEB1"}, "T", cause)["via"]; got != "web" {
		t.Fatalf("dispatch via = %v, want the web shape for a web result", got)
	}
	if got := orphanedParentDetail(itemCreateResult{Via: "connector", Session: "s"}, "T", cause)["via"]; got != "connector" {
		t.Fatalf("dispatch via = %v, want the connector shape for a connector result", got)
	}

	err := orphanedParentError("Scanned Paper", cause)
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want the underlying cause preserved for callers", err)
	}
	if !strings.Contains(err.Error(), "created but the file was not attached") {
		t.Fatalf("error = %q, want the orphan condition stated", err)
	}
}

func TestImportApplyStoredCreateRejectsMissingAttachmentBeforeParent(t *testing.T) {
	fake := newFakeZoteroUpload(t, "")
	missing := filepath.Join(t.TempDir(), "missing.pdf")
	manifest := importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           filepath.Dir(missing),
		Entries: []importManifestEntry{{
			Path: missing, Action: "create", Status: "resolved", Title: "Missing Paper",
			Item: map[string]any{"itemType": "journalArticle", "title": "Missing Paper"},
		}},
	}
	manifestPath := writeImportApplyTestManifest(t, manifest)
	flags := &rootFlags{
		asJSON: true, yes: true, via: "web", maxChanges: -1,
		configPath: testConfigFile(t, fake.srv.URL+"/users/0"),
	}
	env, stderr, err := runImportApplyTestCmdWithFlags(t, flags, []string{"--attach-mode", "stored", manifestPath})
	if err == nil {
		t.Fatalf("stored create with missing attachment succeeded; env=%+v stderr=%s", env, stderr)
	}
	if env.Result == nil || env.Result.Summary.Failed != 1 || len(env.Result.Items) != 1 ||
		!strings.Contains(fmt.Sprint(env.Result.Items[0].Reason), "reading attachment") {
		t.Fatalf("env = %+v, want a failed attachment-read result", env)
	}
	if got := fake.parentSnapshot(); got != 0 {
		t.Fatalf("parent create requests = %d, want 0", got)
	}
}

func TestImportApplyLinkedFileWebCreateReturnsParentAndAttachmentKeys(t *testing.T) {
	fake := newFakeZoteroUpload(t, "")
	pdf := writeUploadFixture(t, "linked-paper.pdf", []byte("%PDF-1.4\nlinked\n%%EOF"))
	manifest := importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           filepath.Dir(pdf),
		Entries: []importManifestEntry{{
			Path: pdf, Action: "create", Status: "resolved", Title: "Linked Paper",
			Item: map[string]any{"itemType": "journalArticle", "title": "Linked Paper", "DOI": "10.1002/linked"},
		}},
	}
	manifestPath := writeImportApplyTestManifest(t, manifest)
	flags := &rootFlags{
		asJSON: true, yes: true, via: "web", maxChanges: -1,
		configPath: testConfigFile(t, fake.srv.URL+"/users/0"),
	}
	env, stderr, err := runImportApplyTestCmdWithFlags(t, flags, []string{"--attach-mode", "linked-file", manifestPath})
	if err != nil {
		t.Fatalf("linked-file Web create apply: %v; result=%+v; stderr=%s", err, env.Result, stderr)
	}
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Summary.Failed != 0 {
		t.Fatalf("env = %+v, want one applied create+attachment operation", env)
	}
	reason, ok := env.Result.Items[0].Reason.(map[string]any)
	if !ok || reason["parent_key"] != "PARENT1" || reason["attachment_key"] != "ATT1" {
		t.Fatalf("reason = %#v, want returned parent and attachment keys", env.Result.Items[0].Reason)
	}
	creates, uploads, registers := fake.snapshot()
	if fake.parentSnapshot() != 1 || creates != 1 || uploads != 0 || registers != 0 {
		t.Fatalf("traffic parent=%d attachment=%d upload=%d register=%d, want parent=1 attachment=1 upload=0 register=0",
			fake.parentSnapshot(), creates, uploads, registers)
	}
}

// TestImportApplyStoredConnectorCreateReportsOrphanedParentOnAttachFailure
// covers zotio-orphan-evidence: routeCreateItem's SaveItems call commits the
// parent in Zotero desktop before SaveAttachment ever runs, and the two calls
// are not transactional (see connector.SaveAttachment's doc comment). When
// SaveAttachment then fails, the operator must be told the parent already
// exists and given evidence to find it, not a bare error.
func TestImportApplyStoredConnectorCreateReportsOrphanedParentOnAttachFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })
	listener, err := net.Listen("tcp", "127.0.0.1:23119")
	if err != nil {
		t.Skipf("port 23119 is unavailable: %v", err)
	}
	oldRecoveryWindow := connectorCreateRecoveryWindow
	oldRecoveryInterval := connectorCreateRecoveryInterval
	connectorCreateRecoveryWindow = 0
	connectorCreateRecoveryInterval = time.Millisecond
	t.Cleanup(func() {
		connectorCreateRecoveryWindow = oldRecoveryWindow
		connectorCreateRecoveryInterval = oldRecoveryInterval
	})
	var saveItemRequests, saveAttachmentRequests int
	attachmentState := &connectorAttachmentTestState{}
	connectorServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connector/ping":
			w.WriteHeader(http.StatusOK)
		case "/connector/saveItems":
			saveItemRequests++
			w.WriteHeader(http.StatusCreated)
		case "/connector/saveAttachment":
			saveAttachmentRequests++
			if err := attachmentState.record(r); err != nil {
				t.Errorf("record connector attachment URL: %v", err)
			}
			http.Error(w, "simulated WebDAV filing failure", http.StatusInternalServerError)
		case "/api/users/0/items":
			w.Header().Set("Last-Modified-Version", "1")
			_ = json.NewEncoder(w).Encode([]any{})
		case "/api/users/0/items/top":
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	connectorServer.Listener = listener
	connectorServer.Start()
	t.Cleanup(connectorServer.Close)

	pdf := writeUploadFixture(t, "orphan-paper.pdf", []byte("%PDF-1.4\norphan\n%%EOF"))
	manifest := importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           filepath.Dir(pdf),
		Entries: []importManifestEntry{{
			Path: pdf, Action: "create", Status: "resolved", Title: "Orphan Paper",
			Item: map[string]any{"itemType": "journalArticle", "title": "Orphan Paper"},
		}},
	}
	manifestPath := writeImportApplyTestManifest(t, manifest)
	flags := &rootFlags{
		asJSON: true, yes: true, via: "connector", timeout: time.Second, maxChanges: -1,
		configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0"),
	}
	env, stderr, err := runImportApplyTestCmdWithFlags(t, flags, []string{"--attach-mode", "stored", manifestPath})
	if err == nil {
		t.Fatalf("stored connector create with failing attach succeeded; env=%+v stderr=%s", env, stderr)
	}
	if saveItemRequests != 1 || saveAttachmentRequests != 1 {
		t.Fatalf("connector requests = saveItems:%d saveAttachment:%d, want one each", saveItemRequests, saveAttachmentRequests)
	}
	if env.Result == nil || env.Result.Summary.Conflicts != 1 || len(env.Result.Items) != 1 {
		t.Fatalf("env = %+v, want one committed conflict", env)
	}
	reason, ok := env.Result.Items[0].Reason.(map[string]any)
	if !ok {
		t.Fatalf("reason = %#v, want a detail map identifying the orphaned parent", env.Result.Items[0].Reason)
	}
	if reason["via"] != "connector" {
		t.Fatalf("reason[via] = %v, want connector", reason["via"])
	}
	session, _ := reason["session"].(string)
	connKey, _ := reason["connector_key"].(string)
	if session == "" || connKey == "" {
		t.Fatalf("reason = %#v, want non-empty connector session and connector_key", reason)
	}
	if reason["title"] != "Orphan Paper" {
		t.Fatalf("reason[title] = %v, want %q", reason["title"], "Orphan Paper")
	}
	if reason["committed"] != true || reason["attachment_marker"] == "" || env.Result.Items[0].Status != "conflict" {
		t.Fatalf("reason = %#v, want committed conflict with attachment marker", reason)
	}
	message, _ := reason["message"].(string)
	// Both routes share one orphan contract, so the sentence cannot say "in
	// Zotero desktop": that is false for the Web API route, which creates the
	// parent server-side. Locating it is the remediation's job, below.
	if !strings.Contains(message, `"Orphan Paper" was created but the file was not attached`) {
		t.Fatalf("reason[message] = %q, want it to name the created-but-unattached condition", message)
	}
	if !strings.Contains(message, "Attach Stored Copy of File") || !strings.Contains(message, "delete the item and re-run") {
		t.Fatalf("reason[message] = %q, want a deterministic next step", message)
	}
	if !strings.Contains(message, "simulated WebDAV filing failure") {
		t.Fatalf("reason[message] = %q, want the underlying connector error wrapped in", message)
	}
	marker, _ := reason["attachment_marker"].(string)
	if markedURL := attachmentState.snapshot(); marker == "" || !attachmentURLHasMarker(markedURL, marker) {
		t.Fatalf("marked URL=%q marker=%q, want failed-attachment evidence retained", markedURL, marker)
	}
	entries, listErr := mutation.ListEntries(helpersTestJournalDir(t))
	if listErr != nil || len(entries) != 1 || len(entries[0].Ops) != 1 ||
		entries[0].Ops[0].Status != "conflict" {
		t.Fatalf("journal entries = %+v, err=%v; want committed attachment conflict", entries, listErr)
	}
}

func TestMarkedConnectorAttachmentURL(t *testing.T) {
	marked, marker, err := markedConnectorAttachmentURL(
		"https://doi.org/10.1234/example#page=2",
		"connector123",
	)
	if err != nil {
		t.Fatal(err)
	}
	if marker != "zotio-write-connector123" || !attachmentURLHasMarker(marked, marker) ||
		!strings.HasPrefix(marked, "https://doi.org/10.1234/example#page=2&") {
		t.Fatalf("marked URL=%q marker=%q", marked, marker)
	}
	if _, _, err := markedConnectorAttachmentURL("not-a-url", "connector123"); err == nil {
		t.Fatal("relative source URL accepted for connector marker")
	}
}

func TestImportApplyUploadModeIsInvalid(t *testing.T) {
	manifestPath := writeImportApplyTestManifest(t, importApplyTestManifest())
	_, _, err := runImportApplyTestCmd(t, []string{"--attach-mode", "upload", manifestPath})
	if err == nil || !strings.Contains(err.Error(), "--attach-mode must be one of none, linked-file, stored") {
		t.Fatalf("err = %v, want upload invalid error", err)
	}
}

func TestImportApplyStoredRejectsFetchPDF(t *testing.T) {
	manifestPath := writeImportApplyTestManifest(t, importApplyTestManifest())
	_, _, err := runImportApplyTestCmd(t, []string{"--attach-mode", "stored", "--fetch-pdf", manifestPath})
	if err == nil || !strings.Contains(err.Error(), "--fetch-pdf cannot be combined with --attach-mode stored") {
		t.Fatalf("err = %v, want redundant PDF sources rejected before writes", err)
	}
}

func TestImportApplyFetchPDFPlansCreateAndResolverSeparately(t *testing.T) {
	manifestPath := writeImportApplyTestManifest(t, importApplyTestManifest())
	env, stderr, err := runImportApplyTestCmd(t, []string{"--fetch-pdf", manifestPath})
	if err != nil {
		t.Fatalf("import apply --fetch-pdf preview: %v; stderr=%s", err, stderr)
	}
	if len(env.Plan.Operations) < 2 {
		t.Fatalf("plan = %+v, want separate parent and resolver operations", env.Plan)
	}
	if env.Plan.Operations[0].ID != "import.apply:001:create" ||
		env.Plan.Operations[1].ID != "import.apply:001:resolver-pdf" ||
		env.Plan.Operations[1].Kind != "attachment_create" {
		t.Fatalf("first operations = %+v, want parent create then conditional resolver attachment", env.Plan.Operations[:2])
	}
}

func TestImportApplyStoredRejectsMissingTitleBeforeRoute(t *testing.T) {
	manifest := importApplyTestManifest()
	manifest.Entries[0].Item["title"] = ""
	manifestPath := writeImportApplyTestManifest(t, manifest)
	_, _, err := runImportApplyTestCmd(t, []string{"--attach-mode", "stored", manifestPath})
	if err == nil || !strings.Contains(err.Error(), "stored create missing title") {
		t.Fatalf("err = %v, want missing title rejected before connector routing", err)
	}
}

func TestStoredConnectorCreateResultPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	status, detail, err := storedConnectorCreateResult(
		ctx,
		&rootFlags{},
		map[string]any{"itemType": "journalArticle", "title": "Cancelled Paper"},
		itemCreateResult{Via: "connector", Session: "session-1", ConnKey: "connector-1", CreatedAfter: time.Now()},
		"Cancelled Paper",
		"zotio:write:connector-1",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	)
	if status != "conflict" || !errors.Is(err, context.Canceled) {
		t.Fatalf("status=%q err=%v, want committed conflict with context cancellation", status, err)
	}
	if detail["committed"] != true || detail["title"] != "Cancelled Paper" ||
		detail["session"] != "session-1" || detail["connector_key"] != "connector-1" {
		t.Fatalf("detail = %#v, want recoverable committed-write evidence", detail)
	}
}

func TestLinkedFileAttachmentItem(t *testing.T) {
	path := filepath.Join(string(filepath.Separator), "tmp", "Paper.pdf")
	item := linkedFileAttachmentItem("PARENT1", path)
	want := map[string]any{
		"itemType":   "attachment",
		"linkMode":   "linked_file",
		"parentItem": "PARENT1",
		"title":      "Paper.pdf",
		"path":       path,

		"contentType": "application/pdf",
	}
	if len(item) != len(want) {
		t.Fatalf("item = %+v, want %+v", item, want)
	}
	for key, wantValue := range want {
		if item[key] != wantValue {
			t.Fatalf("item[%s] = %v, want %v; item=%+v", key, item[key], wantValue, item)
		}
	}
}
func TestOrphanedConnectorParentDetailOmitsUnprovenKey(t *testing.T) {
	detail := orphanedConnectorParentDetail(itemCreateResult{
		Via: "connector", WebKey: "UNPROVEN", Session: "session-1", ConnKey: "connector-1",
	}, "New Paper", errors.New("target filing failed"))
	if _, ok := detail["parent_key"]; ok {
		t.Fatalf("detail = %#v, must not claim a title-only match as the created parent", detail)
	}
	if detail["committed"] != true || detail["session"] != "session-1" || detail["connector_key"] != "connector-1" {
		t.Fatalf("detail = %#v, want committed connector evidence without a guessed key", detail)
	}
}

func TestCreatedItemKey(t *testing.T) {
	if key, ok := createdItemKey(json.RawMessage(`{"success":{"0":"KEY1"}}`)); !ok || key != "KEY1" {
		t.Fatalf("success key = %q, %v; want KEY1, true", key, ok)
	}
	if key, ok := createdItemKey(json.RawMessage(`{"successful":{"0":{"key":"KEY2"}}}`)); !ok || key != "KEY2" {
		t.Fatalf("successful key = %q, %v; want KEY2, true", key, ok)
	}
	if key, ok := createdItemKey(json.RawMessage(`{"failed":{"0":{"code":400}}}`)); ok || key != "" {
		t.Fatalf("failed key = %q, %v; want empty, false", key, ok)
	}
}

func importApplyTestManifest() importManifest {
	return importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           filepath.Join(string(filepath.Separator), "tmp"),
		Entries: []importManifestEntry{
			{
				Path:   filepath.Join(string(filepath.Separator), "tmp", "create.pdf"),
				Action: "create",
				Status: "resolved",
				Title:  "Created Paper",
				Item: map[string]any{
					"itemType": "journalArticle",
					"title":    "Created Paper",
				},
			},
			{
				Path:       filepath.Join(string(filepath.Separator), "tmp", "attach.pdf"),
				Action:     "attach",
				Status:     "resolved",
				MatchedKey: "MATCH1",
				Title:      "Attach Paper",
			},
			{
				Path:           filepath.Join(string(filepath.Separator), "tmp", "duplicate.pdf"),
				Classification: "duplicate",
				Action:         "skip",
				Status:         "resolved",
				MatchedKey:     "DUP1",
				Title:          "Duplicate Paper",
			},
		},
	}
}

func writeImportApplyTestManifest(t *testing.T, m importManifest) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create manifest: %v", err)
	}
	if err := writeImportManifest(f, m); err != nil {
		_ = f.Close()
		t.Fatalf("write manifest: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close manifest: %v", err)
	}
	return path
}

func runImportApplyTestCmd(t *testing.T, args []string) (mutation.Envelope, string, error) {
	t.Helper()
	return runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true}, args)
}

func runImportApplyTestCmdWithFlags(t *testing.T, flags *rootFlags, args []string) (mutation.Envelope, string, error) {
	t.Helper()
	cmd := newImportApplyCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	var errOut bytes.Buffer
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
