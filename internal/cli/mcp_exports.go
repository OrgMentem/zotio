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
