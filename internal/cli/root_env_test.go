// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean roadmap-phase7 3df91067): env fallback for profile/group so MCP
// installs and scheduled agents (env, not flags) honor library/profile selection.

package cli

import (
	"bytes"
	"testing"
)

func runRootForEnvTest(t *testing.T, args ...string) {
	t.Helper()
	t.Cleanup(func() { activeGroupID = "" })
	var flags rootFlags
	root := newRootCmd(&flags)
	root.SetArgs(args)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute %v: %v", args, err)
	}
}

func TestGroupEnvFallbackApplies(t *testing.T) {
	t.Setenv("ZOTERO_GROUP", "12345")
	t.Setenv("ZOTERO_PROFILE", "")
	runRootForEnvTest(t, "capabilities")
	if activeGroupID != "12345" {
		t.Errorf("activeGroupID = %q, want 12345 from ZOTERO_GROUP", activeGroupID)
	}
}

func TestExplicitGroupFlagBeatsEnv(t *testing.T) {
	t.Setenv("ZOTERO_GROUP", "12345")
	t.Setenv("ZOTERO_PROFILE", "")
	runRootForEnvTest(t, "--group", "999", "capabilities")
	if activeGroupID != "999" {
		t.Errorf("activeGroupID = %q, want 999 (explicit --group beats env)", activeGroupID)
	}
}
