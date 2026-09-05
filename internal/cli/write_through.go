// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// After a write succeeds on the write plane -- api.zotero.org for a Web write,
// the desktop connector for a connector create -- write-through replays the
// just-applied changes onto the local SQLite mirror so `--data-source local`
// reads-your-own-writes WITHOUT a `sync`, and surfaces the resulting item state
// in the mutation envelope so agents need no follow-up read. Best-effort: changes
// it can't confidently replay (merges, bulk adds) are left for the next `sync`
// to reconcile authoritatively.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

// mirrorWriteThrough, when non-nil, updates the local mirror from applied writes.
// Set only on the real Execute() path so unit tests driving runMutation directly
// don't touch the filesystem (mirrors the journal-recorder pattern).
var mirrorWriteThrough func(env *mutation.Envelope)

// queryRawForRestore is a test seam to inject query failures for the restore
// path. When nil, restore uses qs.QueryRaw directly. Tests may set it to
// return an error for the live-row existence check to exercise Defect C's
// error path without breaking the preceding trash-row read.
var queryRawForRestore func(qs localQueryStore, query string, args ...any) ([]map[string]any, error)

func queryRaw(qs localQueryStore, query string, args ...any) ([]map[string]any, error) {
	if queryRawForRestore != nil {
		return queryRawForRestore(qs, query, args...)
	}
	return qs.QueryRaw(query, args...)
}

