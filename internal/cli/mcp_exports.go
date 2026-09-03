// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// exported accessors so the MCP layer can expose CLI
// introspection (agent-context) as an MCP resource without duplicating the
// builder or editing the generated agent_context.go.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
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

// ActiveGroupID returns the numeric Zotero group ID scoping this process, or ""
// for the personal library.
func ActiveGroupID() string {
	return activeGroupIDLocked()
}

// mcpSurface records that this process serves the MCP surface. It is a
// process-level marker rather than an argument check at the facade because
// the facade is not the only in-process executor: `workflow run` executes each
// step through its own root command (workflow_run.go
// executeWorkflowRunStepWithRoot), so a step reading `--group all` would slip
// past any argv inspection done where command_run dispatches. A marker cannot
// be routed around.
var mcpSurface atomic.Bool

// MarkMCPSurfaceActive is called once by the MCP server at startup. It refuses
// the --group all fan-out for the rest of the process: the fan-out cycles the
// process-global library scope through one library after another, while the
// server's native sql/search/resource handlers read that same global on
// concurrent requests without taking the mirrored-run slot that serializes
// command execution. Those handlers never opted into an iteration they cannot
// see, and serializing the whole server behind a fan-out would cost more than
// the feature is worth. Numeric --group is unaffected: it establishes one
// scope for one command, which is the exposure the server already accepts.
func MarkMCPSurfaceActive() {
	mcpSurface.Store(true)
}

// mcpSurfaceActive reports whether this process serves the MCP surface.
func mcpSurfaceActive() bool {
	return mcpSurface.Load()
}

// DefaultDBPath returns the canonical local SQLite database path for name,
// honoring the active --group scope and demo mode (see defaultDBPath). It
// exists so the MCP server resolves the identical group-scoped and
// demo-scoped path as the CLI, instead of maintaining a second resolver
// that can silently diverge from defaultDBPath's group/demo handling.
func DefaultDBPath(name string) (string, error) {
	return defaultDBPath(name)
}

// ApplyGroupScopeFromEnv sets the active group scope from ZOTERO_GROUP when it
// is currently unset. MCP resource handlers call exported cli helpers
// (FreshnessJSON, HealthJSON, ItemContextJSON, ...) directly and never execute
// the cobra root, so the ZOTERO_GROUP fallback that PersistentPreRunE performs
// for CLI commands never runs for them; the MCP server calls this once at
// startup to apply the same fallback before serving.
//
// A malformed value is an error rather than a silent fall back to the personal
// library, matching the hard failure root.go produces for --group. Silently
// serving a different library than the operator asked for is the same class of
// bug as the split-brain resolver this function exists to prevent.
//
// The unset check and the assignment share one write lock: a separate read
// followed by a write could interleave with cobra's PersistentPreRunE and
// clobber a scope another goroutine just established.
func ApplyGroupScopeFromEnv() error {
	activeGroupMu.Lock()
	defer activeGroupMu.Unlock()
	if activeGroupIDValue != "" {
		return nil
	}
	v := strings.TrimSpace(os.Getenv("ZOTERO_GROUP"))
	if v == "" {
		return nil
	}
	// "all" is a legal --group value for the CLI, where the fan-out wrapper
	// runs a read command once per library. The MCP surface has no such
	// wrapper: its native handlers read this global directly and would serve
	// one library under the name of "all", so the value stays refused here —
	// only the reason has to say so, or the operator reads "malformed" about a
	// value the CLI documents.
	if v == groupFanoutAll {
		return fmt.Errorf("invalid ZOTERO_GROUP value %q: %s fans reads out across libraries in the CLI only; the MCP server needs one library: set a numeric group ID or unset it", v, groupFanoutAll)
	}
	if !isAllDigits(v) {
		return fmt.Errorf("invalid ZOTERO_GROUP value %q: expected a numeric Zotero group ID", v)
	}
	activeGroupIDValue = v
	return nil
}
