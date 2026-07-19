// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

func TestApplyChangeToItemData(t *testing.T) {
	t.Run("scalar set", func(t *testing.T) {
		d := map[string]any{"DOI": ""}
		if !applyChangeToItemData(d, mutation.Change{Field: "DOI", Add: "10.1/x"}) || d["DOI"] != "10.1/x" {
			t.Errorf("scalar set failed: %v", d)
		}
	})
	t.Run("scalar clear", func(t *testing.T) {
		d := map[string]any{"DOI": "10.1/x"}
		if !applyChangeToItemData(d, mutation.Change{Field: "DOI", Remove: "10.1/x"}) || d["DOI"] != "" {
			t.Errorf("scalar clear failed: %v", d)
		}
	})
	t.Run("tag add then remove", func(t *testing.T) {
		d := map[string]any{}
		applyChangeToItemData(d, mutation.Change{Field: "tags", Add: "ml"})
		tags, _ := d["tags"].([]any)
		if len(tags) != 1 || tags[0].(map[string]any)["tag"] != "ml" {
			t.Fatalf("tag add: %v", d["tags"])
		}
		applyChangeToItemData(d, mutation.Change{Field: "tags", Remove: "ml"})
		if tags, _ := d["tags"].([]any); len(tags) != 0 {
			t.Errorf("tag remove: %v", d["tags"])
		}
	})
	t.Run("collection add", func(t *testing.T) {
		d := map[string]any{"collections": []any{"C1"}}
		applyChangeToItemData(d, mutation.Change{Field: "collections", Add: "C2"})
		if cols, _ := d["collections"].([]any); len(cols) != 2 {
			t.Errorf("collection add: %v", d["collections"])
		}
	})
	t.Run("unsupported bulk and trash refused", func(t *testing.T) {
		if applyChangeToItemData(map[string]any{}, mutation.Change{Field: "collections", Add: []string{"X"}}) {
			t.Error("bulk []string collection add should be unsupported")
		}
		if applyChangeToItemData(map[string]any{}, mutation.Change{Field: "deleted", Add: 1}) {
			t.Error("trash (deleted=1, non-string) should be unsupported")
		}
	})
}

func TestRunMutationReadsYourWritesLocally(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Seed the mirror with an item whose DOI is empty.
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Paper","DOI":"","version":1,"dateModified":"2026-01-01T00:00:00Z"}}`),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = db.Close()

	// Install the write-through hook (as Execute does) for this test only.
	mirrorWriteThrough = applyMirrorWriteThrough
	t.Cleanup(func() { mirrorWriteThrough = nil })

	// A mutation whose apply closure "succeeds" without any network: write-through
	// replays the recorded DOI change onto the mirror.
	ops := []mutation.Op{{
		ID: "items.enrich:P1", Key: "P1", Kind: "enrich",
		Changes: []mutation.Change{{Field: "DOI", Add: "10.1/applied"}},
		Apply:   func() (string, any, error) { return "applied", nil, nil },
	}}
	env, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "items.enrich", ops)
	if err != nil {
		t.Fatalf("runMutation: %v", err)
	}

	// 1) Post-write state surfaced in the envelope (read-your-writes for agents).
	if env.Result == nil || len(env.Result.Items) != 1 || env.Result.Items[0].Item == nil {
		t.Fatalf("expected an applied result item with post-write Item, got %+v", env.Result)
	}
	gotData, _ := env.Result.Items[0].Item["data"].(map[string]any)
	if gotData["DOI"] != "10.1/applied" {
		t.Errorf("envelope item DOI = %v, want 10.1/applied", gotData["DOI"])
	}

	// The advanced Web API version is not available here, so write-through must
	// strip stale pre-write version metadata.
	if _, ok := env.Result.Items[0].Item["version"]; ok {
		t.Errorf("envelope item still exposes stale top-level version: %v", env.Result.Items[0].Item["version"])
	}
	if _, ok := gotData["version"]; ok {
		t.Errorf("envelope item data still exposes stale version: %v", gotData["version"])
	}
	if _, ok := gotData["dateModified"]; ok {
		t.Errorf("envelope item data still exposes stale dateModified: %v", gotData["dateModified"])
	}

	// 2) The local mirror reflects the write WITHOUT any sync.
	db2, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db2.Close()
	rows, err := (localQueryStore{db2}).QueryRaw("SELECT json_extract(data,'$.data.DOI') AS doi FROM resources WHERE resource_type='items' AND id='P1'")
	if err != nil || len(rows) != 1 {
		t.Fatalf("read back: rows=%v err=%v", rows, err)
	}
	if got := sqlStringValue(rows[0]["doi"]); got != "10.1/applied" {
		t.Errorf("mirror DOI after write (no sync) = %q, want 10.1/applied", got)
	}
}

