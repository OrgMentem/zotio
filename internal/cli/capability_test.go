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

func TestMutableCapabilityOverridesHaveWriteMetadata(t *testing.T) {
	want := map[string]struct {
		target  string
		require string
	}{
		"creators audit fix":       {target: "web_api", require: preconditionWebAPIKey},
		"items preprint-check fix": {target: "web_api", require: preconditionWebAPIKey},
		"vault pull":               {target: "local_vault", require: preconditionWebAPIKey},
		"vault sync":               {target: "local_vault", require: preconditionSyncedStore},
	}
	for _, entry := range buildCapabilityRegistry(RootCmd()) {
		expected, ok := want[entry.Path]
		if !ok {
			continue
		}
		delete(want, entry.Path)
		if entry.Operation != "write" || entry.WriteTarget != expected.target {
			t.Errorf("capability %q = operation=%q write_target=%q, want write to %q", entry.Path, entry.Operation, entry.WriteTarget, expected.target)
		}
		found := false
		for _, requirement := range entry.Requires {
			if requirement == expected.require {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("capability %q requires %q, want %q", entry.Path, entry.Requires, expected.require)
		}
	}
	for path := range want {
		t.Errorf("capability registry omitted mutable command %q", path)
	}
}
