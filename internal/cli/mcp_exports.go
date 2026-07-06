// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean qfuq): exported accessors so the MCP layer can expose CLI
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
// PATCH(glean roadmap-phase2): expose the capability registry to MCP hosts.
func CapabilitiesJSON() ([]byte, error) {
	return json.MarshalIndent(buildCapabilityRegistry(RootCmd()), "", "  ")
}

// FeatureIndexJSON returns the curated capability ("which") index as indented
// JSON — the hero-feature list the docs generator renders as the highlights
// reference page, kept in one place with the other introspection exports.
// PATCH(zensical-docs): expose the which feature index to the docs generator.
func FeatureIndexJSON() ([]byte, error) {
	return json.MarshalIndent(whichIndex, "", "  ")
}