// applyMirrorWriteThrough replays each applied operation's changes onto the
// cached mirror item and records the post-write state on the result item. The
// replayed item intentionally omits version fields because Zotero's advanced
// Last-Modified-Version is not threaded through mutation.ResultItem.
func applyMirrorWriteThrough(env *mutation.Envelope) {
	if env == nil || env.Result == nil {
		return
	}
	changesByOp := make(map[string][]mutation.Change, len(env.Plan.Operations))
	for _, op := range env.Plan.Operations {
		changesByOp[op.ID] = op.Changes
	}
	kindByOp := make(map[string]string, len(env.Plan.Operations))
	for _, op := range env.Plan.Operations {
		kindByOp[op.ID] = op.Kind
	}

	db, err := openExistingStoreForWrite(context.Background(), "zotio")
	if err != nil {
		// Distinguish a real mirror open failure from db==nil (not synced yet)
		// and surface the degraded local-cache update.
		warnMirrorOpenFailed(env, err)
		return
	}
	if db == nil {
		// A delete on a never-synced install cannot leave a deletion marker, so
		// the first sync can store the row the read plane still lists.
		// Deliberately not fixed by creating the store here: a command that
		// removes something must not have "and now you have a local mirror" as a
		// side effect (ADR-0007).
		if envHasAppliedPermanentDelete(env) {
			fmt.Fprintln(os.Stderr, "notice: this machine has no local mirror yet, so the permanent delete could not be recorded locally; the first `zotio sync` may still list the deleted item(s), and `zotio sync --full` clears them")
		}
		return // not synced yet — nothing to update; next sync establishes the mirror
	}
	defer db.Close()
	qs := localQueryStore{db}

	singleWrite := env.Result.Summary.Applied == 1
	for i := range env.Result.Items {
		it := &env.Result.Items[i]
		if it.Status != "applied" || it.Key == "" {
			continue
		}
		// Trash, restore and permanent delete change an item's EXISTENCE or its
		// trash membership rather than a replayable field, so applyChangeToItemData
		// cannot express them and the generic replay below bails out. Handled here
		// instead: without this, `items trash` could not see what `items delete`
		// had just trashed, and a permanent delete left a row that offline reads
		// still served and every count still included.
		switch kindByOp[it.OpID] {
		case "item_create":
			// A create has no cached row to replay onto, so the generic path
			// below bails out and the item stayed invisible to
			// `--data-source local` until the next sync (dev/roadmap.md Phase 8).
			// The op already carries the body that was sent and the engine has
			// adopted the key the write plane confirmed, which is everything a
			// row needs.
			if item, ok := mirrorCreatedItem(db, it.Key, changesByOp[it.OpID]); ok && singleWrite {
				it.Item = item
			}
			continue
		case "item_delete":
			reapMirroredItem(db, it.Key)
			continue
		case "item_trash":
			mirrorTrashedItem(db, qs, it.Key)
			continue
		case "item_restore":
			// Restore reverses a trash. When the item was synced from
			// `items-trash` with a higher version than `items`,
			// reconcileItemLifecycleTx has already deleted the live row, so the
			// local mirror looks like the item is gone. The cloud PATCH
			// succeeded, so reconstruct the live row from the cached trash
			// payload when the live row is absent; otherwise the trash-row
			// removal is sufficient and we avoid overwriting a possibly newer
			// live row with stale data.
			trashRows, err := queryRaw(qs, "SELECT data FROM resources WHERE resource_type='items-trash' AND id=?", it.Key)
			if err != nil {
				warnMirrorUpdateFailed(it.Key, err)
				// Best-effort: still try to reap a possibly stale trash row.
				if rerr := db.ReapResource("items-trash", it.Key); rerr != nil {
					warnMirrorUpdateFailed(it.Key, rerr)
				}
				continue
			}
			if len(trashRows) == 0 {
				// Never synced / already reaped: don't silently leave both tables
				// empty. Warn explicitly so the user knows the local mirror is
				// stale and a sync is needed.
				warnMirrorUpdateFailed(it.Key, fmt.Errorf("no cached trash row for %s: local mirror is stale; next sync will reconcile", it.Key))
				if rerr := db.ReapResource("items-trash", it.Key); rerr != nil {
					warnMirrorUpdateFailed(it.Key, rerr)
				}
				continue
			}
			raw := json.RawMessage(sqlStringValue(trashRows[0]["data"]))
			var item map[string]any
			if err := json.Unmarshal([]byte(raw), &item); err != nil {
				warnMirrorUpdateFailed(it.Key, err)
				if rerr := db.ReapResource("items-trash", it.Key); rerr != nil {
					warnMirrorUpdateFailed(it.Key, rerr)
				}
				continue
			}
			// Clear the deleted marker — may appear top-level or under data.
			delete(item, "deleted")
			if data, ok := item["data"].(map[string]any); ok {
				delete(data, "deleted")
			}
			// Drop stale version metadata the same way normal write-through does;
			// the advanced Web version is not available here.
			dropStaleItemVersion(item)
			// Only reinstate when the live row is missing (sync-reconciled
			// case). When mirrorTrashedItem copied the item into trash without
			// deleting the live row, that row may be newer than the trashed
			// copy; blindly UpsertKeyed would overwrite it with stale data.
			// Check first so the common write-through trash path stays a
			// no-op apart from dropping the trash row.
			liveRows, err := queryRaw(qs, "SELECT id FROM resources WHERE resource_type='items' AND id=?", it.Key)
			if err != nil {
				warnMirrorUpdateFailed(it.Key, err)
				// Do NOT reap trash on live-row query error. The query never
				// established whether a live row exists; reaping would leave
				// neither table when live is absent — the exact
				// read-your-writes violation a13b50b fixed, reintroduced on
				// the error path. Retaining trash is recoverable by sync;
				// losing both rows makes the item invisible locally.
				continue
			}
			if len(liveRows) == 0 {
				restored, err := json.Marshal(item)
				if err != nil {
					warnMirrorUpdateFailed(it.Key, err)
					continue
				}
				if err := db.RestoreMirroredItem(it.Key, restored); err != nil {
					warnMirrorUpdateFailed(it.Key, err)
					continue
				}
				// Defect A: cached trash payload may be stale if the item
				// was edited on the server while trashed. item_restore's
				// Apply returns nil (no post-write payload from the write
				// plane is reachable here), so the mirror may hold pre-trash
				// field values. Emit an explicit degraded-cache warning
				// rather than silently claiming the mirror was updated.
				warnMirrorUpdateFailed(it.Key, fmt.Errorf("restored %s from cached trash payload; mirror may hold pre-trash field values pending sync; next sync will reconcile", it.Key))
				continue
			}
			if err := db.ReapResource("items-trash", it.Key); err != nil {
				warnMirrorUpdateFailed(it.Key, err)
			}
			continue
		}
		item, ok, err := replayItemChanges(qs, it.Key, changesByOp[it.OpID])
		if err != nil {
			warnMirrorUpdateFailed(it.Key, err)
			continue
		}
		if !ok {
			continue // unsupported change shape — leave for sync to reconcile
		}
		// Avoid surfacing or caching stale pre-write Zotero versions; the Web
		// API's advanced version is not available here.
		dropStaleItemVersion(item)
		raw, err := json.Marshal(item)
		if err != nil {
			warnMirrorUpdateFailed(it.Key, err)
			continue
		}
		if _, err := db.UpsertKeyed("items", []string{it.Key}, []json.RawMessage{raw}); err != nil {
			warnMirrorUpdateFailed(it.Key, err)
			continue
		}
		// The read plane (local desktop API) does not know about this write until
		// Zotero syncs it down from zotero.org. Without a marker the next `sync`
		// re-applies the pre-write copy and rolls the mirror back.
		recordPendingWrite(db, it.Key, changesByOp[it.OpID])
		// Read-your-writes: return the post-write state so a targeted write needs
		// no follow-up read. Suppressed for a batch — a 53-group tag merge emitted
		// the full item JSON per op, megabytes of payload the caller already has,
		// and it only needs the keys.
		if singleWrite {
			it.Item = item
		}
	}
}

