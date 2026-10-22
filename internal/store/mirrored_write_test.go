// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// A confirmed write's mirror row and its pending-write marker must commit
// together (finding zotio-f570e9ac7c877d14). A kill between the row upsert
// and the marker insert used to leave the mirror without its suppression,
// so the next sync read the still-stale read plane over the just-confirmed
// write and rolled it back until Zotero synced it down.

package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func mirroredWriteTestRow(key string) json.RawMessage {
	return json.RawMessage(`{"key":"` + key + `","version":1,"data":{"key":"` + key + `","itemType":"book","title":"` + key + `"}}`)
}

func mirroredWriteTestOpen(t *testing.T) *Store {
	t.Helper()
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Happy path: the replayed row and its marker land together.
func TestUpsertMirroredWriteStoresRowAndMarker(t *testing.T) {
	s := mirroredWriteTestOpen(t)

	changes := []byte(`[{"field":"title","add":"Renamed"}]`)
	if err := s.UpsertMirroredWrite("items", "K1", mirroredWriteTestRow("K1"), changes); err != nil {
		t.Fatalf("UpsertMirroredWrite: %v", err)
	}
	if got, err := s.Get("items", "K1"); err != nil {
		t.Fatalf("Get: %v", err)
	} else if len(got) == 0 {
		t.Fatal("mirrored row missing after UpsertMirroredWrite")
	}
	pending, err := s.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	mark, ok := pending["K1"]
	if !ok || mark.Deleted || string(mark.Changes) != string(changes) {
		t.Fatalf("pending marker = %+v, present=%v: the marker must record the applied changes", mark, ok)
	}
}

// The crash-gap regression: when the marker write fails, the row rolls back
// with it instead of committing unsuppressed.
func TestUpsertMirroredWriteRollsBackRowWhenMarkerFails(t *testing.T) {
	s := mirroredWriteTestOpen(t)

	if _, err := s.DB().Exec(`CREATE TRIGGER fail_marker_insert BEFORE INSERT ON pending_writes BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`); err != nil {
		t.Fatalf("install marker trigger: %v", err)
	}

	err := s.UpsertMirroredWrite("items", "K1", mirroredWriteTestRow("K1"), []byte(`[{"field":"title","add":"Renamed"}]`))
	if err == nil {
		t.Fatal("UpsertMirroredWrite succeeded with the marker insert broken; the row would commit without suppression")
	}
	if _, gerr := s.Get("items", "K1"); gerr == nil {
		t.Fatal("mirrored row survived a failed marker write; a crash here is the torn state the transaction must prevent")
	}

	if _, err := s.DB().Exec(`DROP TRIGGER fail_marker_insert`); err != nil {
		t.Fatalf("drop marker trigger: %v", err)
	}
	if err := s.UpsertMirroredWrite("items", "K1", mirroredWriteTestRow("K1"), []byte(`[{"field":"title","add":"Renamed"}]`)); err != nil {
		t.Fatalf("retry UpsertMirroredWrite: %v", err)
	}
	if _, err := s.Get("items", "K1"); err != nil {
		t.Fatalf("retry Get: %v", err)
	}
}

// A create retires a standing deletion marker and publishes row and marker
// together, so the new object is never suppressed by its own past.
func TestCreateMirroredItemRetiresDeletionMarkerAtomically(t *testing.T) {
	s := mirroredWriteTestOpen(t)

	if _, err := s.UpsertKeyed("items", []string{"GHOST1"}, []json.RawMessage{mirroredWriteTestRow("GHOST1")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.ReapMirroredItem("GHOST1"); err != nil {
		t.Fatalf("reap: %v", err)
	}

	row := json.RawMessage(`{"key":"GHOST1","data":{"key":"GHOST1","itemType":"journalArticle","title":"Recreated"}}`)
	if err := s.CreateMirroredItem("GHOST1", row, []byte(`[{"field":"item"}]`)); err != nil {
		t.Fatalf("CreateMirroredItem: %v", err)
	}
	if _, err := s.Get("items", "GHOST1"); err != nil {
		t.Fatalf("created row missing: %v", err)
	}
	for _, resource := range []string{"items", "items-trash"} {
		if deleted, err := s.PendingDeletion(resource, "GHOST1"); err != nil {
			t.Fatalf("PendingDeletion %s: %v", resource, err)
		} else if deleted {
			t.Fatalf("deletion marker for %s/GHOST1 survived the confirmed create; every sync would drop the new row", resource)
		}
	}
	pending, err := s.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if _, ok := pending["GHOST1"]; !ok {
		t.Fatal("create marker missing: the unconfirmed create must stay visible until the read plane lists it")
	}
}

// With the marker insert broken, a create lands nothing: no row without
// suppression, and no retired deletion marker without its replacement row.
func TestCreateMirroredItemRollsBackWhenMarkerFails(t *testing.T) {
	s := mirroredWriteTestOpen(t)

	if _, err := s.DB().Exec(`CREATE TRIGGER fail_create_marker BEFORE INSERT ON pending_writes BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`); err != nil {
		t.Fatalf("install marker trigger: %v", err)
	}

	row := json.RawMessage(`{"key":"NEW1","data":{"key":"NEW1","itemType":"book","title":"New"}}`)
	if err := s.CreateMirroredItem("NEW1", row, []byte(`[{"field":"item"}]`)); err == nil {
		t.Fatal("CreateMirroredItem succeeded with the marker insert broken; the row would commit without suppression")
	}
	if _, gerr := s.Get("items", "NEW1"); gerr == nil {
		t.Fatal("created row survived a failed marker write; the next stale sync would roll the confirmed create back")
	}
}
