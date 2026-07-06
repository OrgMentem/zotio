# MCP server

`zotio-mcp` exposes the CLI to MCP hosts like Claude Desktop and Claude Code. Install and register it as shown in [Install](install.md#3-the-mcp-server-zotio-mcp).

## Command-orchestration facade (default)

By default the server exposes a **command-orchestration facade** — `command_search` and `command_run` — rather than one tool per endpoint. Agents discover and drive the CLI the same way a human would: search for the right command, then run it. This keeps the tool surface small and the trust model identical to the CLI.

The rationale and trade-offs are recorded in [ADR 0001 — MCP command surface](../adr/0001-mcp-command-surface.md).

### Switching surfaces

Set `PP_MCP_SURFACE` to switch to the full one-tool-per-endpoint mirror (global flags stripped). The complete tool list for that surface is in the [MCP tools reference](../reference/mcp-tools.md).

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
