// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// journal undo. Reversibility is decided per change
// field: tag/collection membership inverts cleanly (Add<->Remove) when a change
// adds or removes one value; field overwrites (DOI, abstract), merges, and
// deletions are refused because the prior value was not (or cannot be)
// captured, and so is a change that replaces one membership value with another
// (a rename), because inverting it would remove a value the item may have
// held before the run. An op may instead record its own inverse at write time
// (JournalOp.Revert), which is then replayed as is.
// An item create (item_create, import_create) is reversed by moving the
// recorded created key to the trash; a resolver attachment_create on that same
// key rides along, because trashing a parent trashes its child attachments.

package mutation

import "fmt"

// reversibleFields are the only change fields a recorded op may touch to be
// undoable: set-membership toggles whose inverse is unambiguous and lossless.
var reversibleFields = map[string]bool{"tags": true, "collections": true}

// InvertChange returns the inverse of a membership change (Add<->Remove) and
// whether the change is reversible at all. Only single-value (scalar string)
// toggles invert: a non-string Add/Remove (e.g. a []string bulk add recorded by
// a duplicate-merge) is NOT a simple per-item toggle and must not be inverted —
// inverting it would target the wrong item, so such ops are refused by InverseOps.
// A change that sets both Add and Remove replaces one value with another and
// does not invert either: the Add side may already have been a member before
// the write, and the inverse's Remove would then delete it.
func InvertChange(c Change) (Change, bool) {
	if !reversibleFields[c.Field] || isReplacePair(c) {
		return Change{}, false
	}
	if !isScalarMembershipValue(c.Add) || !isScalarMembershipValue(c.Remove) {
		return Change{}, false
	}
	return Change{Field: c.Field, Add: c.Remove, Remove: c.Add, TagType: c.TagType}, true
}

// isReplacePair reports whether a change adds one value and removes another in
// the same step.
func isReplacePair(c Change) bool {
	return c.Add != nil && c.Remove != nil
}

// isScalarMembershipValue reports whether a change value is a single membership
// toggle (a string) or absent (nil). Bulk/non-string values are not reversible.
func isScalarMembershipValue(v any) bool {
	if v == nil {
		return true
	}
	_, ok := v.(string)
	return ok
}

// ReversalRefusal explains why one recorded op cannot be undone.
type ReversalRefusal struct {
	OpID   string `json:"op_id"`
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// InverseOps derives the inverse operations for reversible writes in a journal
// entry, and a refusal list for everything that changed state but cannot be
// undone safely. Ordinary no-op/conflict/failed operations changed nothing and
// are skipped. A non-applied operation whose reason says `committed: true` is
// an unresolved write, so it becomes an explicit refusal with its recovery
// evidence instead of disappearing as "nothing reversible".
func InverseOps(entry JournalEntry) (inverse []Op, refused []ReversalRefusal) {
	trashed := map[string]bool{}
	for _, op := range entry.Ops {
		if op.Status == "applied" && IsCreateKind(op.Kind) && op.Key != "" {
			trashed[op.Key] = true
		}
	}
	for _, op := range entry.Ops {
		// Whatever a resolver attachment did, trashing its parent covers it,
		// including a failed resolver whose parent committed.
		if op.Kind == "attachment_create" && trashed[op.Key] {
			continue
		}
		if op.Status != "applied" {
			if detail, ok := op.Reason.(map[string]any); ok && detail["committed"] == true {
				refused = append(refused, ReversalRefusal{
					OpID: op.ID, Key: op.Key, Kind: op.Kind,
					Reason: fmt.Sprintf("write committed but cannot be undone without confirmed keys; title=%v session=%v connector_key=%v attachment_marker=%v: %v",
						detail["title"], detail["session"], detail["connector_key"], detail["attachment_marker"], detail["message"]),
				})
			}
			continue
		}
		if len(op.Changes) == 0 {
			continue
		}
		if IsCreateKind(op.Kind) {
			if op.Key == "" {
				refused = append(refused, ReversalRefusal{
					OpID: op.ID, Key: op.Key, Kind: op.Kind,
					Reason: "created Zotero key was not recorded; inspect the library and run `zotio items delete <REAL_KEY> --yes`",
				})
				continue
			}
			inverse = append(inverse, Op{
				ID:      "undo:" + op.ID,
				Key:     op.Key,
				Kind:    "undo.item_create",
				Changes: []Change{{Field: "deleted", Add: true}},
			})
			continue
		}
		// Trash and restore are a lossless toggle of one flag, so each inverts
		// by kind. "deleted" stays out of reversibleFields: a permanent
		// item_delete records the same field and must keep refusing.
		if op.Kind == "item_trash" || op.Kind == "item_restore" {
			if op.Key == "" {
				refused = append(refused, ReversalRefusal{OpID: op.ID, Key: op.Key, Kind: op.Kind, Reason: "item key was not recorded"})
				continue
			}
			ch := Change{Field: "deleted", Add: true}
			if op.Kind == "item_trash" {
				ch = Change{Field: "deleted", Remove: true}
			}
			inverse = append(inverse, Op{
				ID:      "undo:" + op.ID,
				Key:     op.Key,
				Kind:    "undo." + op.Kind,
				Changes: []Change{ch},
			})
			continue
		}
		// An op that captured its own inverse while it wrote (a field's prior
		// value, the Extra line it appended) is reversed from that record: its
		// planned Changes cannot be inverted on their own.
		if len(op.Revert) > 0 {
			inverse = append(inverse, Op{
				ID:      "undo:" + op.ID,
				Key:     op.Key,
				Kind:    "undo." + op.Kind,
				Changes: op.Revert,
			})
			continue
		}
		inv := make([]Change, 0, len(op.Changes))
		reason := ""
		for _, ch := range op.Changes {
			ic, ok := InvertChange(ch)
			if !ok {
				reason = fmt.Sprintf("change on field %q is not reversible", ch.Field)
				if reversibleFields[ch.Field] && isReplacePair(ch) {
					reason = fmt.Sprintf("change on field %q replaces %v with %v and recorded no inverse; undo cannot tell whether %v was already there before the run", ch.Field, ch.Remove, ch.Add, ch.Add)
				}
				break
			}
			inv = append(inv, ic)
		}
		if reason != "" {
			refused = append(refused, ReversalRefusal{OpID: op.ID, Key: op.Key, Kind: op.Kind, Reason: reason})
			continue
		}
		inverse = append(inverse, Op{
			ID:      "undo:" + op.ID,
			Key:     op.Key,
			Kind:    "undo." + op.Kind,
			Changes: inv,
		})
	}
	return inverse, refused
}

// IsCreateKind reports whether an op kind creates a new top-level item whose
// Zotero key is only known once Apply returns it. Such ops never plan a real
// key, and their inverse is a guarded move to the trash.
func IsCreateKind(kind string) bool {
	return kind == "item_create" || kind == "import_create"
}
