// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"zotio/internal/mutation"
)

// Retrying a manifest create after a lost response must report a committed
// conflict, not a plain failed create: the first POST may already have
// committed the item, and a second identical POST would duplicate it.
func TestImportApplyLostCreateResponseReportsCommittedConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/0/items" && r.Method == http.MethodPost {
			if r.Header.Get("Zotero-Write-Token") == "" {
				t.Error("create POST arrived without Zotero-Write-Token; the retry cannot replay the same token")
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test server does not support hijacking")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/users/0")
		if r.Method == http.MethodGet && path == "/items/new" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"itemType":"journalArticle","title":"","creators":[],"date":"","DOI":"","publicationTitle":""}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	m := importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           "/tmp",
		Entries: []importManifestEntry{{
			Action: "create",
			Status: "resolved",
			Title:  "Created Paper",
			Item:   map[string]any{"itemType": "journalArticle", "title": "Created Paper"},
		}},
	}
	manifestPath := writeImportApplyTestManifest(t, m)
	env, _, err := runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true, via: "web", yes: true, maxChanges: -1}, []string{manifestPath})
	if err == nil {
		t.Fatal("import apply error = nil, want committed-conflict refusal")
	}
	// The engine reports per-op outcomes in the envelope; the top-level error
	// is the generic "mutation incomplete" wrapper.
	if env.Result == nil || len(env.Result.Items) != 1 || env.Result.Items[0].Status != "conflict" {
		t.Fatalf("result = %+v, want one committed conflict", env.Result)
	}
	detail, _ := env.Result.Items[0].Reason.(map[string]any)
	msg, _ := detail["message"].(string)
	if !strings.Contains(msg, "refusing automatic retry") {
		t.Fatalf("conflict reason = %v, want the refusal message", env.Result.Items[0].Reason)
	}
	if detail["committed"] != true {
		t.Fatalf("conflict lost possible-write evidence: %v", detail)
	}
	if journal, ok := env.Journal.(map[string]any); !ok || journal["run_id"] == nil {
		t.Fatalf("ambiguous create has no journal entry: %v", env.Journal)
	}
}

// A manifest create answered 412 on its replayed write token proves the first
// attempt already committed, so the retry must refuse instead of posting again.
func TestImportApplyReplayedWriteTokenRefusesDuplicate(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/0/items" && r.Method == http.MethodPost {
			posts++
			if posts == 1 {
				hj, _ := w.(http.Hijacker)
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
				return
			}
			if r.Header.Get("Zotero-Write-Token") == "" {
				t.Error("retry POST arrived without Zotero-Write-Token")
			}
			http.Error(w, `{"error":"write token already submitted"}`, http.StatusPreconditionFailed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/users/0")
		if r.Method == http.MethodGet && path == "/items/new" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"itemType":"journalArticle","title":"","creators":[],"date":"","DOI":"","publicationTitle":""}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	m := importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           "/tmp",
		Entries: []importManifestEntry{{
			Action: "create",
			Status: "resolved",
			Title:  "Created Paper",
			Item:   map[string]any{"itemType": "journalArticle", "title": "Created Paper"},
		}},
	}
	manifestPath := writeImportApplyTestManifest(t, m)
	env, _, err := runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true, via: "web", yes: true, maxChanges: -1}, []string{manifestPath})
	if err == nil {
		t.Fatal("import apply error = nil, want committed-conflict refusal")
	}
	// The engine reports per-op outcomes in the envelope; the top-level error
	// is the generic "mutation incomplete" wrapper.
	if env.Result == nil || len(env.Result.Items) != 1 || env.Result.Items[0].Status != "conflict" {
		t.Fatalf("result = %+v, want one committed conflict", env.Result)
	}
	detail, _ := env.Result.Items[0].Reason.(map[string]any)
	msg, _ := detail["message"].(string)
	if !strings.Contains(msg, "refusing automatic retry") {
		t.Fatalf("conflict reason = %v, want the refusal message", env.Result.Items[0].Reason)
	}
}

