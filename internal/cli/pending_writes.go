// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Reconcile locally-applied writes against the read plane during sync.
//
// zotio reads from the Zotero local desktop API and routes writes to
// api.zotero.org. Write-through (write_through.go) replays an applied write onto
// the mirror so the next local read is correct, but `sync` pulls from the read
// plane — which only learns about the write when Zotero itself syncs down. Left
// alone, sync re-applied the pre-write copy over the mirror and silently rolled
// the write back for every later read.
//
// A pending-write marker records what was applied. On each sync the recorded
// changes are re-applied on top of the incoming row, so the read plane's own
// edits land AND the local write survives. When re-applying is a no-op the read
// plane has caught up, and the marker is dropped.
//
// A permanent delete is the same lag window with no field to replay: the key is
// gone on the write plane while the read plane still lists it. Its marker
// carries deleted=1 and the reconciliation DROPS the listed row instead of
// merging into it, so a sync inside the window cannot resurrect a purged item
// (ADR-0007). Confirmation is the mirror image of the field case: the read
// plane no longer listing the key is what proves the delete landed, and only a
// complete pass can establish that, so store.SweepMissing retires the marker.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

// recordPendingWrite persists the changes just replayed onto the mirror so the
// next sync does not roll them back. Best-effort: the cloud write already
// succeeded, so a bookkeeping failure warns rather than failing the run.
func recordPendingWrite(db *store.Store, key string, changes []mutation.Change) {
	if len(changes) == 0 {
		return
	}
	encoded, err := json.Marshal(changes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record pending write for %s: %v\n", key, err)
		return
	}
	if err := db.RecordPendingWrite("items", key, encoded); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record pending write for %s: %v\n", key, err)
	}
}

// pendingWriteTTL bounds how long a marker can hold the mirror against the read
// plane. The gap it covers is normally seconds to minutes — however long Zotero
// takes to sync the write down. A marker still outstanding after this long means
// the write never propagated (Zotero sync disabled, say) or that replaying it can
// never converge; either way upstream truth must win rather than the row staying
// pinned to a local copy indefinitely.
//
// It is also the backstop for a deletion marker, whose normal confirmation
// (a complete pass that no longer lists the key) only arrives when a full sync
// runs. Expiry means the same thing there: upstream wins, and the row the read
// plane keeps reporting is stored again with a warning.
const pendingWriteTTL = 7 * 24 * time.Hour

// reconcilePendingWrites merges unconfirmed local writes into a page of rows
// pulled from the read plane, returning the rows to store and how many markers
// are still outstanding after this page. A row whose key carries a deletion
// marker is dropped from the returned page, so it is never stored.
func reconcilePendingWrites(db *store.Store, resource string, items []json.RawMessage) ([]json.RawMessage, int) {
	pending, err := db.PendingWrites(resource)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read pending writes for %s: %v\n", resource, err)
		return items, 0
	}
	if len(pending) == 0 {
		return items, 0
	}

	stillPending := 0
	dropped := 0
	// merged aliases items until a row actually changes, so an unaffected page
	// is stored without reallocation.
	merged := items
	copied := false
	ensureCopy := func() {
		if copied {
			return
		}
		merged = make([]json.RawMessage, len(items))
		copy(merged, items)
		copied = true
	}
	for i, raw := range items {
		id := pendingWriteID(resource, raw)
		if id == "" {
			continue
		}
		mark, ok := pending[id]
		if !ok {
			continue
		}
		expired := !mark.WrittenAt.IsZero() && time.Since(mark.WrittenAt) > pendingWriteTTL
		if mark.Deleted {
			if expired {
				// The read plane has listed a key zotio purged for longer than
				// the whole window is meant to last, so the delete never
				// propagated — Zotero sync is off, or the object still exists
				// there. Upstream truth wins, exactly as it does for an expired
				// field marker: the row is stored again. Loud, never silent,
				// because a reappearing item is otherwise indistinguishable
				// from a failed delete.
				fmt.Fprintf(os.Stderr,
					"warning: %s/%s was permanently deleted but the read plane still lists it after %s; restoring the read plane's copy to the mirror. Check that Zotero sync is enabled and that the delete reached zotero.org.\n",
					resource, id, pendingWriteTTL)
				clearPendingWrite(db, resource, id)
				continue
			}
			// Inside the window: the object is gone on the write plane and the
			// read plane has not caught up. Storing this row would resurrect it.
			// nil marks the slot for compaction below; a decoded JSON array
			// element is never nil, so the sentinel cannot collide with a row.
			ensureCopy()
			merged[i] = nil
			dropped++
			stillPending++
			continue
		}
		if expired {
			fmt.Fprintf(os.Stderr,
				"warning: %s/%s still unconfirmed after %s; accepting the read plane's copy. Check that Zotero sync is enabled.\n",
				resource, id, pendingWriteTTL)
			clearPendingWrite(db, resource, id)
			continue
		}
		replayed, changed, ok := replayPendingChanges(raw, mark.Changes)
		if !ok {
			// Nothing safe to merge. Accept the read plane's copy and drop the
			// marker rather than pinning the row to a local copy forever.
			clearPendingWrite(db, resource, id)
			continue
		}
		if !changed {
			// Re-applying changed nothing: the read plane already carries the
			// write, so the local copy is confirmed.
			clearPendingWrite(db, resource, id)
			continue
		}
		ensureCopy()
		merged[i] = replayed
		stillPending++
	}
	if dropped > 0 {
		kept := make([]json.RawMessage, 0, len(merged)-dropped)
		for _, raw := range merged {
			if raw != nil {
				kept = append(kept, raw)
			}
		}
		merged = kept
	}
	return merged, stillPending
}

// replayPendingChanges re-applies recorded changes to a row from the read plane.
// changed reports whether the row lacked the write; ok=false means the changes
// could not be replayed and the caller should not merge.
func replayPendingChanges(raw json.RawMessage, encoded []byte) (json.RawMessage, bool, bool) {
	var changes []mutation.Change
	if err := json.Unmarshal(encoded, &changes); err != nil || len(changes) == 0 {
		return nil, false, false
	}
	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, false, false
	}
	data, ok := item["data"].(map[string]any)
	if !ok {
		return nil, false, false
	}
	before, err := json.Marshal(data)
	if err != nil {
		return nil, false, false
	}
	for _, change := range changes {
		if !applyChangeToItemData(data, change) {
			return nil, false, false
		}
	}
	after, err := json.Marshal(data)
	if err != nil {
		return nil, false, false
	}
	if bytes.Equal(before, after) {
		return raw, false, true
	}
	item["data"] = data
	replayed, err := json.Marshal(item)
	if err != nil {
		return nil, false, false
	}
	return replayed, true, true
}

func clearPendingWrite(db *store.Store, resource, id string) {
	if err := db.ClearPendingWrite(resource, id); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not clear pending write for %s: %v\n", id, err)
	}
}

// pendingWriteID resolves the row key the same way the store does, so a marker
// recorded under one key is always matched against the same row.
func pendingWriteID(resource string, raw json.RawMessage) string {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	return extractID(resource, obj)
}
