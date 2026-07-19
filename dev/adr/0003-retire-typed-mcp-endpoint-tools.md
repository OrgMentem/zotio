# ADR 0003 — Retire typed MCP endpoint tools

- **Status:** Accepted (2026-07-10).
- **Scope:** `internal/mcp/` (the `zotio-mcp` server's tool surface).
- **Deciders:** enieuwy.

## Context

The MCP server still exposed 28 typed, spec-derived endpoint tools (`collections_*`, `items_get/list`, `schema_*`, `tags_*`, …) even after the CLI Printing Press generator was retired on 2026-07-08. Those tools were frozen at generator-retirement time while new Zotero workflows moved to hand-written CLI commands that are auto-exposed through the cobratree MCP facade and mirror.

That split left two sources of truth:

1. The CLI command tree, which owns local-read parity, mutation gates, command-specific fixes, and the agent contract.
2. The typed endpoint tools, which bypassed the CLI and called the API directly through `newMCPClient`.

The bypass was already visible as drift. The CLI fixed the `schema` library-prefix 404 by stripping library prefixes (`stripLibraryPrefix`), but the typed `schema_*` MCP tools never received the fix because they used their own direct endpoint pathing. Typed writes were also no longer a useful surface: `tools.go` rejected non-GET typed endpoint calls and directed agents to `command_run` so the CLI mutation envelope (`--dry-run` preview, `--yes` apply, journal/safety gates) stayed authoritative.

ADR-0001 measured the typed endpoint surface at roughly **3.2K standing tokens**. That was not the main token problem, but it was enough cost to keep loading stale tools at every connection while also advertising a second, less-correct way to do the same work.

## Decision

Delete the typed spec-endpoint MCP surface.

`RegisterTools` now registers only:

1. the three MCP framework tools — `context`, `search`, and `sql`; and
2. the selected cobratree command surface: default `command_search` / `command_run` facade, or the per-command mirror when `ZOTIO_MCP_SURFACE=mirror`.

The CLI tree is the single MCP source of truth for endpoint-shaped work. Agents that need collection, item, schema, tag, or search workflows should use `command_search` to discover the CLI command, then `command_run` to execute it; hosts that need native one-tool-per-command schemas can switch to the mirror.

`RegisterResources`, `RegisterPrompts`, the framework tools, and the cobratree facade/mirror machinery remain in place.

## Consequences

- **No stale typed endpoint names.** Hosts pinned to `collections_*`, `items_*`, `schema_*`, `tags_*`, or other spec-derived tool names must switch to `command_run` on the facade or to the per-command mirror.
- **One source of behavior.** Endpoint-shaped MCP workflows now inherit the CLI's local read parity and per-command fixes instead of bypassing them with direct API calls.
- **Mutation safety stays centralized.** Typed writes were already disabled; removing the typed tools makes `command_run` / mirror commands the only MCP path for mutations.
- **Standing token cost drops.** The typed endpoint surface's ~3.2K standing tokens from ADR-0001 drops to zero.
- **ADR-0001's surface table is amended conceptually.** The “Spec endpoints” row is historical as of 2026-07-10; current surfaces are framework tools plus the facade/mirror command surface.

## Alternatives considered

| Option | Why not |
|---|---|
| Keep typed read-only endpoint tools | Preserves stale direct-API behavior and keeps advertising a second source of truth for workflows the CLI already owns. |
| Patch individual drift cases (`schema_*`, path rewriting, future endpoint quirks) | Recreates generator maintenance by hand and guarantees future fixes must be made twice. |
| Hide typed writes only | Already done; it did not solve read-path drift or standing token cost. |
| Keep typed tools only in `mirror` mode | Confuses the runtime switch: `mirror` should mean per-command Cobra mirror, not a mixture of command tools plus retired spec endpoints. |

## Validation

- `RegisterTools` registers only the framework tools and the selected cobratree command surface.
- Dead typed-endpoint helpers and tests were removed.
- `go build ./internal/mcp/... ./cmd/zotio-mcp` compiles.
- Helper drift check: `grep -c 'makeAPIHandler\|mcpParamBinding' internal/mcp` returns no matches.
