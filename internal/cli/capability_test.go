// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// the asserting guard the capabilityOverrides comment
// promises — every override key must resolve to a real runnable command, so a
// typo'd/renamed key can't silently fall through to a wrong (e.g. keyless)
// classification in the agent-facing capability registry.

package cli

import "testing"

func TestCapabilityOverridesResolveToRealCommands(t *testing.T) {
	entries := buildCapabilityRegistry(RootCmd())
	paths := make(map[string]bool, len(entries))
	for _, e := range entries {
		paths[e.Path] = true
	}
	for key := range capabilityOverrides {
		if !paths[key] {
			t.Errorf("capabilityOverrides key %q does not resolve to a runnable command (stale or typo'd?)", key)
		}
	}
}
