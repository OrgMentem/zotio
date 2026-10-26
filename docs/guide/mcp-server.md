# MCP server

`zotio-mcp` exposes the CLI to MCP hosts like Claude Desktop and Claude Code. Install and register it as shown in [Install](install.md#3-the-mcp-server-zotio-mcp).

## Command-orchestration facade (default)

By default the server exposes three framework tools — `context`, `search`, and `sql` — plus a **command-orchestration facade** (`command_search` and `command_run`) and `workflow_submit`, a validated inline multi-step workflow tool. Agents can read domain context, search/query the synced local store directly, drive the CLI the same way a human would (search for the right command, then run it), and submit whole [workflows](workflows.md) — all on the same trust model as the CLI. Writes applied over MCP are journaled and replayed into the local mirror exactly like CLI writes.

The rationale and trade-offs are summarized in [Architecture decisions › MCP command surface](../contributing/architecture-decisions.md#mcp-command-surface), with the full records in the repo.

### Switching surfaces

Set `ZOTIO_MCP_SURFACE=mirror` to expose each MCP-eligible CLI command as one lean tool (global flags stripped). Framework commands match by full path, so `annotations search` is a mirrored tool. Every command that writes local files exposes `--dry-run`. Tool hints follow the capability registry: `destructiveHint` is `true` only for registry-destructive commands and local file writers, additive writes report `destructiveHint:false`, and commands that read only the local mirror report `openWorldHint:false`. Commands annotated `mcp:hidden`, including the arbitrary-argument local-file `workflow run` runner, remain CLI-only — agents run multi-step workflows through the validated `workflow_submit` tool instead (see [Workflows & triggers](workflows.md)). The retired spec-derived typed endpoint tools (`collections_*`, `items_*`, `schema_*`, `tags_*`, …) are no longer part of either surface; use `command_run` or the mirror for those workflows.

## Context resources

Beyond tools, the server serves live Zotero context as MCP **resources**:

- `zotero://context` · `zotero://agent-context` — CLI + library introspection
- `zotero://status` · `zotero://freshness` — connectivity and cache state
- `zotero://schema` — local SQLite cache DDL, for writing queries with the `sql` tool
- `zotero://capabilities` — the read/write trust registry ([reference](../reference/capabilities.md))

### Library resources

These templates return library content as untrusted data:

| URI template | Content |
| --- | --- |
| `zotero://collections/{key}` | Collection metadata and an item manifest from the local store. |
| `zotero://items/{key}` | Item metadata and annotations from the local store. |
| `zotero://health/{scope}` | Ranked library-health findings for `library`, `collection:KEY`, `tag:NAME`, `query:TEXT`, or `item:KEY`. |
| `zotero://collections/{key}/tree` | The collection and nested subcollections, with depth and node limits. |
| `zotero://items/{key}/children` | Child notes and attachments, with bounded results. |
| `zotero://items/{key}/attachments` | Attachment keys, titles, content types, and link modes. |
| `zotero://items/{key}/context` | The item, parent, collections, tags, and child/attachment counts. |
| `zotero://items/{key}/related` | Incoming and outgoing related-item edges from the local store. |

### Prompts

- `inspect-library`: survey sync status, collections, counts, and gaps.
- `export-reading-notes`: prepare an annotation digest; optional arguments are `collection` and `item`.
- `prepare-citation-export`: prepare a bibliography; optional arguments are `collection` and `format`.
- `synthesize`: summarize a bounded context bundle; optional arguments are `item` and `collection`.
- `prepare-library-health`: assess findings and preview fixes; accepts an optional `scope`.
- `prepare-import`: review the scan → resolve → apply pipeline; accepts an optional `dir`.
- `sync-vault-safely`: audit the vault, sync notes, and review write-back conflicts.

## Transport

The server uses stdio by default. Select HTTP with `--transport http` or `ZOTIO_MCP_TRANSPORT=http`; the flag takes precedence.
`--addr` sets the bind address, which defaults to `127.0.0.1:7777`. The HTTP endpoint is `/mcp`.

```bash
zotio-mcp --transport http --addr 127.0.0.1:7777 --mcp-auth-token-file /path/to/private-token
```

HTTP clients must send `Authorization: Bearer <token>`. The server selects the token in this order:

1. The file named by `--mcp-auth-token-file`.
2. The `ZOTIO_MCP_TOKEN` environment variable.
3. A new token for this run, saved in a mode-0600 file in the zotio state directory.

For a generated token, stderr reports the file path, not the token.
Each start replaces the generated token file for that bind address.
Use an explicit token file or environment variable when clients need a stable token.
The server refuses token values passed through `--mcp-auth-token`.

`--allow-unauthenticated` disables HTTP bearer authentication. Avoid it: loopback does not isolate users, and reachable clients can invoke read and write tools.
This transport token is separate from the Zotero API key.

## Authentication

The `ZOTERO_API_KEY` env var is optional for read-only local-desktop use (the local API needs no key). Set it to enable writes and reach group libraries — see [Authentication](authentication.md).

```bash
claude mcp add zotero zotio-mcp -e ZOTERO_API_KEY=<your-key>
```

## Concurrent access

Only one zotio writer may update an installation or independent output at a time. Installation writers use the host-user lock `~/.zotio/.writer.lock`: `--config`, `ZOTERO_CONFIG`, and `ZOTERO_DATA_DIR` do not make concurrent writer scopes because profiles remain shared at `~/.zotio/profiles.json`. A concurrent write fails immediately with exit code 9 and is safe to retry after the active writer finishes; read-only commands and dry-run previews remain available.

`watch` and `tail` take the installation writer lock for each cycle or poll, then release it between cycles.

## Library content is data, never instruction

An item's title, abstract, note, tag, or annotation is authored by whoever can write to the library — a group co-member, a scraped web page, a metadata provider — and this same server exposes the CLI's write surface. Results that carry library content are therefore framed before they reach the host model, so an injected "ignore previous instructions" reads as a quoted string rather than as an operator directive:

- **JSON results** (`search`, `sql`, and the library resources: collections, items, bundles, manifests, reading notes) carry a top-level `_zotio_provenance` object — `source: zotero_library`, `trust: untrusted_data`, and a notice naming the content as data. Existing shapes are preserved: `search` keeps its `items`/`count` envelope, `sql` keeps its `rows` object, and resources keep their object shape and `application/json` MIME type. A field named `_zotio_provenance` in the library itself cannot forge it, because the authoritative value is written after decoding.
- **Non-JSON text results** — including mirrored CLI stdout that is not JSON — are wrapped in a preamble plus a per-call nonce-delimited `<<<ZOTERO-DATA …>>>` block, so content cannot close the block and resume as trusted text. Unsafe C0/DEL bytes are neutralized in the same pass (tab, LF, and CR survive); mirrored stdout that *is* valid JSON keeps its shape and is neither nonce-framed nor marker-injected, and JSON encoding escapes any control byte inside it.
- **Trusted resources stay unframed:** `zotero://context`, `zotero://agent-context`, `zotero://status`, `zotero://freshness`, `zotero://schema`, and `zotero://capabilities` describe this CLI, not your library.

Bounds: the native library JSON paths (`search`, `sql`, and the library resources) keep each result under 60 KB. A top-level array payload there is capped at 50 items, and an oversized object is truncated recursively — its nested arrays reduced to at most 50 entries — with the truncation recorded in the payload. Small object results, such as a compact `sql` row envelope, are bounded by bytes rather than by a 50-row cap. Mirrored command stdout has a byte cap only: output over the cap is replaced by a preview with the original size, and a JSON array under the byte cap is not limited to 50 elements. Command discovery is serialized without these bounds.

Framing is mitigation, not a boundary. What actually decides whether a model's request mutates anything is the CLI's own write gate: `command_run` forwards the flags the model supplies, and every command that writes to Zotero or the vault previews unless `yes` is passed. There is no separate allowlist of mutating commands — every MCP-eligible command is reachable (commands marked `mcp:hidden`, such as `workflow run`, stay CLI-only) — so the gate is the boundary: a model that withholds `yes` cannot apply a write to Zotero or the vault. The gate does not cover local output files. Commands that write a file and are otherwise read-only, such as `collections export`, create or replace their output file unless `dry-run` is passed, so pass `dry-run` to preview them. `dry-run` overrides `yes`, which is what makes a `workflow_submit` preview safe, since the runner injects `--dry-run` into each step that supports it and skips steps that cannot preview.
