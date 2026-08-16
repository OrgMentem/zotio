// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// After a write succeeds in the cloud via the Web API, write-through replays the
// just-applied changes onto the local SQLite mirror so `--data-source local`
// reads-your-own-writes WITHOUT a `sync`, and surfaces the resulting item state
// in the mutation envelope so agents need no follow-up read. Best-effort: changes
// it can't confidently replay (merges, trash, creates) are left for the next
// `sync` to reconcile authoritatively.

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
			continue // create / unsupported change shape — leave for sync to reconcile
		}
		// Avoid surfacing or caching stale pre-write Zotero versions; the Web
		// API's advanced version is not available here.
		dropStaleItemVersion(item)
		raw, err := json.Marshal(item)
		if err != nil {
			warnMirrorUpdateFailed(it.Key, err)
			continue
		}
		if err := db.UpsertKeyed("items", []string{it.Key}, []json.RawMessage{raw}); err != nil {
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

// reapMirroredItem removes an item the caller permanently deleted, from both the
// item mirror and the trash mirror. Nothing did this before, so a row survived
// its own object: offline reads served items that 404 on both planes, and every
// mirror-derived count included them.
func reapMirroredItem(db *store.Store, key string) {
	if err := db.ReapResource("items", key); err != nil {
		warnMirrorUpdateFailed(key, err)
		return
	}
	// A permanently deleted item cannot be in the trash either.
	if err := db.ReapResource("items-trash", key); err != nil {
		warnMirrorUpdateFailed(key, err)
	}
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
	if err := db.UpsertKeyed("items-trash", []string{key}, []json.RawMessage{raw}); err != nil {
		warnMirrorUpdateFailed(key, err)
	}
}

// replayItemChanges loads the cached mirror item, applies the changes to its
// inner data, and returns the full updated item object. ok=false when the item
// is not in the mirror (a create) or a change can't be confidently replayed.
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
