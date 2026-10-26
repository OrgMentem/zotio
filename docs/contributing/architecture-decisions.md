# Architecture decisions

zotio records its non-trivial design decisions as **ADRs** (Architecture Decision Records). The full technical records — context, options weighed, consequences — live in the repository under [`dev/adr/`](https://github.com/OrgMentem/zotio/tree/main/dev/adr). This page gives short, plain-language summaries of the ones that affect how you *use* the tool.

## MCP command surface

The MCP server presents a small **command-orchestration facade** — `command_search` and `command_run` — so an agent can discover and run CLI commands. The default surface also includes three framework tools: `context`, `search`, and `sql`. The hand-written `workflow_submit` tool accepts an inline multi-step [workflow](../guide/workflows.md), validated against the same per-command allowlist as `command_run`.

**What this means for you:** the default MCP surface is the facade. If you want the one-tool-per-command mirror instead (one tool for each MCP-eligible CLI command, not for each API endpoint), set `ZOTIO_MCP_SURFACE=mirror` (see the [MCP server guide](../guide/mcp-server.md) and the [MCP tools reference](../reference/mcp-tools.md)).

Full record: [ADR 0001 — MCP command surface](https://github.com/OrgMentem/zotio/blob/main/dev/adr/0001-mcp-command-surface.md).

## Retired typed MCP tools

Earlier builds also exposed 28 typed, spec-derived endpoint tools (`collections_*`, `items_*`, `schema_*`, `tags_*`, …) that called the Zotero API directly, bypassing the CLI. They drifted behind CLI fixes — a `schema` 404 fix never reached the typed `schema_*` tools — and advertised a second, less-correct way to do the same work, so they were removed. The CLI command tree, via the facade or mirror, is the single MCP source of truth.

**What this means for you:** hosts pinned to `collections_*` / `items_*` / `schema_*` / `tags_*` tool names must switch to `command_run` (facade) or the per-command mirror.

Full record: [ADR 0003 — Retire typed MCP endpoint tools](https://github.com/OrgMentem/zotio/blob/main/dev/adr/0003-retire-typed-mcp-endpoint-tools.md).

## Local read parity

Offline reads from the synced SQLite mirror are a **deliberate, per-resource subsystem** — not a generic query planner. Support for `--data-source local` filters is added intentionally, one resource at a time, only where people actually filter, so local behavior stays faithful to the live API rather than degrading to a lowest-common-denominator abstraction.

**What this means for you:** `--data-source local` supports the implemented local scopes. Under `auto`, the generic read resolver tries the live API first and uses the mirror for eligible failures. Local item reads refuse `--since-version` and `--format` with a precondition error because the mirror cannot reproduce them. Other unsupported scopes may fail explicitly or use the generic fallback, which warns when it cannot honor filters. See [Local read parity](../concepts/local-read-parity.md).

Full record: [ADR 0002 — Local read parity subsystem](https://github.com/OrgMentem/zotio/blob/main/dev/adr/0002-local-read-parity-subsystem.md).

## Single-writer concurrency

zotio is **multi-reader, single-writer**. Any number of reads, `--dry-run` previews, and unapplied `--agent` invocations run concurrently, but only one process at a time may apply writes to an installation — or to one independent output artifact, such as an export snapshot, collection bundle, or vault directory. Writers **fail fast rather than wait**: a second writer exits 9 immediately with retry guidance, because a queued writer would silently apply against state it read before the wait. Atomic file replacement, which the code previously relied on, prevents a torn file but is not concurrency control; a writer now holds its lock across the whole load-mutate-publish transaction.

**What this means for you:** serialize applies and parallelize reads. Scheduled jobs and agent hosts that fire overlapping writes should treat exit 9 as "retry shortly", not as a failure. Selecting a different config file or data directory (`--config`, `ZOTERO_CONFIG`, `ZOTERO_DATA_DIR`) does **not** buy you a second writer, because the profile store remains shared per user home.

Full record: [ADR 0005 — Single-writer concurrency contract](https://github.com/OrgMentem/zotio/blob/main/dev/adr/0005-single-writer-concurrency-contract.md).

## Permanent-delete tombstones

A permanent delete removes the mirrored item and records a deletion marker in the same transaction. Sync reconciliation suppresses stale desktop rows with that marker while Zotero sync catches up.

**What this means for you:** a sync during the desktop's delay does not restore a permanently deleted item to the local mirror. If the desktop continues to list it, check that Zotero is running and syncing.

Full record: [ADR 0007 — Permanent-delete tombstones](https://github.com/OrgMentem/zotio/blob/main/dev/adr/0007-permanent-delete-tombstones.md).

## Opt-in batched item writes

`items tags add` and `items tags remove` accept `--batch` to write up to 50 items per request. Each item retains its own outcome and version-conflict check.

**What this means for you:** batching reduces requests, but cannot stop at the first failure within a batch. It continues after failures and rejects `--max-failures` and explicit `--continue-on-error=false`. Leave out `--batch` when you need fail-fast writes.

Full record: [ADR 0008 — Batched item writes](https://github.com/OrgMentem/zotio/blob/main/dev/adr/0008-batched-item-writes.md).
