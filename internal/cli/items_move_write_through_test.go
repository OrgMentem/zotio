// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// A successful items move must be visible to the next local read.

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zotio/internal/store"
)

// Field report #1 finding 2: after a successful `items move`, reads still
// reported the pre-write membership because reads resolve against the local
// mirror while the write lands on the Web API. Exercises the whole command path
// (not a synthetic op) so a regression in the write-through hook, in the change
// shape items move emits, or in the mirror replay is caught here.
func TestItemsMoveWritesThroughToLocalMirror(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"KEYAAAA2","version":24,"data":{"key":"KEYAAAA2","itemType":"journalArticle","title":"Paper","collections":[]}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	// Install the hook exactly as Execute does.
	mirrorWriteThrough = applyMirrorWriteThrough
	t.Cleanup(func() { mirrorWriteThrough = nil })

	srv := newItemMoveTestServer(t, map[string]string{"KEYAAAA2": "24"}, map[string][]string{"KEYAAAA2": {}})
	env := mustRunItemsMoveTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--to", "TARGET22", "KEYAAAA2")
	if env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("env = %+v, want one applied move", env)
	}
	if srv.patchCounts["KEYAAAA2"] != 1 {
		t.Fatalf("PATCH count = %d, want 1", srv.patchCounts["KEYAAAA2"])
	}

	// The mirror must reflect the write with no intervening sync, or the next
	// read contradicts the write that just succeeded.
	mirrored := mirroredCollections(t, dbPath, "KEYAAAA2")
	if len(mirrored) != 1 || mirrored[0] != "TARGET22" {
		t.Fatalf("mirror collections after move = %v, want [TARGET22]", mirrored)
	}

	// And the envelope must carry the post-write state so an agent needs no
	// follow-up read at all.
	if env.Result.Items[0].Item == nil {
		t.Fatal("applied result item carries no post-write state")
	}
	data, _ := env.Result.Items[0].Item["data"].(map[string]any)
	cols, _ := data["collections"].([]any)
	if len(cols) != 1 || cols[0] != "TARGET22" {
		t.Fatalf("envelope post-write collections = %v, want [TARGET22]", data["collections"])
	}
}

// The reporter's exact sequence: move, then sync, then read. Sync pulls from the
// local desktop API, which has not received the Web API write yet, so without
// pending-write reconciliation this rolled the move back in the store the reads
// use — the move "succeeded" and the next read denied it.
func TestItemsMoveSurvivesASyncFromTheStaleReadPlane(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	home := t.TempDir()
	t.Setenv("HOME", home)

	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"KEYAAAA2","version":24,"data":{"key":"KEYAAAA2","itemType":"journalArticle","title":"Paper","collections":[]}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	mirrorWriteThrough = applyMirrorWriteThrough
	t.Cleanup(func() { mirrorWriteThrough = nil })

	srv := newItemMoveTestServer(t, map[string]string{"KEYAAAA2": "24"}, map[string][]string{"KEYAAAA2": {}})
	if env := mustRunItemsMoveTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--to", "TARGET22", "KEYAAAA2"); env.Result.Summary.Applied != 1 {
		t.Fatalf("env = %+v, want one applied move", env)
	}

	// Now sync from a read plane that still reports the pre-write membership.
	readPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"KEYAAAA2","version":25,"data":{"key":"KEYAAAA2","itemType":"journalArticle","title":"Paper","collections":[]}}]`))
	}))
	defer readPlane.Close()

	syncDB, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store for sync: %v", err)
	}
	if res := syncResource(context.Background(), syncTestClient(readPlane.URL), syncDB, "items", 0, false, 1, false); res.Err != nil {
		t.Fatalf("sync error: %v", res.Err)
	}
	if err := syncDB.Close(); err != nil {
		t.Fatalf("close sync store: %v", err)
	}

	if mirrored := mirroredCollections(t, dbPath, "KEYAAAA2"); len(mirrored) != 1 || mirrored[0] != "TARGET22" {
		t.Fatalf("collections after move+sync = %v, want [TARGET22]", mirrored)
	}
}

// Removing a membership must be mirrored too, not just adding one.
func TestItemsMoveRemovalWritesThroughToLocalMirror(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"KEYAAAA2","version":24,"data":{"key":"KEYAAAA2","itemType":"journalArticle","title":"Paper","collections":["SRCAAAA2","KEEP2222"]}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	mirrorWriteThrough = applyMirrorWriteThrough
	t.Cleanup(func() { mirrorWriteThrough = nil })

	srv := newItemMoveTestServer(t, map[string]string{"KEYAAAA2": "24"}, map[string][]string{"KEYAAAA2": {"SRCAAAA2", "KEEP2222"}})
	env := mustRunItemsMoveTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--from", "SRCAAAA2", "KEYAAAA2")
	if env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("env = %+v, want one applied removal", env)
	}

	mirrored := mirroredCollections(t, dbPath, "KEYAAAA2")
	if len(mirrored) != 1 || mirrored[0] != "KEEP2222" {
		t.Fatalf("mirror collections after removal = %v, want [KEEP2222]", mirrored)
	}
}

func mirroredCollections(t *testing.T, dbPath, key string) []string {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db.Close()
	rows, err := (localQueryStore{db}).QueryRaw(
		"SELECT json_extract(data,'$.data.collections') AS cols FROM resources WHERE resource_type='items' AND id=?", key)
	if err != nil {
		t.Fatalf("read back mirror: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("mirror rows for %s = %d, want 1", key, len(rows))
	}
	var cols []string
	raw := sqlStringValue(rows[0]["cols"])
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), &cols); err != nil {
		t.Fatalf("decode mirrored collections %q: %v", raw, err)
	}
	return cols
}
