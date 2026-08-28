// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// journal undo. Reversibility is decided per change
// field: tag/collection membership inverts cleanly (Add<->Remove); field
// overwrites (DOI, abstract), renames, merges, and deletions are refused because
// the prior value was not (or cannot be) captured. An item create is reversed
// by moving the recorded created key to the trash.

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
func InvertChange(c Change) (Change, bool) {
	if !reversibleFields[c.Field] {
		return Change{}, false
	}
	if !isScalarMembershipValue(c.Add) || !isScalarMembershipValue(c.Remove) {
		return Change{}, false
	}
	return Change{Field: c.Field, Add: c.Remove, Remove: c.Add, TagType: c.TagType}, true
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
	for _, op := range entry.Ops {
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
		if op.Kind == "item_create" {
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
		inv := make([]Change, 0, len(op.Changes))
		reason := ""
		for _, ch := range op.Changes {
			ic, ok := InvertChange(ch)
			if !ok {
				reason = fmt.Sprintf("change on field %q is not reversible", ch.Field)
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
