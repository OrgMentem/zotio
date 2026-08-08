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
const pendingWriteTTL = 7 * 24 * time.Hour

// reconcilePendingWrites merges unconfirmed local writes into a page of rows
// pulled from the read plane, returning the rows to store and how many markers
// are still outstanding after this page.
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
	merged := items
	for i, raw := range items {
		id := pendingWriteID(resource, raw)
		if id == "" {
			continue
		}
		mark, ok := pending[id]
		if !ok {
			continue
		}
		if !mark.WrittenAt.IsZero() && time.Since(mark.WrittenAt) > pendingWriteTTL {
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
		// Copy-on-write so an unaffected page is stored without reallocation.
		if &merged[0] == &items[0] && len(merged) == len(items) {
			merged = make([]json.RawMessage, len(items))
			copy(merged, items)
		}
		merged[i] = replayed
		stillPending++
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
