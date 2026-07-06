// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean security-audit): regression test for the json_extract field-name
// guard that protects ResolveByName's Sprintf-built query from injection via a
// caller-supplied matchField containing quotes or SQL syntax.

package store

import "testing"

func TestIsSafeJSONFieldName(t *testing.T) {
	safe := []string{"name", "key", "email", "data.name", "field_1", "_x"}
	for _, s := range safe {
		if !isSafeJSONFieldName(s) {
			t.Errorf("isSafeJSONFieldName(%q) = false, want true", s)
		}
	}
	unsafe := []string{
		"",
		"na'me",
		"name'); DROP TABLE resources--",
		"a b",
		"name\"",
		"a)b",
		"$.name",
	}
	for _, s := range unsafe {
		if isSafeJSONFieldName(s) {
			t.Errorf("isSafeJSONFieldName(%q) = true, want false (injection vector)", s)
		}
	}
}
