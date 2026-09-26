// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"zotio/internal/mutation"
)

// Retrying a manifest create after a lost response must report a committed
// conflict, not a plain failed create: the first POST may already have
// committed the item, and a second identical POST would duplicate it.
func TestImportApplyLostCreateResponseReportsCommittedConflict(t *testing.T) {
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

// An items create batch lost in transit must report a committed conflict with
// the deterministic write token attached, not a plain transport failure.
func TestItemsCreateLostBatchResponseReportsCommittedConflict(t *testing.T) {
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
}