// mirrorCreatedItem writes the row for an item this run just created, so a
// `--data-source local` read sees it without waiting for a sync. It returns the
// stored item object and whether a row was written.
//
// The payload is the item body the create actually sent, carried on the op's
// "item" change (itemsCreatePreflightOps, runSingleItemCreate), plus the key the
// write plane confirmed. Both create routes record that same change, so the
// mirrored row does not depend on which route ran.
//
// The row deliberately carries NO version. Zotero assigns one this process never
// sees, and upsertGenericResourceTx's version-monotonic guard accepts any
// incoming row over a version-less one -- so the first sync that lists the item
// replaces this row with the server's authoritative copy. That is the same
// property Store.ClearResourceVersions relies on, and the reason
// dropStaleItemVersion strips the version from every other replayed row.
// dateAdded, dateModified and meta are server-assigned; they stay absent rather
// than being fabricated from this machine's clock, so a local read reports what
// was created and nothing zotio cannot know.
func mirrorCreatedItem(db *store.Store, key string, changes []mutation.Change) (map[string]any, bool) {
	data, ok := createdItemData(changes)
	if !ok {
		return nil, false // no recorded body — the next sync reconciles the create
	}
	data["key"] = key
	item := map[string]any{"key": key, "data": data}
	raw, err := json.Marshal(item)
	if err != nil {
		warnMirrorUpdateFailed(key, err)
		return nil, false
	}
	// A deletion marker for this key would make reconcilePendingWrites drop the
	// row from every synced page for as long as the marker stands, suppressing
	// an object that now exists. Nothing retires a deletion marker on a clock —
	// only a total-observation pass that stops listing the key does (ADR-0007) —
	// so without this the suppression would outlast the create indefinitely on
	// an installation that never runs a full sync. A create the write plane
	// confirmed under the key is strictly newer evidence than a marker saying
	// the key is gone, so the marker is retired here. Zotero mints item keys and
	// does not reuse them, so no real create can land on a purged key; the two
	// DELETEs are cheap and the failure they rule out — an item that silently
	// never mirrors — is invisible to the user until it is chased down.
	for _, resource := range []string{"items", "items-trash"} {
		if err := db.ClearPendingWrite(resource, key); err != nil {
			warnMirrorUpdateFailed(key, err)
		}
	}
	if _, err := db.UpsertKeyed("items", []string{key}, []json.RawMessage{raw}); err != nil {
		warnMirrorUpdateFailed(key, err)
		return nil, false
	}
	// The read plane does not list the item until Zotero syncs it down, so the
	// create is an unconfirmed local write like any other: the marker makes it
	// visible to doctor and retires itself on the first page that carries the
	// key, because an "item" change is not replayable and reconcilePendingWrites
	// then accepts the read plane's authoritative copy.
	recordPendingWrite(db, key, changes)
	return item, true
}

