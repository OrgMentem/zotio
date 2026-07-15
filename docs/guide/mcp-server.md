# MCP server

`zotio-mcp` exposes the CLI to MCP hosts like Claude Desktop and Claude Code. Install and register it as shown in [Install](install.md#3-the-mcp-server-zotio-mcp).

## Command-orchestration facade (default)

By default the server exposes three framework tools — `context`, `search`, and `sql` — plus a **command-orchestration facade** (`command_search` and `command_run`) and `workflow_submit`, a validated inline multi-step workflow tool. Agents can read domain context, search/query the synced local store directly, drive the CLI the same way a human would (search for the right command, then run it), and submit whole [workflows](workflows.md) — all on the same trust model as the CLI. Writes applied over MCP are journaled and replayed into the local mirror exactly like CLI writes.

The rationale and trade-offs are summarized in [Architecture decisions › MCP command surface](../contributing/architecture-decisions.md#mcp-command-surface), with the full records in the repo.

### Switching surfaces

Set `ZOTIO_MCP_SURFACE=mirror` to expose each MCP-eligible CLI command as one lean tool (global flags stripped). Commands annotated `mcp:hidden`, including the arbitrary-argument local-file `workflow run` runner, remain CLI-only — agents run multi-step workflows through the validated `workflow_submit` tool instead (see [Workflows & triggers](workflows.md)). The retired spec-derived typed endpoint tools (`collections_*`, `items_*`, `schema_*`, `tags_*`, …) are no longer part of either surface; use `command_run` or the mirror for those workflows.

## Context resources

Beyond tools, the server serves live Zotero context as MCP **resources**:

- `zotero://context` · `zotero://agent-context` — CLI + library introspection
- `zotero://status` · `zotero://freshness` — connectivity and cache state
- `zotero://schema` — Zotero item-type and field schema
- `zotero://capabilities` — the read/write trust registry ([reference](../reference/capabilities.md))

## Authentication

The `ZOTERO_API_KEY` env var is optional for read-only local-desktop use (the local API needs no key). Set it to enable writes and reach group libraries — see [Authentication](authentication.md).

```bash
claude mcp add zotero zotio-mcp -e ZOTERO_API_KEY=<your-key>
```