// The stored Web route used to POST the parent without a write token, so a
// lost response and a rerun of the same manifest created a second parent. The
// parent now goes out under the entry's deterministic token, the lost response
// is a committed conflict, and no attachment work starts without a parent key.
func TestImportApplyStoredWebLostParentResponseReplaysSameToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })
	var tokens []string
	var attachmentPosts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/0/items" && r.Method == http.MethodPost {
			token := r.Header.Get("Zotero-Write-Token")
			tokens = append(tokens, token)
			if token == "" {
				t.Error("stored Web parent POST arrived without Zotero-Write-Token")
			}
			if len(tokens) == 1 {
				// Zotero commits the parent, then the response is lost.
				hj, _ := w.(http.Hijacker)
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
				return
			}
			http.Error(w, `{"error":"write token already submitted"}`, http.StatusPreconditionFailed)
			return
		}
		if strings.Contains(r.URL.Path, "/file") || strings.Contains(r.URL.Path, "/children") {
			attachmentPosts.Add(1)
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	pdf := writeUploadFixture(t, "stored-web.pdf", []byte("%PDF-1.4\nstored web\n%%EOF"))
	manifestPath := writeImportApplyTestManifest(t, importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           filepath.Dir(pdf),
		Entries: []importManifestEntry{{
			Path: pdf, Action: "create", Status: "resolved", Title: "Stored Web Paper",
			Item: map[string]any{"itemType": "journalArticle", "title": "Stored Web Paper"},
		}},
	})
	for run := 1; run <= 2; run++ {
		env, _, err := runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true, via: "web", yes: true, maxChanges: -1},
			[]string{"--attach-mode", "stored", manifestPath})
		if err == nil || env.Result == nil || len(env.Result.Items) != 1 || env.Result.Items[0].Status != "conflict" {
			t.Fatalf("run %d: result = %+v err=%v, want one committed conflict", run, env.Result, err)
		}
		detail, _ := env.Result.Items[0].Reason.(map[string]any)
		if detail["committed"] != true || !strings.Contains(fmt.Sprint(detail["message"]), "refusing automatic retry") {
			t.Fatalf("run %d: reason = %#v, want committed refusal evidence", run, env.Result.Items[0].Reason)
		}
		if journal, ok := env.Journal.(map[string]any); !ok || journal["run_id"] == nil {
			t.Fatalf("run %d: ambiguous stored parent has no journal entry: %v", run, env.Journal)
		}
	}
	// The client may itself replay the dropped POST; every send, and every
	// rerun, must carry the same token.
	if len(tokens) < 3 {
		t.Fatalf("tokens = %q, want the first run and the rerun both to POST", tokens)
	}
	for _, token := range tokens[1:] {
		if token != tokens[0] {
			t.Fatalf("tokens = %q, want the same token replayed on every send and rerun", tokens)
		}
	}
	if attachmentPosts.Load() != 0 {
		t.Fatalf("attachment requests = %d, want none without a confirmed parent", attachmentPosts.Load())
	}
}

// An items create batch lost in transit must report a committed conflict with
// the deterministic write token attached, not a plain transport failure.
func TestItemsCreateLostBatchResponseReportsCommittedConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })
	var gotToken atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken.Store(r.Header.Get("Zotero-Write-Token") != "")
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	cmd := newItemsCreateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetErr(io.Discard)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--items", `[{"itemType":"journalArticle","title":"a"},{"itemType":"journalArticle","title":"b"}]`})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "refusing automatic retry") {
		t.Fatalf("items create error = %v, want the committed-conflict refusal; stdout=%s", err, out.String())
	}
	if !gotToken.Load() {
		t.Fatal("create POST arrived without Zotero-Write-Token; the retry cannot replay the same token")
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not a mutation envelope: %v; stdout=%q", err, out.String())
	}
	if env.Result == nil || env.Result.Summary.Conflicts != 2 || env.Result.Summary.Applied != 0 {
		t.Fatalf("result = %s, want 2 committed conflicts", out.String())
	}
	for _, item := range env.Result.Items {
		detail, _ := item.Reason.(map[string]any)
		if detail["committed"] != true {
			t.Fatalf("conflict lost possible-write evidence: %v", detail)
		}
	}
	if journal, ok := env.Journal.(map[string]any); !ok || journal["run_id"] == nil {
		t.Fatalf("ambiguous batch has no journal entry: %v", env.Journal)
	}
}

