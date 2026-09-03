// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// A marker must hold the mirror against the read plane for exactly as long as
// the plane is known to be stale, and not one sync longer.

package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func pendingWritesTestItem(key string) json.RawMessage {
	return json.RawMessage(`{"key":"` + key + `","version":1,"data":{"key":"` + key + `","itemType":"book","title":"` + key + `"}}`)
}

func pendingWritesTestKeys(t *testing.T, page []json.RawMessage) []string {
	t.Helper()
	keys := make([]string, 0, len(page))
	for _, raw := range page {
		if raw == nil {
			t.Fatal("reconcilePendingWrites returned a nil row; the drop sentinel leaked into the stored page")
		}
		keys = append(keys, pendingWriteID("items", raw))
	}
	return keys
}

// A deletion marker removes the key from the page instead of merging into it.
// Merging is meaningless for a delete — there is no field to replay — and
// storing the row is the resurrection itself. Suppression is per key: the rest
// of the page is what the read plane genuinely reports and must still land, in
// the order it arrived.
func TestReconcilePendingWritesDropsRowsMarkedDeleted(t *testing.T) {
	db := syncTestOpenStore(t)
	defer db.Close()

	for _, key := range []string{"GONE1", "GONE2"} {
		if _, err := db.UpsertKeyed("items", []string{key}, []json.RawMessage{pendingWritesTestItem(key)}); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
		if err := db.ReapMirroredItem(key); err != nil {
			t.Fatalf("ReapMirroredItem %s: %v", key, err)
		}
	}

	page := []json.RawMessage{
		pendingWritesTestItem("GONE1"),
		pendingWritesTestItem("KEEP1"),
		pendingWritesTestItem("GONE2"),
		pendingWritesTestItem("KEEP2"),
	}
	stderr := captureStderr(t, func() {
		merged, stillPending := reconcilePendingWrites(db, "items", page)
		got := pendingWritesTestKeys(t, merged)
		if strings.Join(got, ",") != "KEEP1,KEEP2" {
			t.Errorf("stored page = %v, want [KEEP1 KEEP2]: a purged key was re-inserted, or an unrelated row was dropped", got)
		}
		if stillPending != 2 {
			t.Errorf("stillPending = %d, want 2: both deletions are unconfirmed until the read plane stops listing them", stillPending)
		}
	})
	if stderr != "" {
		t.Errorf("stderr = %q, want silence: an in-window delete is the normal case, not a warning", stderr)
	}

	pending, err := db.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	for _, key := range []string{"GONE1", "GONE2"} {
		if !pending[key].Deleted {
			t.Errorf("deletion marker for %s was cleared while the read plane still lists the key; the next page resurrects it", key)
		}
	}
}

// Documented TTL behaviour. A read plane that still lists a purged key after
// the whole TTL is not lagging any more: the delete never reached zotero.org,
// or Zotero sync is off, so the object really does still exist upstream.
// Upstream then wins, exactly as it does for an expired field marker — the row
// is stored again. It must NOT come back silently: a reappearing item is
// otherwise indistinguishable from a delete that failed, and the marker must go
// so the row is not re-dropped on every later sync forever.
func TestReconcilePendingWritesYieldsToTheReadPlaneWhenADeletionMarkerExpires(t *testing.T) {
	db := syncTestOpenStore(t)
	defer db.Close()

	if _, err := db.UpsertKeyed("items", []string{"STUCK"}, []json.RawMessage{pendingWritesTestItem("STUCK")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.ReapMirroredItem("STUCK"); err != nil {
		t.Fatalf("ReapMirroredItem: %v", err)
	}
	if _, err := db.DB().Exec(`UPDATE pending_writes SET written_at = ? WHERE id = 'STUCK'`,
		time.Now().Add(-2*pendingWriteTTL)); err != nil {
		t.Fatalf("age deletion marker: %v", err)
	}

	page := []json.RawMessage{pendingWritesTestItem("STUCK")}
	var merged []json.RawMessage
	stderr := captureStderr(t, func() {
		merged, _ = reconcilePendingWrites(db, "items", page)
	})

	if got := pendingWritesTestKeys(t, merged); len(got) != 1 || got[0] != "STUCK" {
		t.Fatalf("stored page = %v, want [STUCK]: an expired marker must yield to the read plane, not suppress the row forever", got)
	}
	if !strings.Contains(stderr, "STUCK") || !strings.Contains(stderr, "permanently deleted") {
		t.Errorf("stderr = %q, want a warning naming the key and the failed delete: the row must never reappear silently", stderr)
	}
	pending, err := db.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if _, ok := pending["STUCK"]; ok {
		t.Error("expired deletion marker survived; it would re-drop the row on every later sync")
	}
}

// Markers are scoped per resource type. The reap writes one for items and one
// for items-trash because both planes keep listing a purged key, but a marker
// for one resource must never suppress a same-keyed row of another — Zotero
// keys are unique per library, not per resource, and a collection can share a
// key shape with an item.
func TestReconcilePendingWritesScopesDeletionMarkersToTheirResource(t *testing.T) {
	db := syncTestOpenStore(t)
	defer db.Close()

	if _, err := db.UpsertKeyed("items", []string{"SHARED"}, []json.RawMessage{pendingWritesTestItem("SHARED")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.ReapMirroredItem("SHARED"); err != nil {
		t.Fatalf("ReapMirroredItem: %v", err)
	}

	page := []json.RawMessage{json.RawMessage(`{"key":"SHARED","version":1,"data":{"key":"SHARED","name":"A collection"}}`)}
	merged, stillPending := reconcilePendingWrites(db, "collections", page)
	if len(merged) != 1 {
		t.Fatalf("stored collections page = %d rows, want 1: an items deletion marker suppressed a collection", len(merged))
	}
	if stillPending != 0 {
		t.Errorf("stillPending = %d, want 0", stillPending)
	}
}