// createdItemData copies the created item's body out of the op's changes. The
// copy is load-bearing: that map is the op's own recorded change, shared with
// the rendered plan and the journal entry, and the mirrored row has to add its
// own "key" without editing either.
//
// Fail-closed on anything that is not a Zotero item body. An "item" change does
// not always carry one: import file --via connector records a per-record
// descriptor ({file, via, record}) because the desktop translator, not zotio,
// produces the items. Mirroring that would store a row whose data has no Zotero
// fields at all, which offline reads and every mirror-derived count would then
// serve as an item. itemType is the discriminator because Zotero requires it on
// every item and the store indexes it ($.data.itemType); a body without one
// could not produce a usable row anyway, so it is skipped and the next sync
// reconciles the create as it did before.
func createdItemData(changes []mutation.Change) (map[string]any, bool) {
	for _, change := range changes {
		body, ok := change.Add.(map[string]any)
		if change.Field != "item" || !ok {
			continue
		}
		if itemType, ok := body["itemType"].(string); !ok || itemType == "" {
			return nil, false
		}
		data := make(map[string]any, len(body)+1)
		for field, value := range body {
			data[field] = value
		}
		return data, true
	}
	return nil, false
}

// reapMirroredItem removes an item the caller permanently deleted, from both the
// item mirror and the trash mirror. Nothing did this before, so a row survived
// its own object: offline reads served items that 404 on both planes, and every
// mirror-derived count included them.
//
// One store call, not two. Both lifecycle rows go in a single transaction under
// the store's write lock, so no reader observes the item gone from items while
// it is still in items-trash, and a failure cannot leave the trash row behind.
//
// The same transaction records a deletion marker per canonical resource, which
// is what stops the resurrection this reap used to leave open: the delete goes
// to api.zotero.org while sync reads the local desktop API, so until Zotero
// syncs the delete down the read plane keeps listing the item and any sync in
// that window re-inserted it. reconcilePendingWrites now drops a listed row
// whose key is marked deleted (ADR-0007).
func reapMirroredItem(db *store.Store, key string) {
	if err := db.ReapMirroredItem(key); err != nil {
		warnMirrorUpdateFailed(key, err)
	}
}

// writePlaneAbsence classifies what this machine's mirror knows about a key the
// write plane answers 404 for. That one status code covers three states, and
// they need three different reports.
//
// ADR-0007 reasoned about one direction of the plane lag: a write that landed
// on api.zotero.org and has not reached the desktop yet. This is the INVERSE
// window, which it did not cover. An item created through the desktop
// connector exists on the read plane, and in this mirror, while api.zotero.org
// has never heard of its key (~15-20s observed), so a delete routed to the Web
// API 404s for an item that is demonstrably alive. A repeated permanent delete
// 404s for the opposite reason: the key is gone there because an earlier run of
// this very command removed it.
type writePlaneAbsence int

const (
	// absenceUnknown: the mirror has nothing to say — no mirror file, no row,
	// no marker, or a mirror read that failed. The 404 stands on its own and is
	// reported as it always was.
	absenceUnknown writePlaneAbsence = iota
	// absencePurgedHere: a deletion marker exists, so THIS installation already
	// applied a permanent delete for the key against the write plane — a marker
	// is written only after that plane reported the delete applied (ADR-0007).
	// The 404 is that delete seen a second time, and the mirror is already in
	// the purged state, so the honest report is an idempotent no-op.
	absencePurgedHere
	// absenceMirroredOnly: a canonical row exists and carries no deletion
	// marker, so the key is live on the read plane and absent from the write
	// plane. Either it has not propagated up yet, or the mirror row is stale;
	// both are failures, and both leave the mirror untouched.
	absenceMirroredOnly
)

