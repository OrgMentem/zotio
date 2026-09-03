// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"strings"
	"testing"
)

// ZOTERO_GROUP=all must stay refused by the MCP startup fallback even though
// --group all is a legal CLI value. The fan-out is a wrapper around cobra
// command execution; the MCP server's native handlers read the group global
// directly and never pass through it, so accepting "all" here would serve one
// library under the name of every library. This test exists to stop a later
// reader who sees `all` accepted in the CLI from "fixing" the MCP path to match.
func TestApplyGroupScopeFromEnvRefusesTheFanoutSentinel(t *testing.T) {
	restore := SnapshotGlobals()
	defer restore()
	setActiveGroupID("")

	t.Setenv("ZOTERO_GROUP", groupFanoutAll)
	err := ApplyGroupScopeFromEnv()
	if err == nil {
		t.Fatal("ApplyGroupScopeFromEnv() error = nil, want ZOTERO_GROUP=all refused: the MCP handlers cannot fan out")
	}
	if got := ActiveGroupID(); got != "" {
		t.Fatalf("ActiveGroupID() = %q, want %q after a rejected value", got, "")
	}
	// The refusal has to say what the value means and what to do instead;
	// "expected a numeric Zotero group ID" alone reads as "malformed" about a
	// value the CLI documents.
	for _, want := range []string{"all", "CLI only", "one library", "numeric group ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err.Error(), want)
		}
	}
}
