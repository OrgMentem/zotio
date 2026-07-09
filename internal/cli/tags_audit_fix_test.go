// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

func seedTagsAuditFixStore(t *testing.T, items []json.RawMessage) {
	t.Helper()
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotio"))
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
	patches := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
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
	if len(patches) != 2 {
		t.Fatalf("patches=%d, want 2", len(patches))
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

func largeTagAuditItems() []json.RawMessage {
	items := make([]json.RawMessage, 0, 901)
	for i := range 301 {
		key := fmt.Sprintf("C%03d", i)
		items = append(items, json.RawMessage(fmt.Sprintf(`{"key":%q,"version":1,"data":{"key":%q,"tags":[{"tag":"Data Science","type":0}]}}`, key, key)))
	}
	for i := range 300 {
		key := fmt.Sprintf("A%03d", i)
		items = append(items, json.RawMessage(fmt.Sprintf(`{"key":%q,"version":2,"data":{"key":%q,"tags":[{"tag":"data science","type":0}]}}`, key, key)))
	}
	for i := range 300 {
		key := fmt.Sprintf("B%03d", i)
		items = append(items, json.RawMessage(fmt.Sprintf(`{"key":%q,"version":3,"data":{"key":%q,"tags":[{"tag":"Data  Science","type":0}]}}`, key, key)))
	}
	return items
}

func TestTagsAuditFixMaxChangesCountsItemWrites(t *testing.T) {
	seedTagsAuditFixStore(t, largeTagAuditItems())
	patches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "unexpected request", http.StatusMethodNotAllowed)
			return
		}
		patches++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	refused, refusedJSON, err := runTagsAuditFixCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: 500}, srv.URL)
	if err == nil {
		t.Fatal("tags audit fix apply succeeded, want max_changes_exceeded error")
	}
	if refused.OK || refused.Error == nil || refused.Error.Code != "max_changes_exceeded" {
		t.Fatalf("refused envelope = %+v, want max_changes_exceeded", refused)
	}
	if refused.Plan.Summary.Planned != 600 || len(refused.Plan.Operations) != 600 {
		t.Fatalf("refused plan = summary %+v len %d, want 600 item writes", refused.Plan.Summary, len(refused.Plan.Operations))
	}
	if !bytes.Contains([]byte(refusedJSON), []byte(`"planned": 600`)) {
		t.Fatalf("preview JSON %q does not include planned item-write count 600", refusedJSON)
	}
	if patches != 0 {
		t.Fatalf("refused apply made %d PATCH request(s), want 0", patches)
	}

	seedTagsAuditFixStore(t, largeTagAuditItems())
	applied, _, err := runTagsAuditFixCmd(t, &rootFlags{asJSON: true, yes: true, maxChanges: 600}, srv.URL)
	if err != nil {
		t.Fatalf("tags audit fix apply at cap: %v", err)
	}
	if !applied.OK || applied.Result == nil || applied.Plan.Summary.Planned != 600 || applied.Result.Summary.Applied != 600 {
		t.Fatalf("applied envelope = %+v, want 600 planned/applied item writes", applied)
	}
	if patches != 600 {
		t.Fatalf("PATCH requests after allowed apply = %d, want 600", patches)
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