// classifyWritePlaneAbsence reads the mirror for one key's absence evidence.
//
// It runs only where write-through itself runs: mirrorWriteThrough is installed
// by the production entry points (root.go, cmd/zotio-mcp) and left nil by unit
// tests that drive commands without a mirror. That gate is the rule, not a test
// convenience — the classification exists to keep the reported status and the
// mirror in agreement, so where no mirror is being maintained there is nothing
// to agree with and the plain 404 is the whole truth.
//
// Every failure answers absenceUnknown. A mirror this path cannot read is not
// evidence, and a permanent delete must never soften a 404 on a guess.
func classifyWritePlaneAbsence(key string) writePlaneAbsence {
	if mirrorWriteThrough == nil || key == "" {
		return absenceUnknown
	}
	db, err := openExistingStoreForWrite(context.Background(), "zotio")
	if err != nil || db == nil {
		return absenceUnknown
	}
	defer db.Close()
	// The marker outranks the row: a purge reaps both canonical rows in the
	// same transaction as it writes the markers, so the two states cannot be
	// confused, and asking about the marker first keeps a resurrected row from
	// masking a confirmed delete.
	for _, resource := range []string{"items", "items-trash"} {
		marked, err := db.PendingDeletion(resource, key)
		if err != nil {
			return absenceUnknown
		}
		if marked {
			return absencePurgedHere
		}
	}
	for _, resource := range []string{"items", "items-trash"} {
		if _, err := db.Get(resource, key); err == nil {
			return absenceMirroredOnly
		}
	}
	return absenceUnknown
}

// mirrorTrashedItem copies an item's mirrored row into the trash mirror, so
// `items trash` can see it before Zotero has synced the trash down.
//
// The write lands on api.zotero.org while reads go to the local desktop API,
// which does not report the trash for ~15s. That window is precisely when a
// caller runs `items trash` to confirm what it just did.
func mirrorTrashedItem(db *store.Store, qs localQueryStore, key string) {
	rows, err := qs.QueryRaw("SELECT data FROM resources WHERE resource_type='items' AND id=?", key)
	if err != nil {
		warnMirrorUpdateFailed(key, err)
		return
	}
	if len(rows) == 0 {
		return // not mirrored yet; the next sync establishes it
	}
	raw := json.RawMessage(sqlStringValue(rows[0]["data"]))
	// Deliberately does not reap the live row. In Zotero a trashed item still
	// exists in the library with deleted=1, so `items get` must keep resolving
	// it; only a permanent delete removes the row (see reapMirroredItem). The
	// store's reconcileItemLifecycleTx arbitrates which of items/items-trash
	// survives once a synced payload carries a newer version.
	if _, err := db.UpsertKeyed("items-trash", []string{key}, []json.RawMessage{raw}); err != nil {
		warnMirrorUpdateFailed(key, err)
	}
}

// replayItemChanges loads the cached mirror item, applies the changes to its
// inner data, and returns the full updated item object. ok=false when the item
// is not in the mirror or a change can't be confidently replayed. A create no
// longer reaches here: mirrorCreatedItem writes its row from the op's own body.
// Errors are reserved for real local-mirror failures/corruption that should be
// warned about without failing the already-successful cloud write.
func replayItemChanges(qs localQueryStore, key string, changes []mutation.Change) (map[string]any, bool, error) {
	if len(changes) == 0 {
		return nil, false, nil
	}
	rows, err := qs.QueryRaw("SELECT data FROM resources WHERE resource_type='items' AND id=?", key)
	if err != nil {
		return nil, false, err
	}
	if len(rows) == 0 {
		return nil, false, nil
	}
	var item map[string]any
	if err := json.Unmarshal([]byte(sqlStringValue(rows[0]["data"])), &item); err != nil {
		return nil, false, err
	}
	data, ok := item["data"].(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("cached item %s has no data object", key)
	}
	for _, c := range changes {
		if !applyChangeToItemData(data, c) {
			return nil, false, nil // unsupported (e.g. merge []string add, trash) — abort this item
		}
	}
	item["data"] = data
	return item, true, nil
}

