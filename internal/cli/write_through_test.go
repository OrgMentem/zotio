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
	t.Run("automatic tag add preserves type", func(t *testing.T) {
		d := map[string]any{}
		applyChangeToItemData(d, mutation.Change{Field: "tags", Add: "papio:unavailable", TagType: 1})
		tags, _ := d["tags"].([]any)
		if len(tags) != 1 || tags[0].(map[string]any)["type"] != 1 {
			t.Fatalf("automatic tag add: %v", d["tags"])
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

// TestMirrorSkipsUnknownFieldNames covers the fail-open regression: producers
// have shipped Changes naming fields that were never real Zotero item fields
// (a rename that emits "tag" singular instead of "tags", a generic "record"
// or "note" placeholder, a scoped-wrong "attachment" label). None of these
// belong in the mirrored item's data map, so applyChangeToItemData must
// refuse to replay them instead of injecting a bogus key.
func TestMirrorSkipsUnknownFieldNames(t *testing.T) {
	for _, field := range []string{"tag", "tags_set", "record", "note", "attachment"} {
		t.Run(field, func(t *testing.T) {
			d := map[string]any{}
			if applyChangeToItemData(d, mutation.Change{Field: field, Add: "x"}) {
				t.Fatalf("unrecognized field %q should not replay", field)
			}
			if _, present := d[field]; present {
				t.Fatalf("unrecognized field %q was injected into the mirror: %v", field, d)
			}
		})
	}
}

// TestMirrorRejectsIdentityFields covers bookkeeping/identity fields, which
// must never be replayed even though a malformed producer could name them
// directly: overwriting "key"/"version"/"itemType"/"dateModified" from a
// Change value would desync the mirror's own bookkeeping from the API.
func TestMirrorRejectsIdentityFields(t *testing.T) {
	for _, field := range []string{"key", "version", "itemType", "dateModified"} {
		t.Run(field, func(t *testing.T) {
			d := map[string]any{field: "original"}
			if applyChangeToItemData(d, mutation.Change{Field: field, Add: "corrupted"}) {
				t.Fatalf("identity field %q should not replay", field)
			}
			if d[field] != "original" {
				t.Fatalf("identity field %q was mutated: %v", field, d[field])
			}
		})
	}
}

// TestMirrorReplaysKnownScalarFields guards against over-tightening: real
// scalar fields this CLI's own write paths set (items_update.go's
// --title/--abstract-note/--extra) must keep replaying exactly as before.
func TestMirrorReplaysKnownScalarFields(t *testing.T) {
	for _, field := range []string{"title", "abstractNote", "extra"} {
		t.Run(field, func(t *testing.T) {
			d := map[string]any{field: ""}
			if !applyChangeToItemData(d, mutation.Change{Field: field, Add: "new value"}) || d[field] != "new value" {
				t.Fatalf("known scalar field %q should replay: %v", field, d)
			}
		})
	}
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

// Restore after a sync-reconciled trash must reconstruct the live row from
// the cached trash payload; otherwise the item ends up in neither table until
// the next sync. Common write-through trash keeps the live row (UpsertKeyed
// does not reconcile), so the fix must NOT overwrite a present live row.
// See zotio-a13b50b and reconcileItemLifecycleTx.
func TestApplyMirrorWriteThrough_RestoreReinstatesLiveRow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Seed only the trash mirror to simulate the sync-reconciled case where
	// reconcileItemLifecycleTx deleted the live row (trash had higher version).
	// Payload carries deleted markers and stale version fields that must be
	// stripped before reinsertion.
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	raw := json.RawMessage(`{"key":"R1","version":9,"data":{"key":"R1","itemType":"book","title":"Restored","deleted":1,"version":9,"dateModified":"2026-01-01T00:00:00Z"}}`)
	if err := db.UpsertKeyed("items-trash", []string{"R1"}, []json.RawMessage{raw}); err != nil {
		_ = db.Close()
		t.Fatalf("seed trash: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}

	env := mutation.Envelope{
		Plan: mutation.Plan{Operations: []mutation.Op{
			{ID: "items.restore:R1", Key: "R1", Kind: "item_restore", Changes: []mutation.Change{{Field: "deleted", Remove: true}}},
		}},
		Result: &mutation.Result{
			Summary: mutation.ResultSummary{Applied: 1},
			Items:   []mutation.ResultItem{{OpID: "items.restore:R1", Key: "R1", Status: "applied"}},
		},
	}
	stderr := captureWriteThroughStderr(t, func() { applyMirrorWriteThrough(&env) })
	if stderr != "" {
		t.Fatalf("restore with cached trash row should not warn, got stderr %q", stderr)
	}

	db2, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db2.Close()
	qs := localQueryStore{db2}

	// Live table must now contain the item and not the trash table.
	liveRows, err := qs.QueryRaw("SELECT data FROM resources WHERE resource_type='items' AND id=?", "R1")
	if err != nil || len(liveRows) != 1 {
		t.Fatalf("live row after restore: rows=%v err=%v", liveRows, err)
	}
	var item map[string]any
	if err := json.Unmarshal([]byte(sqlStringValue(liveRows[0]["data"])), &item); err != nil {
		t.Fatalf("decode live item: %v", err)
	}
	if _, ok := item["deleted"]; ok {
		t.Fatalf("restored live item still has top-level deleted: %v", item)
	}
	if data, ok := item["data"].(map[string]any); ok {
		if _, ok := data["deleted"]; ok {
			t.Fatalf("restored live item data still has deleted: %v", data)
		}
		if _, ok := data["version"]; ok {
			t.Fatalf("restored live item data still has stale version: %v", data)
		}
		if _, ok := data["dateModified"]; ok {
			t.Fatalf("restored live item data still has stale dateModified: %v", data)
		}
	} else {
		t.Fatalf("restored item has no data object: %v", item)
	}
	if _, ok := item["version"]; ok {
		t.Fatalf("restored live item still has stale top-level version: %v", item)
	}
	// title must survive the round-trip.
	if got := sqlStringValue(liveRows[0]["data"]); !strings.Contains(got, "Restored") {
		t.Fatalf("restored payload lost title: %v", got)
	}

	trashRows, err := qs.QueryRaw("SELECT id FROM resources WHERE resource_type='items-trash' AND id=?", "R1")
	if err != nil {
		t.Fatalf("query trash after restore: %v", err)
	}
	if len(trashRows) != 0 {
		t.Fatalf("trash row still present after restore: %v", trashRows)
	}
}

func TestApplyMirrorWriteThrough_RestoreWithoutTrashRowWarns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Create an empty store so openExistingStoreForWrite returns a DB
	// (not nil) but with no trash row to restore from.
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_ = db.Close()

	env := mutation.Envelope{
		Plan: mutation.Plan{Operations: []mutation.Op{
			{ID: "items.restore:MISSING", Key: "MISSING", Kind: "item_restore", Changes: []mutation.Change{{Field: "deleted", Remove: true}}},
		}},
		Result: &mutation.Result{
			Summary: mutation.ResultSummary{Applied: 1},
			Items:   []mutation.ResultItem{{OpID: "items.restore:MISSING", Key: "MISSING", Status: "applied"}},
		},
	}
	stderr := captureWriteThroughStderr(t, func() { applyMirrorWriteThrough(&env) })
	if !strings.Contains(stderr, "warning: read-your-writes mirror update failed for MISSING:") {
		t.Fatalf("missing-trash restore should warn, got stderr %q", stderr)
	}
	if !strings.Contains(stderr, "no cached trash row") {
		t.Fatalf("missing-trash warning should name the degraded-cache condition, got %q", stderr)
	}

	db2, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db2.Close()
	qs := localQueryStore{db2}
	liveRows, err := qs.QueryRaw("SELECT id FROM resources WHERE resource_type='items' AND id=?", "MISSING")
	if err != nil {
		t.Fatalf("query live after missing restore: %v", err)
	}
	if len(liveRows) != 0 {
		t.Fatalf("missing-trash restore should not create a live row, got %v", liveRows)
	}
	trashRows, err := qs.QueryRaw("SELECT id FROM resources WHERE resource_type='items-trash' AND id=?", "MISSING")
	if err != nil {
		t.Fatalf("query trash after missing restore: %v", err)
	}
	if len(trashRows) != 0 {
		t.Fatalf("trash row should remain absent after missing restore: %v", trashRows)
	}
}

func TestApplyMirrorWriteThrough_TrashKeepsLiveRow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedWriteThroughItem(t, "T1", `{"key":"T1","version":1,"data":{"key":"T1","itemType":"book","title":"TrashMe"}}`)

	env := mutation.Envelope{
		Plan: mutation.Plan{Operations: []mutation.Op{
			{ID: "items.delete:T1", Key: "T1", Kind: "item_trash", Changes: []mutation.Change{{Field: "deleted", Add: true}}},
		}},
		Result: &mutation.Result{
			Summary: mutation.ResultSummary{Applied: 1},
			Items:   []mutation.ResultItem{{OpID: "items.delete:T1", Key: "T1", Status: "applied"}},
		},
	}
	stderr := captureWriteThroughStderr(t, func() { applyMirrorWriteThrough(&env) })
	if stderr != "" {
		t.Fatalf("trash should not warn, got %q", stderr)
	}

	db2, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db2.Close()
	qs := localQueryStore{db2}
	trashRows, err := qs.QueryRaw("SELECT id FROM resources WHERE resource_type='items-trash' AND id=?", "T1")
	if err != nil || len(trashRows) != 1 {
		t.Fatalf("trash row after trash: rows=%v err=%v", trashRows, err)
	}
	liveRows, err := qs.QueryRaw("SELECT id FROM resources WHERE resource_type='items' AND id=?", "T1")
	if err != nil {
		t.Fatalf("query live after trash: %v", err)
	}
	if len(liveRows) != 1 {
		t.Fatalf("live row should remain after trash (Zotero keeps items with deleted=1; only permanent delete reaps) — got %v", liveRows)
	}
}

func TestApplyMirrorWriteThrough_RestoreWhenLivePresentDoesNotOverwrite(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Seed BOTH tables to simulate the common write-through trash path
	// where mirrorTrashedItem copied into trash without deleting the live
	// row (UpsertKeyed does not call reconcileItemLifecycleTx, so live
	// survives). The live row is newer (title LiveTitle) than the stale
	// trash copy (title StaleTitle). Restore must NOT overwrite live.
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	liveRaw := json.RawMessage(`{"key":"R2","version":2,"data":{"key":"R2","itemType":"book","title":"LiveTitle","version":2}}`)
	trashRaw := json.RawMessage(`{"key":"R2","version":1,"data":{"key":"R2","itemType":"book","title":"StaleTitle","deleted":1,"version":1}}`)
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{liveRaw}); err != nil {
		_ = db.Close()
		t.Fatalf("seed live: %v", err)
	}
	// Use UpsertKeyed for trash to avoid reconcile deleting live (mirrors real write-through)
	if err := db.UpsertKeyed("items-trash", []string{"R2"}, []json.RawMessage{trashRaw}); err != nil {
		_ = db.Close()
		t.Fatalf("seed trash: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}

	env := mutation.Envelope{
		Plan: mutation.Plan{Operations: []mutation.Op{
			{ID: "items.restore:R2", Key: "R2", Kind: "item_restore", Changes: []mutation.Change{{Field: "deleted", Remove: true}}},
		}},
		Result: &mutation.Result{
			Summary: mutation.ResultSummary{Applied: 1},
			Items:   []mutation.ResultItem{{OpID: "items.restore:R2", Key: "R2", Status: "applied"}},
		},
	}
	stderr := captureWriteThroughStderr(t, func() { applyMirrorWriteThrough(&env) })
	if stderr != "" {
		t.Fatalf("restore with live present should not warn, got %q", stderr)
	}

	db2, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db2.Close()
	qs := localQueryStore{db2}
	liveRows, err := qs.QueryRaw("SELECT data FROM resources WHERE resource_type='items' AND id=?", "R2")
	if err != nil || len(liveRows) != 1 {
		t.Fatalf("live row after restore: rows=%v err=%v", liveRows, err)
	}
	if got := sqlStringValue(liveRows[0]["data"]); !strings.Contains(got, "LiveTitle") {
		t.Fatalf("restore overwrote live row with stale trash payload: %v", got)
	}
	if got := sqlStringValue(liveRows[0]["data"]); strings.Contains(got, "StaleTitle") {
		t.Fatalf("live row should not contain stale trash title, got %v", got)
	}
	trashRows, err := qs.QueryRaw("SELECT id FROM resources WHERE resource_type='items-trash' AND id=?", "R2")
	if err != nil {
		t.Fatalf("query trash after restore: %v", err)
	}
	if len(trashRows) != 0 {
		t.Fatalf("trash row still present after restore: %v", trashRows)
	}
}
