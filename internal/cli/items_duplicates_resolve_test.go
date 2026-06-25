// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean write-safety): cover duplicate resolve preview/apply plans.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"zotero-pp-cli/internal/store"
)

type duplicateResolveTestServer struct {
	server       *httptest.Server
	versions     map[string]int
	items        map[string]map[string]any
	patchBodies  map[string]map[string]any
	patchHeaders map[string]string
	patchCounts  map[string]int
}

func newDuplicateResolveTestServer(t *testing.T, versions map[string]int, items map[string]map[string]any) *duplicateResolveTestServer {
	t.Helper()
	ts := &duplicateResolveTestServer{
		versions:     versions,
		items:        items,
		patchBodies:  map[string]map[string]any{},
		patchHeaders: map[string]string{},
		patchCounts:  map[string]int{},
	}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for key, dataObj := range ts.items {
			if r.URL.Path != "/users/0/items/"+key {
				continue
			}
			switch r.Method {
			case http.MethodGet:
				version := ts.versions[key]
				w.Header().Set("Last-Modified-Version", fmt.Sprintf("%d", version))
				_ = json.NewEncoder(w).Encode(map[string]any{"key": key, "version": version, "data": dataObj})
			case http.MethodPatch:
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode patch body: %v", err)
				}
				ts.patchBodies[key] = body
				ts.patchHeaders[key] = r.Header.Get("If-Unmodified-Since-Version")
				ts.patchCounts[key]++
				for bodyKey, value := range body {
					dataObj[bodyKey] = value
				}
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			}
			return
		}
		http.Error(w, "unexpected path", http.StatusNotFound)
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func seedDuplicateResolveStore(t *testing.T, items []json.RawMessage) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotero-pp-cli"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func runItemsDuplicatesResolveTestCmd(t *testing.T, srv *duplicateResolveTestServer, flags *rootFlags, args ...string) mutationEnvelope {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	cmd := newItemsDuplicatesCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items duplicates %v: %v; stderr=%s", args, err, errOut.String())
	}
	var env mutationEnvelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode mutation envelope %q: %v", out.String(), err)
	}
	return env
}

func TestItemsDuplicatesResolvePreviewWritesNothing(t *testing.T) {
	seedDuplicateResolveStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":10,"data":{"key":"K1","itemType":"journalArticle","title":"Same","DOI":"10/example","collections":["C1"],"tags":[{"tag":"keep"}]}}`),
		json.RawMessage(`{"key":"K2","version":11,"data":{"key":"K2","itemType":"journalArticle","title":"Same","DOI":"10/example","collections":["C2"],"tags":[{"tag":"dup"}]}}`),
	})
	srv := newDuplicateResolveTestServer(t, map[string]int{"K1": 10, "K2": 11}, map[string]map[string]any{
		"K1": {"key": "K1", "itemType": "journalArticle", "title": "Same", "DOI": "10/example", "collections": []any{"C1"}, "tags": []any{map[string]any{"tag": "keep"}}},
		"K2": {"key": "K2", "itemType": "journalArticle", "title": "Same", "DOI": "10/example", "collections": []any{"C2"}, "tags": []any{map[string]any{"tag": "dup"}}},
	})

	env := runItemsDuplicatesResolveTestCmd(t, srv, &rootFlags{asJSON: true, maxChanges: -1}, "resolve", "--doi")
	if !env.OK || env.Mode != "preview" || env.Result != nil || env.Plan.Summary.Planned != 1 {
		t.Fatalf("env = %+v, want preview with one planned merge", env)
	}
	if len(env.Plan.Operations) != 1 || env.Plan.Operations[0].Kind != "duplicate_merge" || env.Plan.Operations[0].Key != "K2" {
		t.Fatalf("ops = %+v, want K2 duplicate_merge", env.Plan.Operations)
	}
	if !duplicateResolvePlanHasAdd(env.Plan.Operations[0].Changes, "collections", "C2") || !duplicateResolvePlanHasAdd(env.Plan.Operations[0].Changes, "deleted", float64(1)) {
		t.Errorf("changes = %+v, want collection merge and trash note", env.Plan.Operations[0].Changes)
	}
	if srv.patchCounts["K1"] != 0 || srv.patchCounts["K2"] != 0 {
		t.Fatalf("preview PATCH counts = master %d dup %d, want 0", srv.patchCounts["K1"], srv.patchCounts["K2"])
	}
}