// Re-applying a manifest must not stack a second linked_file child on the same
// parent and path: the second apply reports the existing attachment as a no-op
// and never POSTs, including after the first POST's response was lost.
func TestImportApplyLinkedFileAttachIsIdempotent(t *testing.T) {
	for _, dropFirstResponse := range []bool{false, true} {
		t.Run(fmt.Sprintf("drop_first_response=%t", dropFirstResponse), func(t *testing.T) {
			fastRetryBackoff(t)
			var mu sync.Mutex
			var posts int
			tokens := map[string]bool{}
			var children []map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/users/0/items/MATCH1/children":
					_ = json.NewEncoder(w).Encode(children)
				case r.Method == http.MethodPost && r.URL.Path == "/users/0/items":
					posts++
					token := r.Header.Get("Zotero-Write-Token")
					if token == "" {
						t.Error("linked-file POST arrived without Zotero-Write-Token")
					}
					if tokens[token] {
						http.Error(w, `{"error":"write token already submitted"}`, http.StatusPreconditionFailed)
						return
					}
					tokens[token] = true
					var body []map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 {
						http.Error(w, "bad body", http.StatusBadRequest)
						return
					}
					children = append(children, map[string]any{"key": "CHILD001", "data": map[string]any{
						"key": "CHILD001", "itemType": "attachment", "linkMode": "linked_file", "path": body[0]["path"],
					}})
					if dropFirstResponse && posts == 1 {
						hj, _ := w.(http.Hijacker)
						conn, _, _ := hj.Hijack()
						_ = conn.Close()
						return
					}
					_, _ = fmt.Fprint(w, `{"success":{"0":"CHILD001"}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

			pdf := writeUploadFixture(t, "linked.pdf", []byte("%PDF-1.4\nlinked\n%%EOF"))
			manifestPath := writeImportApplyTestManifest(t, importManifest{
				SchemaVersion: importManifestSchemaVersion,
				Dir:           filepath.Dir(pdf),
				Entries: []importManifestEntry{{
					Path: pdf, Action: "attach", Status: "resolved", MatchedKey: "MATCH1", Title: "Linked",
				}},
			})
			var statuses []string
			for run := 1; run <= 2; run++ {
				env, _, err := runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true, via: "web", yes: true, maxChanges: -1},
					[]string{"--attach-mode", "linked-file", manifestPath})
				if err != nil || env.Result == nil || len(env.Result.Items) != 1 {
					t.Fatalf("run %d: result=%+v err=%v", run, env.Result, err)
				}
				statuses = append(statuses, env.Result.Items[0].Status)
				if run == 2 {
					reason, _ := env.Result.Items[0].Reason.(map[string]any)
					if env.Result.Items[0].Status != "no_op" || reason["attachment_key"] != "CHILD001" {
						t.Fatalf("second apply = %q %#v, want no_op naming the existing attachment", env.Result.Items[0].Status, reason)
					}
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if len(children) != 1 {
				t.Fatalf("children = %d after two applies (statuses %v), want exactly one linked attachment", len(children), statuses)
			}
			if !dropFirstResponse && posts != 1 {
				t.Fatalf("POST /items count = %d, want 1 across both applies", posts)
			}
		})
	}
}
