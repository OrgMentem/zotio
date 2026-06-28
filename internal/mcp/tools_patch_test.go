// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean 1b05b22e): verify path-parameter percent-encoding blocks path-segment injection.

package mcp

import "testing"

// TestMCPPathValueNeutralizesInjection proves the makeAPIHandler path-parameter
// substitution can no longer be steered to a different endpoint by a value that
// contains URL path metacharacters, while valid Zotero keys pass through byte-for-byte.
func TestMCPPathValueNeutralizesInjection(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"valid_key_unchanged", "ABCD1234", "ABCD1234"},
		{"slash_escaped", "ABC/items", "ABC%2Fitems"},
		{"traversal_escaped", "../../keys", "..%2F..%2Fkeys"},
		{"space_escaped", "a b", "a%20b"},
		{"query_escaped", "K?format=json", "K%3Fformat=json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mcpPathValue(tc.in); got != tc.want {
				t.Errorf("mcpPathValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
