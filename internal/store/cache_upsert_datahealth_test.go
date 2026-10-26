// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestReadCacheDataHealthSerializesDeleteThroughPublication(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	row := json.RawMessage(`{"key":"ABCD2345","version":1,"data":{"key":"ABCD2345","itemType":"book","title":"Deleted"}}`)
	deleteDone := make(chan error, 1)
	called := false
	s.upsertBatchHook = func() {
		called = true
		// This hook runs after the marker lookup and before publication. The reap
		// must not acquire the writer lock anywhere in that interval.
		if s.writeMu.TryLock() {
			s.writeMu.Unlock()
			t.Fatal("read cache released the writer lock before publication")
		}
		go func() { deleteDone <- s.ReapMirroredItem("ABCD2345") }()
	}
	if err := s.UpsertReadCacheContext(context.Background(), "items", []json.RawMessage{row}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("cache did not reach publication")
	}
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	s.upsertBatchHook = nil
	// A read after the reap must also suppress the row, not retire its marker.
	for _, resource := range []string{"items", "items-trash"} {
		if err := s.UpsertReadCacheContext(context.Background(), resource, []json.RawMessage{row}); err != nil {
			t.Fatal(err)
		}
		raw, err := s.Get(resource, "ABCD2345")
		if err == nil && len(raw) != 0 {
			t.Fatalf("%s resurrected %s", resource, raw)
		}
		marked, err := s.PendingDeletion(resource, "ABCD2345")
		if err != nil || !marked {
			t.Fatalf("%s marker=%v err=%v", resource, marked, err)
		}
	}
}

func TestReadCacheDataHealthBatchIsAtomic(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.db.Exec(`CREATE TRIGGER fail_cache_insert BEFORE INSERT ON resources WHEN NEW.id = 'ABCD2346' BEGIN SELECT RAISE(ABORT, 'injected cache failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	err = s.UpsertReadCacheContext(context.Background(), "items", []json.RawMessage{
		json.RawMessage(`{"key":"ABCD2345","version":1,"data":{"key":"ABCD2345","itemType":"book"}}`),
		json.RawMessage(`{"key":"ABCD2346","version":1,"data":{"key":"ABCD2346","itemType":"book"}}`),
	})
	if err == nil {
		t.Fatal("expected injected publication failure")
	}
	raw, err := s.Get("items", "ABCD2345")
	if err == nil && len(raw) != 0 {
		t.Fatalf("failed batch committed its first row: %s", raw)
	}
}
