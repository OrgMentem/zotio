// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"testing"
)

// The JSON/text parity test asserts RecommendedAction commands but never
// inspects the per-alias evidence map: a regression that always emits
// rename_command (leaking a runnable command for unsafe aliases into
// agent-consumed evidence) or never emits it would pass CI. These cases pin
// the runnable gate on the evidence itself.
func TestCreatorVariantFindingEvidenceRenameCommandGate(t *testing.T) {
	group := creatorVariantGroup{
		Tier:               creatorVariantTierExact,
		Canonical:          "Adam J. Rock",
		CanonicalItemCount: 3,
		TotalItems:         5,
		Aliases: []creatorVariantAlias{
			{
				Name:          "Adam J Rock",
				ItemCount:     2,
				ItemKeys:      []string{"A1", "A2"},
				RenameCommand: `zotio creators rename --from 'Adam J Rock' --to 'Adam J. Rock'`,
				Unsafe:        false,
			},
			{
				Name:      "A Rock",
				ItemCount: 1,
				ItemKeys:  []string{"B1"},
				Unsafe:    true,
			},
		},
	}

	evidence := creatorVariantFindingEvidence(group)
	aliases, ok := evidence["aliases"].([]map[string]any)
	if !ok || len(aliases) != 2 {
		t.Fatalf("evidence aliases = %#v, want 2 alias maps", evidence["aliases"])
	}

	runnable := aliases[0]
	if got := runnable["rename_command"]; got != group.Aliases[0].RenameCommand {
		t.Errorf("runnable rename_command = %#v, want the alias command", got)
	}
	for _, key := range []string{"item_count", "unsafe", "name", "item_keys"} {
		if _, ok := runnable[key]; !ok {
			t.Errorf("runnable alias evidence missing %q: %#v", key, runnable)
		}
	}

	unsafe := aliases[1]
	if got, ok := unsafe["rename_command"]; ok {
		t.Errorf("unsafe alias carries rename_command = %#v, want the key absent", got)
	}
	if got := unsafe["unsafe"]; got != true {
		t.Errorf("unsafe alias unsafe flag = %#v, want true", got)
	}
	if got := unsafe["item_count"]; got != 1 {
		t.Errorf("unsafe alias item_count = %#v, want 1", got)
	}
}

// creatorPunctuationCount feeds the canonical-name comparator after fullness,
// initial-period, case-quality, and item-count scores tie. A regression that
// drops the tie-breaker (or inverts it) would silently change which spelling
// the audit blesses as canonical, so pin both the counts and the ordering.
func TestCreatorPunctuationCount(t *testing.T) {
	cases := []struct {
		name  string
		count int
	}{
		{"Ann Lee", 0},
		{"Ann-Lee", 1},
		{"Ann, Lee", 1},
		{"Ann.. Lee", 2},
		{`"Ann" Lee`, 2},
	}
	for _, tc := range cases {
		if got := creatorPunctuationCount(tc.name); got != tc.count {
			t.Errorf("creatorPunctuationCount(%q) = %d, want %d", tc.name, got, tc.count)
		}
	}
}

func TestCreatorCanonicalLessPrefersTidierPunctuation(t *testing.T) {
	newVariant := func(name string) *creatorNameVariant {
		return &creatorNameVariant{Name: name, itemKeys: map[string]bool{"K1": true}}
	}
	tidy, noisy := newVariant("Ann Lee"), newVariant("Ann, Lee")
	if !creatorVariantCanonicalLess(tidy, noisy) {
		t.Error("tidier spelling does not sort before the punctuated twin")
	}
	if creatorVariantCanonicalLess(noisy, tidy) {
		t.Error("punctuated spelling sorts before the tidier twin")
	}
}