// envHasAppliedPermanentDelete reports whether the run purged at least one item.
// It answers a yes/no question rather than returning the keys because the notice
// it gates is emitted once per run: a 50-item purge has one cause and one
// remedy, and 50 identical lines would bury the command's real output.
func envHasAppliedPermanentDelete(env *mutation.Envelope) bool {
	if env == nil || env.Result == nil {
		return false
	}
	purges := make(map[string]bool, len(env.Plan.Operations))
	for _, op := range env.Plan.Operations {
		if op.Kind == "item_delete" {
			purges[op.ID] = true
		}
	}
	for _, item := range env.Result.Items {
		if item.Status == "applied" && purges[item.OpID] {
			return true
		}
	}
	return false
}

func warnMirrorOpenFailed(env *mutation.Envelope, err error) {
	if env == nil || env.Result == nil {
		return
	}
	for _, it := range env.Result.Items {
		if it.Status == "applied" && it.Key != "" {
			warnMirrorUpdateFailed(it.Key, err)
		}
	}
}

func warnMirrorUpdateFailed(key string, err error) {
	fmt.Fprintf(os.Stderr, "warning: read-your-writes mirror update failed for %s: %v\n", key, err)
}

func dropStaleItemVersion(item map[string]any) {
	delete(item, "version")
	if data, ok := item["data"].(map[string]any); ok {
		delete(data, "version")
		delete(data, "dateModified")
	}
}

// mirrorReplayableFields is a conservative allowlist of Zotero item fields
// applyChangeToItemData will replay as a scalar set/clear onto the local
// mirror. It's derived from the fields this CLI's own write paths actually
// set directly: items_update.go's --title/--abstract-note/--extra flags,
// items_enrich.go's provenance and --missing-pdf url writes, and
// items_preprint_check_fix.go's DOI corrections -- not from the full,
// item-type-dependent Zotero schema (see schema_item-fields.go), which
// varies per itemType and isn't resolvable offline at replay time.
//
// Anything absent here -- including every producer-invented or mis-scoped
// field name -- is deliberately left unreplayed for the next `sync` to
// reconcile authoritatively. Keep this list narrow: a field missing from it
// just waits one sync cycle to catch up; a field wrongly present in it can
// silently corrupt the mirror, which agents then trust as confirmed
// post-write state (see it.Item in applyMirrorWriteThrough).
var mirrorReplayableFields = map[string]bool{
	"title":            true,
	"abstractNote":     true,
	"extra":            true,
	"DOI":              true,
	"url":              true,
	"date":             true,
	"shortTitle":       true,
	"language":         true,
	"rights":           true,
	"publicationTitle": true,
	"volume":           true,
	"issue":            true,
	"pages":            true,
	"series":           true,
	"place":            true,
	"publisher":        true,
	"ISBN":             true,
	"ISSN":             true,
	"callNumber":       true,
	"archive":          true,
	"archiveLocation":  true,
	"libraryCatalog":   true,
	"accessDate":       true,
}

// mirrorIdentityFields are bookkeeping/identity fields that must never be
// replayed onto the mirror even if some producer names them: they aren't
// user-editable content, and blindly overwriting them (e.g. "key",
// "version") would desync the mirror's own bookkeeping from the API rather
// than reflect the write. Checked explicitly, in addition to being absent
// from mirrorReplayableFields, so the rejection survives future edits to
// the allowlist above.
var mirrorIdentityFields = map[string]bool{
	"key":          true,
	"version":      true,
	"itemType":     true,
	"dateAdded":    true,
	"dateModified": true,
	"deleted":      true,
	"parentItem":   true,
	"relations":    true,
	"library":      true,
	"links":        true,
	"meta":         true,
}

