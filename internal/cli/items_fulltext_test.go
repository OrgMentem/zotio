// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import "testing"

func TestItemsFulltextAnnotations(t *testing.T) {
	cmd := newItemsFulltextCmd(&rootFlags{})
	anns := cmd.Annotations
	if got := anns["zotio:endpoint"]; got != "items.fulltext" {
		t.Errorf("zotio:endpoint = %q, want items.fulltext", got)
	}
	if got := anns["zotio:method"]; got != "GET" {
		t.Errorf("zotio:method = %q, want GET", got)
	}
	if got := anns["zotio:path"]; got != "/items/{itemKey}/fulltext" {
		t.Errorf("zotio:path = %q, want /items/{itemKey}/fulltext", got)
	}
	if got := anns["mcp:read-only"]; got != "true" {
		t.Errorf("mcp:read-only = %q, want true", got)
	}
}