func TestItemsDuplicatesResolveApplyMergesCollectionsAndTrashesDuplicate(t *testing.T) {
	seedDuplicateResolveStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":10,"data":{"key":"K1","itemType":"journalArticle","title":"Same","DOI":"10/example","abstractNote":"present","collections":["C1"],"tags":[{"tag":"keep"}]}}`),
		json.RawMessage(`{"key":"K2","version":11,"data":{"key":"K2","itemType":"journalArticle","title":"Same","DOI":"10/example","collections":["C2"],"tags":[{"tag":"dup"}]}}`),
	})
	srv := newDuplicateResolveTestServer(t, map[string]int{"K1": 10, "K2": 11}, map[string]map[string]any{
		"K1": {"key": "K1", "itemType": "journalArticle", "title": "Same", "DOI": "10/example", "abstractNote": "present", "collections": []any{"C1"}, "tags": []any{map[string]any{"tag": "keep"}}},
		"K2": {"key": "K2", "itemType": "journalArticle", "title": "Same", "DOI": "10/example", "collections": []any{"C2"}, "tags": []any{map[string]any{"tag": "dup"}}},
	})

	env := runItemsDuplicatesResolveTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "resolve", "--doi")
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want one applied merge", env)
	}
	if srv.patchCounts["K1"] != 1 || srv.patchCounts["K2"] != 1 {
		t.Fatalf("PATCH counts = master %d dup %d, want 1 each", srv.patchCounts["K1"], srv.patchCounts["K2"])
	}
	if srv.patchHeaders["K1"] != "10" || srv.patchHeaders["K2"] != "11" {
		t.Fatalf("headers = master %q dup %q, want 10/11", srv.patchHeaders["K1"], srv.patchHeaders["K2"])
	}
	if !duplicateResolveBodyHasString(srv.patchBodies["K1"], "collections", "C1") || !duplicateResolveBodyHasString(srv.patchBodies["K1"], "collections", "C2") {
		t.Errorf("master PATCH collections = %+v, want C1+C2", srv.patchBodies["K1"])
	}
	if !duplicateResolveBodyHasTag(srv.patchBodies["K1"], "keep") || !duplicateResolveBodyHasTag(srv.patchBodies["K1"], "dup") {
		t.Errorf("master PATCH tags = %+v, want keep+dup", srv.patchBodies["K1"])
	}
	if srv.patchBodies["K2"]["deleted"] != float64(1) {
		t.Errorf("dup PATCH body = %+v, want deleted=1", srv.patchBodies["K2"])
	}
}

func TestItemsDuplicatesResolveNoDuplicatesEmptyPlan(t *testing.T) {
	seedDuplicateResolveStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":1,"data":{"key":"K1","itemType":"journalArticle","title":"One","DOI":"10/one"}}`),
		json.RawMessage(`{"key":"K2","version":2,"data":{"key":"K2","itemType":"journalArticle","title":"Two","DOI":"10/two"}}`),
	})
	srv := newDuplicateResolveTestServer(t, map[string]int{}, map[string]map[string]any{})

	env := runItemsDuplicatesResolveTestCmd(t, srv, &rootFlags{asJSON: true, maxChanges: -1}, "resolve")
	if !env.OK || env.Mode != "preview" || env.Plan.Summary.Selected != 0 || env.Plan.Summary.Planned != 0 || len(env.Plan.Operations) != 0 {
		t.Fatalf("env = %+v, want empty preview plan", env)
	}
}

func duplicateResolvePlanHasAdd(changes []mutationChange, field string, want any) bool {
	for _, change := range changes {
		if change.Field != field {
			continue
		}
		if fmt.Sprint(change.Add) == fmt.Sprint(want) {
			return true
		}
		values, ok := change.Add.([]any)
		if !ok {
			continue
		}
		for _, value := range values {
			if fmt.Sprint(value) == fmt.Sprint(want) {
				return true
			}
		}
	}
	return false
}

func duplicateResolveBodyHasString(body map[string]any, field string, want string) bool {
	values, ok := body[field].([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		if got, ok := value.(string); ok && got == want {
			return true
		}
	}
	return false
}

func duplicateResolveBodyHasTag(body map[string]any, want string) bool {
	values, ok := body["tags"].([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		tagObj, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if got, ok := tagObj["tag"].(string); ok && got == want {
			return true
		}
	}
	return false
}
