// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package mutation

import (
	"testing"
)

// Rows is the operator-facing mutation summary: failures and destructive ops
// first, with state labels that must stay honest while the JSON envelope and
// engine tests stay green. Reason rendering is pinned elsewhere; these cases
// pin priority ordering and the planned/no-op/destructive labels.
func TestRowsPreviewOrdersDestructiveFirstWithStateLabels(t *testing.T) {
	env := Envelope{
		Plan: Plan{Operations: []Op{
			{ID: "plan", Key: "K2", Kind: "tag", Changes: []Change{{Field: "tags"}}},
			{ID: "noop", Key: "K3", Kind: "tag"},
			{ID: "wipe", Key: "K1", Kind: "tag", Changes: []Change{{Field: "tags"}}, Destructive: true},
		}},
	}

	got := Rows(env)
	want := []string{
		"  [destructive] tag K1",
		"  [planned] tag K2",
		"  [no_op] tag K3",
	}
	if len(got) != len(want) {
		t.Fatalf("Rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Rows = %v, want %v", got, want)
		}
	}
}

func TestRowsResultPrioritizesFailuresAndKeepsReasons(t *testing.T) {
	env := Envelope{
		Plan: Plan{Operations: []Op{
			{ID: "ok", Key: "K1"},
			{ID: "boom", Key: "K2"},
			{ID: "clash", Key: "K3"},
			{ID: "skip", Key: "K4"},
			{ID: "burn", Key: "K5", Destructive: true},
		}},
		Result: &Result{Items: []ResultItem{
			{OpID: "ok", Key: "K1", Status: "applied"},
			{OpID: "boom", Key: "K2", Status: "failed", Reason: "boom"},
			{OpID: "clash", Key: "K3", Status: "conflict", Reason: map[string]any{"message": "server says no"}},
			{OpID: "skip", Key: "K4", Status: "not_attempted"},
			{OpID: "burn", Key: "K5", Status: "applied"},
		}},
	}

	got := Rows(env)
	// failed (0), conflict (1), then the priority-2 pair in input order:
	// not_attempted keeps its label, and the destructive applied op is
	// promoted to 2 without relabeling its status.
	want := []string{
		"  [failed] K2 — boom",
		"  [conflict] K3 — server says no",
		"  [not_attempted] K4",
		"  [applied] K5",
		"  [applied] K1",
	}
	if len(got) != len(want) {
		t.Fatalf("Rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Rows = %v, want %v", got, want)
		}
	}
}
