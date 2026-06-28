// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase8 read-your-writes): tests that an applied write is
// replayed into the local mirror so --data-source local reads it WITHOUT a sync,
// and that unsupported change shapes are left for sync to reconcile.

package cli

import (
	"context"
	"encoding/json"
	"testing"

	"zotero-pp-cli/internal/mutation"
	"zotero-pp-cli/internal/store"
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
	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotero-pp-cli"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"Paper","DOI":""}}`),
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

	// 2) The local mirror reflects the write WITHOUT any sync.
	db2, err := store.OpenWithContext(context.Background(), defaultDBPath("zotero-pp-cli"))
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
