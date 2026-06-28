// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean write-safety): cover tags audit fix preview/apply mutation envelopes.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"zotero-pp-cli/internal/mutation"
	"zotero-pp-cli/internal/store"
)

func seedTagsAuditFixStore(t *testing.T, items []json.RawMessage) {
	t.Helper()
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotero-pp-cli"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		_ = db.Close()
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func duplicateTagAuditItems() []json.RawMessage {
	return []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":1,"data":{"key":"K1","tags":[{"tag":"Data Science","type":0}]}}`),
		json.RawMessage(`{"key":"K2","version":2,"data":{"key":"K2","tags":[{"tag":"Data Science","type":0},{"tag":"data science","type":0}]}}`),
		json.RawMessage(`{"key":"K3","version":3,"data":{"key":"K3","tags":[{"tag":"Data  Science","type":0}]}}`),
	}
}

func runTagsAuditFixCmd(t *testing.T, flags *rootFlags, baseURL string) (mutation.Envelope, string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", baseURL+"/users/0")
	cmd := newTagsAuditCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"fix"})
	err := cmd.Execute()
	var env mutation.Envelope
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode envelope %q: %v", out.String(), decodeErr)
		}
	}
	return env, out.String(), err
}

func TestTagsAuditFixPreviewPlansRenamesWithoutWrites(t *testing.T) {
	seedTagsAuditFixStore(t, duplicateTagAuditItems())
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "preview must not call Zotero", http.StatusInternalServerError)
	}))
	defer srv.Close()

	env, _, err := runTagsAuditFixCmd(t, &rootFlags{asJSON: true}, srv.URL)
	if err != nil {
		t.Fatalf("tags audit fix preview: %v", err)
	}
	if requests != 0 {
		t.Fatalf("preview made %d Zotero request(s), want 0", requests)
	}
	if !env.OK || env.Mode != "preview" || env.PreviewReason != "default" || env.Result != nil {
		t.Fatalf("preview envelope = %+v, want ok preview without result", env)
	}
	if env.Plan.Summary.Planned != 2 || len(env.Plan.Operations) != 2 {
		t.Fatalf("planned ops = summary %+v len %d, want 2", env.Plan.Summary, len(env.Plan.Operations))
	}
	for _, op := range env.Plan.Operations {
		if op.Kind != "tag_rename" || op.Destructive {
			t.Errorf("op = %+v, want non-destructive tag_rename", op)
		}
		if len(op.Changes) != 1 || op.Changes[0].Field != "tag" || op.Changes[0].Add != "Data Science" {
			t.Errorf("changes = %+v, want tag rename to canonical Data Science", op.Changes)
		}
	}
}

func TestTagsAuditFixApplyRenamesEachAlias(t *testing.T) {
	seedTagsAuditFixStore(t, duplicateTagAuditItems())
	getCount := 0
	patches := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getCount++
			if r.URL.Path != "/users/0/items" {
				http.NotFound(w, r)
				return
			}
			switch r.URL.Query().Get("tag") {
			case "Data  Science":
				_, _ = w.Write([]byte(`[{"key":"K3","version":7,"data":{"key":"K3","tags":[{"tag":"Data  Science","type":0},{"tag":"other","type":0}]}}]`))
			case "data science":
				_, _ = w.Write([]byte(`[{"key":"K2","version":8,"data":{"key":"K2","tags":[{"tag":"Data Science","type":0},{"tag":"data science","type":0}]}}]`))
			default:
				http.Error(w, "unexpected tag query", http.StatusBadRequest)
			}
		case http.MethodPatch:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			patches[r.URL.Path] = body
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	env, _, err := runTagsAuditFixCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, srv.URL)
	if err != nil {
		t.Fatalf("tags audit fix apply: %v", err)
	}
	if getCount != 2 || len(patches) != 2 {
		t.Fatalf("GETs=%d patches=%d, want 2 each", getCount, len(patches))
	}
	for _, path := range []string{"/users/0/items/K2", "/users/0/items/K3"} {
		body, ok := patches[path]
		if !ok {
			t.Fatalf("missing PATCH for %s; got %#v", path, patches)
		}
		tags, ok := body["tags"].([]any)
		if !ok || len(tags) == 0 {
			t.Fatalf("PATCH %s tags = %#v, want non-empty array", path, body["tags"])
		}
		for _, raw := range tags {
			tag, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("PATCH %s tag entry = %#v", path, raw)
			}
			if tag["tag"] == "Data  Science" || tag["tag"] == "data science" {
				t.Fatalf("PATCH %s left alias tag in %#v", path, tags)
			}
		}
	}
	if !env.OK || env.Mode != "apply" || env.Result == nil {
		t.Fatalf("apply envelope = %+v, want ok apply with result", env)
	}
	if env.Result.Summary.Applied != 2 || len(env.Result.Items) != 2 {
		t.Fatalf("apply result = %+v, want two applied", env.Result)
	}
	for _, item := range env.Result.Items {
		if item.Status != "applied" {
			t.Errorf("result item = %+v, want applied", item)
		}
	}
}

func TestTagsAuditFixNoAliasesIsEmptyPlan(t *testing.T) {
	seedTagsAuditFixStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":1,"data":{"key":"K1","tags":[{"tag":"Solo","type":0}]}}`),
		json.RawMessage(`{"key":"K2","version":2,"data":{"key":"K2","tags":[{"tag":"Unique","type":0}]}}`),
	})
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "empty apply must not call Zotero", http.StatusInternalServerError)
	}))
	defer srv.Close()

	env, _, err := runTagsAuditFixCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, srv.URL)
	if err != nil {
		t.Fatalf("tags audit fix empty apply: %v", err)
	}
	if requests != 0 {
		t.Fatalf("empty plan made %d Zotero request(s), want 0", requests)
	}
	if !env.OK || env.Mode != "apply" || env.Plan.Summary.Planned != 0 || len(env.Plan.Operations) != 0 {
		t.Fatalf("empty envelope = %+v, want ok apply with no ops", env)
	}
	if env.Result == nil || env.Result.Summary.Attempted != 0 || len(env.Result.Items) != 0 {
		t.Fatalf("empty result = %+v, want no attempted items", env.Result)
	}
}
