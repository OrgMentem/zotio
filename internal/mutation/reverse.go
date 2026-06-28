// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase3): journal undo. Reversibility is decided per change
// field: tag/collection membership inverts cleanly (Add<->Remove); field
// overwrites (DOI, abstract), renames, merges, and deletions are refused because
// the prior value was not (or cannot be) captured.

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
	return Change{Field: c.Field, Add: c.Remove, Remove: c.Add}, true
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

// InverseOps derives the inverse operations for the applied, reversible ops in a
// journal entry, and a refusal list for ops that cannot be safely reversed. Only
// ops recorded with status "applied" are considered (a no-op/conflict/failed op
// changed nothing). The returned ops carry inverted Changes but no Apply closure;
// the caller attaches the apply step.
func InverseOps(entry JournalEntry) (inverse []Op, refused []ReversalRefusal) {
	for _, op := range entry.Ops {
		if op.Status != "applied" || len(op.Changes) == 0 {
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