// TestApplyMirrorWriteThroughSkipsNonAppliedStatuses covers degraded mirror
// updates and branches that must not mutate or surface replayed items.
func TestApplyMirrorWriteThroughSkipsNonAppliedStatuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedWriteThroughItem(t, "P1", `{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Paper","DOI":""}}`)

	env := mutation.Envelope{
		Plan: mutation.Plan{Operations: []mutation.Op{
			{ID: "failed", Key: "P1", Changes: []mutation.Change{{Field: "DOI", Add: "failed"}}},
			{ID: "skipped", Key: "P1", Changes: []mutation.Change{{Field: "DOI", Add: "skipped"}}},
			{ID: "conflict", Key: "P1", Changes: []mutation.Change{{Field: "DOI", Add: "conflict"}}},
			{ID: "noop", Key: "P1", Changes: []mutation.Change{{Field: "DOI", Add: "noop"}}},
		}},
		Result: &mutation.Result{Items: []mutation.ResultItem{
			{OpID: "failed", Key: "P1", Status: "failed"},
			{OpID: "skipped", Key: "P1", Status: "skipped"},
			{OpID: "conflict", Key: "P1", Status: "conflict"},
			{OpID: "noop", Key: "P1", Status: "no_op"},
		}},
	}
	applyMirrorWriteThrough(&env)

	if got := writeThroughItemField(t, "P1", "$.data.DOI"); got != "" {
		t.Fatalf("non-applied statuses mutated mirror DOI to %q", got)
	}
	for _, it := range env.Result.Items {
		if it.Item != nil {
			t.Fatalf("non-applied result %s unexpectedly got item: %v", it.Status, it.Item)
		}
	}
}

func TestApplyMirrorWriteThroughDryRunNoOp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedWriteThroughItem(t, "P1", `{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Paper","DOI":""}}`)

	env := mutation.Envelope{
		Plan: mutation.Plan{Operations: []mutation.Op{{
			ID: "op", Key: "P1", Changes: []mutation.Change{{Field: "DOI", Add: "10.1/preview"}},
		}}},
		Result: nil,
	}
	applyMirrorWriteThrough(&env)

	if got := writeThroughItemField(t, "P1", "$.data.DOI"); got != "" {
		t.Fatalf("dry-run mirror DOI = %q, want unchanged empty string", got)
	}
}

func TestApplyMirrorWriteThroughCreateSkipsMissingMirrorItem(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_ = db.Close()

	env := mutation.Envelope{
		Plan: mutation.Plan{Operations: []mutation.Op{{
			ID: "create", Key: "NEW1", Changes: []mutation.Change{{Field: "title", Add: "New"}},
		}}},
		Result: &mutation.Result{Items: []mutation.ResultItem{{OpID: "create", Key: "NEW1", Status: "applied"}}},
	}
	applyMirrorWriteThrough(&env)

	if env.Result.Items[0].Item != nil {
		t.Fatalf("create/missing mirror item unexpectedly surfaced item: %v", env.Result.Items[0].Item)
	}
	db2, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db2.Close()
	rows, err := (localQueryStore{db2}).QueryRaw("SELECT id FROM resources WHERE resource_type='items' AND id='NEW1'")
	if err != nil {
		t.Fatalf("query create row: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("create/missing mirror item inserted rows: %v", rows)
	}
}

func TestApplyMirrorWriteThroughWarnsOnMirrorOpenFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}
	if err := os.Mkdir(dbPath, 0o755); err != nil {
		t.Fatalf("make db path directory: %v", err)
	}

	env := mutation.Envelope{
		Plan: mutation.Plan{Operations: []mutation.Op{{
			ID: "op", Key: "P1", Changes: []mutation.Change{{Field: "DOI", Add: "10.1/applied"}},
		}}},
		Result: &mutation.Result{Items: []mutation.ResultItem{{OpID: "op", Key: "P1", Status: "applied"}}},
	}
	stderr := captureWriteThroughStderr(t, func() {
		applyMirrorWriteThrough(&env)
	})

	if !strings.Contains(stderr, "warning: read-your-writes mirror update failed for P1:") {
		t.Fatalf("stderr %q does not contain read-your-writes warning", stderr)
	}
	if env.Result.Items[0].Item != nil {
		t.Fatalf("failed mirror open unexpectedly surfaced item: %v", env.Result.Items[0].Item)
	}
}

func seedWriteThroughItem(t *testing.T, key, raw string) {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{json.RawMessage(raw)}); err != nil {
		_ = db.Close()
		t.Fatalf("seed %s: %v", key, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}
}

func writeThroughItemField(t *testing.T, key, jsonPath string) string {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	rows, err := (localQueryStore{db}).QueryRaw("SELECT json_extract(data, ?) AS value FROM resources WHERE resource_type='items' AND id=?", jsonPath, key)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read %s %s: rows=%v err=%v", key, jsonPath, rows, err)
	}
	return sqlStringValue(rows[0]["value"])
}

func captureWriteThroughStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = old })

	fn()

	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	_ = r.Close()
	os.Stderr = old
	return string(out)
}
