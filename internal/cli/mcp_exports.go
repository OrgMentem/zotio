// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// exported accessors so the MCP layer can expose CLI
// introspection (agent-context) as an MCP resource without duplicating the
// builder or editing the generated agent_context.go.

package cli

import "encoding/json"

// AgentContextJSON builds the structured agent-context description from a fresh
// command tree and returns it as indented JSON — the same payload the
// `agent-context` command emits, for use as an MCP resource.
func AgentContextJSON() ([]byte, error) {
	ctx := buildAgentContext(RootCmd())
	return json.MarshalIndent(ctx, "", "  ")
}

// CapabilitiesJSON builds the typed capability + preconditions registry from a
// fresh command tree and returns it as indented JSON — the payload the
// `capabilities` command emits, for use as the zotero://capabilities MCP resource.
func CapabilitiesJSON() ([]byte, error) {
	return json.MarshalIndent(buildCapabilityRegistry(RootCmd()), "", "  ")
}

// FeatureIndexJSON returns the curated capability ("which") index as indented
// JSON — the highlighted feature list the docs generator renders as the
// highlights reference page, kept in one place with the other introspection exports.
func FeatureIndexJSON() ([]byte, error) {
	return json.MarshalIndent(whichIndex, "", "  ")
}

// CommandOverrideCapability returns the safety-critical registry metadata for a
// command path (root name stripped): the declared operation kind, required
// preconditions, and destructiveness. ok is false when no override is declared
// for that path. Consumed by the MCP command-orchestration facade so
// command_search / command_run detail is capability- and safety-aware without
// re-deriving the registry or importing unexported state.
func CommandOverrideCapability(path string) (operation string, requires []string, destructive bool, ok bool) {
	entry, found := capabilityOverrides[path]
	if !found {
		return "", nil, false, false
	}
	return entry.Operation, entry.Requires, entry.Destructive, true
}
