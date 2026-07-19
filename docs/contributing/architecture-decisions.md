# Architecture decisions

zotio records its non-trivial design decisions as **ADRs** (Architecture Decision Records). The full technical records — context, options weighed, consequences — live in the repository under [`dev/adr/`](https://github.com/OrgMentem/zotio/tree/main/dev/adr). This page gives short, plain-language summaries of the ones that affect how you *use* the tool.

## MCP command surface

The MCP server does **not** expose one tool per Zotero endpoint. Instead it presents a small **command-orchestration facade** — `command_search` and `command_run` — so an agent discovers and drives the CLI the same way a person would: find the right command, then run it. This keeps the tool list small, keeps the trust model identical to the CLI, and means new CLI commands are available over MCP with no extra wiring. The one hand-written exception is `workflow_submit`, which accepts an inline multi-step [workflow](../guide/workflows.md) validated against the same per-command allowlist as `command_run`.

**What this means for you:** the default MCP surface is the facade. If you want the full one-tool-per-endpoint mirror instead, set `ZOTIO_MCP_SURFACE` (see the [MCP server guide](../guide/mcp-server.md) and the [MCP tools reference](../reference/mcp-tools.md)).

Full record: [ADR 0001 — MCP command surface](https://github.com/OrgMentem/zotio/blob/main/dev/adr/0001-mcp-command-surface.md).

## Retired typed MCP tools

Earlier builds also exposed 28 typed, spec-derived endpoint tools (`collections_*`, `items_*`, `schema_*`, `tags_*`, …) that called the Zotero API directly, bypassing the CLI. They drifted behind CLI fixes — a `schema` 404 fix never reached the typed `schema_*` tools — and advertised a second, less-correct way to do the same work, so they were removed. The CLI command tree, via the facade or mirror, is the single MCP source of truth.

**What this means for you:** hosts pinned to `collections_*` / `items_*` / `schema_*` / `tags_*` tool names must switch to `command_run` (facade) or the per-command mirror.

Full record: [ADR 0003 — Retire typed MCP endpoint tools](https://github.com/OrgMentem/zotio/blob/main/dev/adr/0003-retire-typed-mcp-endpoint-tools.md).

## Local read parity

Offline reads from the synced SQLite mirror are a **deliberate, per-resource subsystem** — not a generic query planner. Support for `--data-source local` filters is added intentionally, one resource at a time, only where people actually filter, so local behavior stays faithful to the live API rather than degrading to a lowest-common-denominator abstraction.

**What this means for you:** `--data-source local` works for the scopes that have been built out; where a local path doesn't exist yet, `auto` falls back to the live API rather than returning partial results. See [Local read parity](../concepts/local-read-parity.md).

Full record: [ADR 0002 — Local read parity subsystem](https://github.com/OrgMentem/zotio/blob/main/dev/adr/0002-local-read-parity-subsystem.md).
