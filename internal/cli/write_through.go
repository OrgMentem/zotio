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
)

// mirrorWriteThrough, when non-nil, updates the local mirror from applied writes.
// Set only on the real Execute() path so unit tests driving runMutation directly
// don't touch the filesystem (mirrors the journal-recorder pattern).
var mirrorWriteThrough func(env *mutation.Envelope)

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

	for i := range env.Result.Items {
		it := &env.Result.Items[i]
		if it.Status != "applied" || it.Key == "" {
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
		it.Item = item // read-your-writes: post-write state in the envelope
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

// applyChangeToItemData forward-applies one change to an item's data map. It
// handles tag/collection membership (scalar values only), creator display-name
// renames over the ordered creators array, and scalar field set/clear; anything
// else (bulk []string adds, "deleted"/trash, structural edits) returns false so
// the caller skips write-through for that item.
func applyChangeToItemData(data map[string]any, c mutation.Change) bool {
	switch c.Field {
	case "tags":
		return applyTagChangeToData(data, c)
	case "collections":
		return applyCollectionChangeToData(data, c)
	case "creators":
		return applyCreatorRenameChangeToData(data, c)
	default:
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