// applyChangeToItemData forward-applies one change to an item's data map. It
// handles tag/collection membership (scalar values only), creator display-name
// renames over the ordered creators array, and scalar field set/clear for a
// conservative, explicit allowlist of known Zotero item fields; anything else
// (bulk []string adds, "deleted"/trash, structural edits, unrecognized or
// identity field names) returns false so the caller skips write-through for
// that item and leaves it for the next `sync`.
func applyChangeToItemData(data map[string]any, c mutation.Change) bool {
	switch c.Field {
	case "tags":
		return applyTagChangeToData(data, c)
	case "collections":
		return applyCollectionChangeToData(data, c)
	case "creators":
		return applyCreatorRenameChangeToData(data, c)
	default:
		// Fail-closed: replay only fields this CLI knows to be directly-settable
		// Zotero item fields (mirrorReplayableFields), and never identity or
		// bookkeeping fields even if a producer names them explicitly. A field
		// left out of the allowlist is not an error -- it's simply skipped here
		// and self-heals on the next `sync`. That's strictly safer than the
		// alternative: a wrongly (or wrongly-scoped) replayed value sits in the
		// mirror silently wrong until the next sync, and in the meantime is
		// handed to agents as confirmed post-write state via it.Item in
		// applyMirrorWriteThrough -- read-your-writes becomes read-your-corruption.
		if mirrorIdentityFields[c.Field] || !mirrorReplayableFields[c.Field] {
			return false
		}
		if c.Add != nil {
			s, ok := c.Add.(string)
			if !ok {
				return false
			}
			data[c.Field] = s
			return true
		}
		if c.Remove != nil {
			if _, ok := c.Remove.(string); !ok {
				return false
			}
			data[c.Field] = ""
			return true
		}
		return true
	}
}

func applyTagChangeToData(data map[string]any, c mutation.Change) bool {
	tags, _ := data["tags"].([]any)
	if c.Add != nil {
		name, ok := c.Add.(string)
		if !ok {
			return false
		}
		present := false
		for _, t := range tags {
			if m, ok := t.(map[string]any); ok && m["tag"] == name {
				present = true
				break
			}
		}
		if !present {
			tag := map[string]any{"tag": name}
			if c.TagType != 0 {
				tag["type"] = c.TagType
			}
			tags = append(tags, tag)
		}
	}
	if c.Remove != nil {
		name, ok := c.Remove.(string)
		if !ok {
			return false
		}
		kept := make([]any, 0, len(tags))
		for _, t := range tags {
			if m, ok := t.(map[string]any); ok && m["tag"] == name {
				if c.TagType == 0 || itemTagType(m) == c.TagType {
					continue
				}
			}
			kept = append(kept, t)
		}
		tags = kept
	}
	data["tags"] = tags
	return true
}

func applyCreatorRenameChangeToData(data map[string]any, c mutation.Change) bool {
	oldName, ok := c.Remove.(string)
	if !ok || oldName == "" {
		return false
	}
	newName, ok := c.Add.(string)
	if !ok || newName == "" {
		return false
	}
	rawCreators, _ := data["creators"].([]any)
	renamed := make([]any, 0, len(rawCreators))
	changed := false
	for _, rawCreator := range rawCreators {
		creator, ok := rawCreator.(map[string]any)
		if !ok {
			renamed = append(renamed, rawCreator)
			continue
		}
		copied := copyCreatorObject(creator)
		if creatorDisplayNameFromObject(copied) == oldName {
			rewriteCreatorDisplayName(copied, newName)
			changed = true
		}
		renamed = append(renamed, copied)
	}
	if !changed {
		return false
	}
	data["creators"] = renamed
	return true
}

func applyCollectionChangeToData(data map[string]any, c mutation.Change) bool {
	cols, _ := data["collections"].([]any)
	if c.Add != nil {
		name, ok := c.Add.(string)
		if !ok {
			return false
		}
		present := false
		for _, v := range cols {
			if s, ok := v.(string); ok && s == name {
				present = true
				break
			}
		}
		if !present {
			cols = append(cols, name)
		}
	}
	if c.Remove != nil {
		name, ok := c.Remove.(string)
		if !ok {
			return false
		}
		kept := make([]any, 0, len(cols))
		for _, v := range cols {
			if s, ok := v.(string); ok && s == name {
				continue
			}
			kept = append(kept, v)
		}
		cols = kept
	}
	data["collections"] = cols
	return true
}
