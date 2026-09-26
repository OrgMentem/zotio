// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Regression tests for three sync-safety findings. The mirror-row atomicity
// half of zotio-f570e9ac7c877d14 lives at the store layer in
// internal/store/mirrored_write_test.go; this file pins the sync-visible
// contracts: a pending-marker lookup failure fails the page without storing
// a ghost (zotio-331e9da9181eaed3), and a failed full-pass sweep fails the
// resource without advancing the checkpoint (zotio-a7e373e8cbd6a1aa).

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A pending-marker lookup failure must fail the page. The markers are what
// stop a sync inside the read-plane lag window from re-inserting a purged
// item; accepting the raw page without checking suppression stores a ghost
// the later passes never remove.
func TestReconcilePendingWritesFailsOnMarkerLookupFailure(t *testing.T) {
	db := syncTestOpenStore(t)
	defer db.Close()

	if _, err := db.DB().Exec(`ALTER TABLE pending_writes RENAME TO pending_writes_broken`); err != nil {
		t.Fatalf("break pending_writes: %v", err)
	}

	merged, stillPending, rerr := reconcilePendingWrites(db, "items", []json.RawMessage{pendingWritesTestItem("KEEP1")})
	if rerr == nil {
		t.Fatal("reconcile succeeded with the marker lookup broken; the raw page would be stored without suppression")
	}
	if merged != nil || stillPending != 0 {
		t.Fatalf("reconcile = (%v, %d), want (nil, 0) on lookup failure: no row may be accepted without its markers", merged, stillPending)
	}
}

// End to end for the same defect: an incremental sync whose marker lookup
// fails must error and store no ghost, and the retry after the lookup is
// repaired must suppress the purged row again.
func TestSyncStoresNoGhostWhenPendingLookupFails(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	// Mirror state as a permanent delete leaves it: rows reaped, deletion
	// markers standing for both canonical resources.
	if _, err := db.UpsertKeyed("items", []string{"GHOST1"}, []json.RawMessage{pendingWritesTestItem("GHOST1")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.ReapMirroredItem("GHOST1"); err != nil {
		t.Fatalf("reap: %v", err)
	}

	// The desktop has not synced the delete down yet, so the read plane
	// still lists the purged key.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[` + string(pendingWritesTestItem("GHOST1")) + `]`))
	}))
	defer server.Close()

	if _, err := db.DB().Exec(`ALTER TABLE pending_writes RENAME TO pending_writes_broken`); err != nil {
		t.Fatalf("break pending_writes: %v", err)
	}

	res := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 0, false, 1, false)
	if res.Err == nil {
		t.Fatal("sync succeeded with the marker lookup broken; the ghost row would be stored as a success")
	}
	if ids, err := db.ResourceIDs("items"); err != nil {
		t.Fatalf("ResourceIDs: %v", err)
	} else if ids["GHOST1"] {
		t.Fatal("sync stored the purged row while its markers were unreadable; later passes drop incoming copies but never remove this ghost")
	}

	// Repair the lookup and retry: suppression succeeds, still with no ghost.
	if _, err := db.DB().Exec(`ALTER TABLE pending_writes_broken RENAME TO pending_writes`); err != nil {
		t.Fatalf("restore pending_writes: %v", err)
	}
	if res := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 0, false, 1, false); res.Err != nil {
		t.Fatalf("retry sync: %v", res.Err)
	}
	if ids, err := db.ResourceIDs("items"); err != nil {
		t.Fatalf("ResourceIDs: %v", err)
	} else if ids["GHOST1"] {
		t.Fatal("retry stored the purged row despite its deletion marker standing")
	}
	if deleted, err := db.PendingDeletion("items", "GHOST1"); err != nil {
		t.Fatalf("PendingDeletion: %v", err)
	} else if !deleted {
		t.Fatal("retry retired the deletion marker while the read plane still lists the key")
	}
}

// A failed full-pass sweep must fail the resource and leave the checkpoint
// retryable. The stale rows and unconfirmed markers are still in the mirror,
// so reporting success checkpoints past data no later pass revisits.
func TestFullSyncSweepFailureFailsResourceWithoutCheckpoint(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[` + string(pendingWritesTestItem("FRESH")) + `]`))
	}))
	defer server.Close()
	client := syncTestClient(server.URL)

	if _, err := db.UpsertKeyed("items", []string{"STALE"}, []json.RawMessage{pendingWritesTestItem("STALE")}); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}
	if err := db.SaveLibraryVersion("items", client.Plane(), 42); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}

	// Fail only the sweep's reaps: page upserts are INSERTs, so a trigger on
	// DELETE FROM resources fires exclusively inside SweepMissing here.
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_sweep_before_delete BEFORE DELETE ON resources BEGIN SELECT RAISE(ABORT, 'injected sweep failure'); END`); err != nil {
		t.Fatalf("install sweep trigger: %v", err)
	}

	res := syncResource(context.Background(), client, db, "items", 0, true, 0, false)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "reaping deleted") {
		t.Fatalf("sync err = %v, want a reaping failure: a failed sweep must not report complete", res.Err)
	}
	if ids, err := db.ResourceIDs("items"); err != nil {
		t.Fatalf("ResourceIDs: %v", err)
	} else if !ids["STALE"] {
		t.Fatal("failed sweep reaped the stale row anyway; the reap is supposed to roll back with its transaction")
	}
	if v, err := db.GetLibraryVersion("items", client.Plane()); err != nil {
		t.Fatalf("GetLibraryVersion: %v", err)
	} else if v != 42 {
		t.Fatalf("library version = %d, want 42: the checkpoint advanced past a sweep that never landed", v)
	}

	if _, err := db.DB().Exec(`DROP TRIGGER fail_sweep_before_delete`); err != nil {
		t.Fatalf("drop sweep trigger: %v", err)
	}
	if res := syncResource(context.Background(), client, db, "items", 0, true, 0, false); res.Err != nil {
		t.Fatalf("retry sync: %v", res.Err)
	}
	if ids, err := db.ResourceIDs("items"); err != nil {
		t.Fatalf("ResourceIDs: %v", err)
	} else if ids["STALE"] {
		t.Fatal("retry did not reap the row the plane stopped reporting")
	}
}
