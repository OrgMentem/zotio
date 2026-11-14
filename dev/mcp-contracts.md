# MCP surface contracts

Read this reference before work in this area. Paths in code spans and commands are repository-root-relative unless stated otherwise.

## MCP Surface

The MCP surface **is** the CLI tree: `zotio-mcp` runs Cobra commands in-process via the `command_search`/`command_run` facade (or per-command mirror, `ZOTIO_MCP_SURFACE=mirror`). New functionality = a CLI command; it is auto-exposed over MCP with the same behavior and write gates unless intentionally marked `mcp:hidden` (for example, `workflow run` and `schema drift`). Framework commands the server covers itself are skipped by full command path (`search`, not `annotations search`), so a nested command that shares a leaf name stays exposed; commands marked `mcp:writes-files` expose `--dry-run`. The raw argument vectors of `workflow run` would bypass the facade's per-command safe-flag allowlist; its validated agent route is `workflow_submit`. **Never add spec-derived typed MCP tools** — that parallel surface was retired for drifting behind CLI fixes (ADR-0003). The only hand-written MCP tools are the framework trio `context`/`search`/`sql` and `workflow_submit`, plus resources and prompts.
