// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The mirror must not outlive the objects it mirrors.

package cli

import (
	"context"
	"encoding/json"
	"testing"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

func reapTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return db, dbPath
}

func mirroredItemKeys(t *testing.T, db *store.Store, resource string) map[string]bool {
	t.Helper()
	ids, err := db.ResourceIDs(resource)
	if err != nil {
		t.Fatalf("ResourceIDs(%s): %v", resource, err)
	}
	return ids
}

// Walk-test X-4: three items the reporter permanently deleted THROUGH zotio kept
// their mirror rows, so `items list --data-source local` and `search` served
// items that return 404 on both planes, and every mirror-derived count drifted
// upward (933 top-level items against 929 real ones).
func TestPermanentDeleteReapsMirrorRow(t *testing.T) {
	db, _ := reapTestStore(t)
	defer db.Close()

	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"GONE","version":1,"data":{"key":"GONE","itemType":"journalArticle","title":"Deleted"}}`),
		json.RawMessage(`{"key":"KEEP","version":1,"data":{"key":"KEEP","itemType":"journalArticle","title":"Kept"}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	// It was also in the trash, as a permanently deleted item often is.
	if err := db.UpsertKeyed("items-trash", []string{"GONE"},
		[]json.RawMessage{json.RawMessage(`{"key":"GONE","data":{"key":"GONE"}}`)}); err != nil {
		t.Fatalf("seed trash mirror: %v", err)
	}

	reapMirroredItem(db, "GONE")

	items := mirroredItemKeys(t, db, "items")
	if items["GONE"] {
		t.Error("permanently deleted item still mirrored; offline reads would keep serving it")
	}
	if !items["KEEP"] {
		t.Error("reaped an unrelated item")
	}
	if mirroredItemKeys(t, db, "items-trash")["GONE"] {
		t.Error("permanently deleted item still in the trash mirror")
	}
}

// Walk-test X-3: `items trash` could not show what `items delete` had just
// trashed, because the write lands on api.zotero.org while reads go to the local
// desktop API, which does not report the trash for ~15s.
func TestTrashWriteThroughMirrorsTheItem(t *testing.T) {
	db, _ := reapTestStore(t)
	defer db.Close()

	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"TRASHED","version":1,"data":{"key":"TRASHED","itemType":"book","title":"Just trashed"}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}

	mirrorTrashedItem(db, localQueryStore{db}, "TRASHED")

	if !mirroredItemKeys(t, db, "items-trash")["TRASHED"] {
		t.Fatal("trash write-through did not record the item, so `items trash` stays blind for ~15s after `items delete`")
	}

	// Restoring it must take it back out, or the trash listing keeps a ghost.
	if err := db.ReapResource("items-trash", "TRASHED"); err != nil {
		t.Fatalf("ReapResource: %v", err)
	}
	if mirroredItemKeys(t, db, "items-trash")["TRASHED"] {
		t.Error("restore did not clear the trash mirror row")
	}
}

// A full pass is the only way zotio can learn an object is gone, because the
// local desktop API implements no /deleted feed. Anything the pass did not
// report no longer exists.
func TestSweepMissingReapsAbsentRows(t *testing.T) {
	db, _ := reapTestStore(t)
	defer db.Close()

	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"ALIVE","version":1,"data":{"key":"ALIVE","itemType":"book"}}`),
		json.RawMessage(`{"key":"PHANTOM1","version":1,"data":{"key":"PHANTOM1","itemType":"book"}}`),
		json.RawMessage(`{"key":"PHANTOM2","version":1,"data":{"key":"PHANTOM2","itemType":"book"}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}

	reaped, err := db.SweepMissing("items", map[string]bool{"ALIVE": true})
	if err != nil {
		t.Fatalf("SweepMissing: %v", err)
	}
	if reaped != 2 {
		t.Fatalf("reaped %d rows, want 2", reaped)
	}
	keys := mirroredItemKeys(t, db, "items")
	if !keys["ALIVE"] || keys["PHANTOM1"] || keys["PHANTOM2"] {
		t.Fatalf("sweep left the wrong rows: %v", keys)
	}
}

// SweepMissing itself intentionally treats an empty seen-set as "everything is
// gone" — a resource with nothing reported really did lose every row it had.
// The safety property belongs to the CALLER: syncResource only calls this after
// completedNaturally is true, which is guaranteed false for any request or
// decode error (each returns before reaching the sweep). A fetch failure can
// therefore never masquerade as "the resource is empty" and reach here with a
// stale seen-set — that guarantee, not a check on len(seenKeys), is what makes
// this safe to call unconditionally on a genuinely empty full pass (see N4-3:
// requiring seenKeys to be non-empty left a phantom items-trash row unreapable
// forever, because the live trash really was empty on every pass).
func TestSweepMissingTreatsEmptySeenSetAsEverythingGone(t *testing.T) {
	db, _ := reapTestStore(t)
	defer db.Close()

	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"ALIVE","version":1,"data":{"key":"ALIVE","itemType":"book"}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	reaped, err := db.SweepMissing("items", map[string]bool{})
	if err != nil {
		t.Fatalf("SweepMissing: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("SweepMissing reaped %d, want 1: an empty seen-set must mean everything reported is gone", reaped)
	}
}

// Write-through must dispatch on op kind, not just replay field changes.
func TestWriteThroughDispatchesTrashAndDelete(t *testing.T) {
	db, dbPath := reapTestStore(t)
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"T1","version":1,"data":{"key":"T1","itemType":"book"}}`),
		json.RawMessage(`{"key":"D1","version":1,"data":{"key":"D1","itemType":"book"}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	env := mutation.Envelope{
		Plan: mutation.Plan{Operations: []mutation.Op{
			{ID: "items.delete:T1", Key: "T1", Kind: "item_trash"},
			{ID: "items.delete:D1", Key: "D1", Kind: "item_delete"},
		}},
		Result: &mutation.Result{
			Summary: mutation.ResultSummary{Applied: 2},
			Items: []mutation.ResultItem{
				{OpID: "items.delete:T1", Key: "T1", Status: "applied"},
				{OpID: "items.delete:D1", Key: "D1", Status: "applied"},
			},
		},
	}
	applyMirrorWriteThrough(&env)

	db2, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db2.Close()
	if !mirroredItemKeys(t, db2, "items-trash")["T1"] {
		t.Error("trashed item was not mirrored into items-trash")
	}
	if mirroredItemKeys(t, db2, "items")["D1"] {
		t.Error("permanently deleted item was not reaped from the mirror")
	}
	if !mirroredItemKeys(t, db2, "items")["T1"] {
		t.Error("a trash must not remove the item row itself; only a permanent delete does")
	}
}
