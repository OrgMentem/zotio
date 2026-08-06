// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// exported accessors so the MCP layer can expose CLI
// introspection (agent-context) as an MCP resource without duplicating the
// builder or editing the generated agent_context.go.

package cli

import (
	"encoding/json"
	"os"
	"strings"
)

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

// DefaultDBPath returns the canonical local SQLite database path for name,
// honoring the active --group scope and demo mode (see defaultDBPath). It
// exists so the MCP server resolves the identical group-scoped and
// demo-scoped path as the CLI, instead of maintaining a second resolver
// that can silently diverge from defaultDBPath's group/demo handling.
func DefaultDBPath(name string) (string, error) {
	return defaultDBPath(name)
}

// ApplyGroupScopeFromEnv sets activeGroupID from ZOTERO_GROUP when it is
// currently unset and the env value is a non-empty numeric string. MCP
// resource handlers call exported cli helpers (FreshnessJSON, HealthJSON,
// ItemContextJSON, ...) directly and never execute the cobra root, so the
// ZOTERO_GROUP fallback that PersistentPreRunE performs for CLI commands
// never runs for them; the MCP server calls this once at startup to apply
// the same fallback before serving.
func ApplyGroupScopeFromEnv() {
	if activeGroupID != "" {
		return
	}
	if v := strings.TrimSpace(os.Getenv("ZOTERO_GROUP")); v != "" && isAllDigits(v) {
		activeGroupID = v
	}
}
