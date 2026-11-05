// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"zotio/internal/store"
)

const (
	syncTrackItemV1 = `{"key":"AAA","version":1,"data":{"key":"AAA","version":1,"itemType":"journalArticle","title":"One"}}`
	syncTrackItemV2 = `{"key":"AAA","version":2,"data":{"key":"AAA","version":2,"itemType":"journalArticle","title":"Two"}}`
)

func newSyncTrackingStore(t *testing.T, name string) *store.Store {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func syncTrackingContext() (context.Context, *syncChangeTracker) {
	tracker := &syncChangeTracker{}
	return context.WithValue(context.Background(), syncChangeTrackerContextKey{}, tracker), tracker
}

func syncTrackItemRefs() []syncRowRef {
	return []syncRowRef{{resource: "items", id: "AAA"}}
}

// TestTrackSyncRowChanges_ConcurrentWriterNotCountedAsThisWritersChange proves
// the change count of one tracked write is not inflated by another worker's
// write landing between its before- and after-reads. sync runs one worker per
// resource (default concurrency 4) and those workers share rows — items and
// items-trash always pair — so the unlocked diff attributed a neighbour's
// write to a no-op write of its own, and `watch --workflow-on-change` ran the
// triggered workflow after a cycle that changed nothing.
//
// Exactly one row image changes in the whole scenario (v1 -> v2, by the
// interfering writer), so the run's total must be 1, not 2.
func TestTrackSyncRowChanges_ConcurrentWriterNotCountedAsThisWritersChange(t *testing.T) {
	db := newSyncTrackingStore(t, "sync_change_race.db")
	if err := db.Upsert("items", "AAA", json.RawMessage(syncTrackItemV1)); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	ctx, tracker := syncTrackingContext()

	noopWriterInside := make(chan struct{})
	interferingDone := make(chan struct{})
	var interferingErr error
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-noopWriterInside
		interferingErr = trackSyncRowChanges(ctx, db, syncTrackItemRefs, func(ctx context.Context) error {
			return db.UpsertContext(ctx, "items", "AAA", json.RawMessage(syncTrackItemV2))
		})
		close(interferingDone)
	}()

	// This writer re-sends the payload the row already holds: a no-op that
	// must count nothing. It waits inside its own tracked section to give the
	// interfering writer every chance to land between the two reads.
	noopErr := trackSyncRowChanges(ctx, db, syncTrackItemRefs, func(ctx context.Context) error {
		close(noopWriterInside)
		select {
		case <-interferingDone:
			// Unserialized: the neighbour's write landed inside this
			// writer's window. Its change is about to be counted twice.
		case <-time.After(time.Second):
			// Serialized: the neighbour is waiting for this write lease.
		}
		return db.UpsertContext(ctx, "items", "AAA", json.RawMessage(syncTrackItemV1))
	})
	if noopErr != nil {
		t.Fatalf("no-op tracked write: %v", noopErr)
	}
	wg.Wait()
	if interferingErr != nil {
		t.Fatalf("interfering tracked write: %v", interferingErr)
	}

	stored, err := db.Get("items", "AAA")
	if err != nil {
		t.Fatalf("read back item: %v", err)
	}
	if string(stored) != syncTrackItemV2 {
		t.Fatalf("stored row = %s, want the v2 payload", stored)
	}
	if got := tracker.upserted.Load(); got != 1 {
		t.Fatalf("counted changes = %d, want 1 (only the v1 -> v2 write changed a row)", got)
	}
}

// TestTrackSyncRowChanges_CountsOnlyRealChanges pins the documented rule the
// serialization must preserve across all three tracked write paths: a row the
// plane re-sends unchanged, and a payload the version-monotonic guard retains
// as older, are not changes. It also proves those writes do not deadlock on
// the write lease the tracker now holds around them.
func TestTrackSyncRowChanges_CountsOnlyRealChanges(t *testing.T) {
	db := newSyncTrackingStore(t, "sync_change_counts.db")
	ctx, tracker := syncTrackingContext()

	step := func(name string, want int64, run func() error) {
		t.Helper()
		before := tracker.upserted.Load()
		done := make(chan error, 1)
		go func() { done <- run() }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: tracked write deadlocked on the store write lease", name)
		}
		if got := tracker.upserted.Load() - before; got != want {
			t.Fatalf("%s: counted %d changes, want %d", name, got, want)
		}
	}

	// Batch page path (UpsertBatchContext).
	step("new item page", 1, func() error {
		_, _, err := upsertResourceBatch(ctx, db, "items", []json.RawMessage{json.RawMessage(syncTrackItemV1)})
		return err
	})
	step("identical item page", 0, func() error {
		_, _, err := upsertResourceBatch(ctx, db, "items", []json.RawMessage{json.RawMessage(syncTrackItemV1)})
		return err
	})
	step("changed item page", 1, func() error {
		_, _, err := upsertResourceBatch(ctx, db, "items", []json.RawMessage{json.RawMessage(syncTrackItemV2)})
		return err
	})
	step("superseded item page", 0, func() error {
		_, _, err := upsertResourceBatch(ctx, db, "items", []json.RawMessage{json.RawMessage(syncTrackItemV1)})
		return err
	})

	// Caller-keyed path (UpsertKeyedContext), as the dependent schema sync uses.
	keyedRefs := func() []syncRowRef {
		return []syncRowRef{{resource: "schema-item-type-fields", id: "book"}}
	}
	keyedWrite := func(payload string) func() error {
		return func() error {
			return trackSyncRowChanges(ctx, db, keyedRefs, func(ctx context.Context) error {
				_, err := db.UpsertKeyedContext(ctx, "schema-item-type-fields", []string{"book"}, []json.RawMessage{json.RawMessage(payload)})
				return err
			})
		}
	}
	step("new keyed row", 1, keyedWrite(`[{"field":"title"}]`))
	step("identical keyed row", 0, keyedWrite(`[{"field":"title"}]`))
	step("changed keyed row", 1, keyedWrite(`[{"field":"title"},{"field":"series"}]`))

	// Single-object path (UpsertContext).
	step("new single object", 1, func() error {
		return upsertSingleObject(ctx, db, "keys", json.RawMessage(`{"username":"one"}`))
	})
	step("identical single object", 0, func() error {
		return upsertSingleObject(ctx, db, "keys", json.RawMessage(`{"username":"one"}`))
	})
	step("changed single object", 1, func() error {
		return upsertSingleObject(ctx, db, "keys", json.RawMessage(`{"username":"two"}`))
	})
}

// TestTrackSyncRowChanges_UntrackedRunDoesNoChangeDetection keeps a plain
// `zotio sync` (no summary, so no tracker) on the unchanged path: the write
// runs directly, and the row refs are never decoded.
func TestTrackSyncRowChanges_UntrackedRunDoesNoChangeDetection(t *testing.T) {
	db := newSyncTrackingStore(t, "sync_change_untracked.db")
	refsCalls := 0
	refs := func() []syncRowRef {
		refsCalls++
		return syncTrackItemRefs()
	}
	err := trackSyncRowChanges(context.Background(), db, refs, func(ctx context.Context) error {
		return db.UpsertContext(ctx, "items", "AAA", json.RawMessage(syncTrackItemV1))
	})
	if err != nil {
		t.Fatalf("untracked tracked write: %v", err)
	}
	if refsCalls != 0 {
		t.Fatalf("rowRefs called %d times in an untracked run, want 0", refsCalls)
	}
	if _, err := db.Get("items", "AAA"); err != nil {
		t.Fatalf("untracked write did not land: %v", err)
	}
}
