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

## Concurrent access

Only one zotio writer may update an installation or independent output at a time. Installation writers use the host-user lock `~/.zotio/.writer.lock`: `--config`, `ZOTERO_CONFIG`, and `ZOTIO_DATA_DIR` do not make concurrent writer scopes because profiles remain shared at `~/.zotio/profiles.json`. A concurrent write fails immediately with exit code 9 and is safe to retry after the active writer finishes; read-only commands and dry-run previews remain available.

## Library content is data, never instruction

An item's title, abstract, note, tag, or annotation is authored by whoever can write to the library — a group co-member, a scraped web page, a metadata provider — and this same server exposes the CLI's write surface. Results that carry library content are therefore framed before they reach the host model, so an injected "ignore previous instructions" reads as a quoted string rather than as an operator directive:

- **JSON results** (`search`, `sql`, and the library resources: collections, items, bundles, manifests, reading notes) carry a top-level `_zotio_provenance` object — `source: zotero_library`, `trust: untrusted_data`, and a notice naming the content as data. Array payloads move under an `items` field alongside `count` and the existing truncation metadata; a result field named `_zotio_provenance` in the library itself cannot forge it, because the authoritative value is written after decoding.
- **Text results** — mirrored CLI stdout included — are wrapped in a preamble plus a per-call nonce-delimited `<<<ZOTERO-DATA …>>>` block, so content cannot close the block and resume as trusted text. C0 and ANSI control bytes are neutralized in the same pass.
- **Trusted resources stay unframed:** `zotero://context`, `zotero://agent-context`, `zotero://status`, `zotero://freshness`, `zotero://schema`, and `zotero://capabilities` describe this CLI, not your library.

MIME types, JSON validity, root field names, and the 50-item / 60-KB result bounds are unchanged. Framing is mitigation, not a boundary: the write gate — `command_run` still requires an explicit `yes` — is what actually decides whether a model's request mutates anything.
