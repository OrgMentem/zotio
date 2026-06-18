# Zotero CLI

**Every Zotero feature in the terminal, plus offline search, annotation export, and library analytics no existing tool offers.**

This CLI reads directly from your running Zotero desktop app — no API key needed for reads; write-back (creating, updating, or syncing items to Zotero) needs a Web API key configured once. It syncs your library to a local SQLite store for offline search and compound queries, then adds 18 features (reading queues, tag audits, annotation timelines, collection exports) that Zotero's UI and pyzotero can't do. With `--data-source local`, item reads apply the same `--item-type`, `--tag`, collection, `--sort`/`--direction`, and `--limit`/`--start` scopes as live API calls, so offline results match online ones.

## Install

The recommended path installs both the `zotero-pp-cli` binary and the `pp-zotero` agent skill in one shot:

```bash
npx -y @mvanhorn/printing-press install zotero
```

For CLI only (no skill):

```bash
npx -y @mvanhorn/printing-press install zotero --cli-only
```


### Without Node

The generated install path is category-agnostic until this CLI is published. If `npx` is not available before publish, install Node or use the category-specific Go fallback from the public-library entry after publish.

### Pre-built binary

Download a pre-built binary for your platform from the [latest release](https://github.com/mvanhorn/printing-press-library/releases/tag/zotero-current). On macOS, clear the Gatekeeper quarantine: `xattr -d com.apple.quarantine <binary>`. On Unix, mark it executable: `chmod +x <binary>`.

<!-- pp-hermes-install-anchor -->
## Install for Hermes

From the Hermes CLI:

```bash
hermes skills install mvanhorn/printing-press-library/cli-skills/pp-zotero --force
```

Inside a Hermes chat session:

```bash
/skills install mvanhorn/printing-press-library/cli-skills/pp-zotero --force
```

## Install for OpenClaw

Tell your OpenClaw agent (copy this):

```
Install the pp-zotero skill from https://github.com/mvanhorn/printing-press-library/tree/main/cli-skills/pp-zotero. The skill defines how its required CLI can be installed.
```

## Authentication

**Reads** go to your Zotero desktop app at `localhost:23119` — no API key required while Zotero is running. **Writes** (`items create`/`update`/`delete`, `vault push`/`pull`/`resolve`) require a Zotero web API key: configure it once with `zotero-pp-cli auth set-token <key>` (or set `ZOTERO_API_KEY`), and generate a key at https://www.zotero.org/settings/keys. When your configured base is the local API, writes auto-route to `api.zotero.org` while reads stay local — the first write prints a one-time stderr notice naming the write target. Run `zotero-pp-cli doctor` to see a `writes:` line reporting whether write-back is available or read-only. A web API key is also needed to read group libraries or to read while the desktop app is closed.

## Quick Start

```bash
# verify Zotero is running and the local API is reachable
zotero-pp-cli doctor


# sync your library to local SQLite for offline search
zotero-pp-cli sync


# find papers by keyword (offline, against synced data)
zotero-pp-cli search 'automation trust' --data-source local --json


# see your library breakdown by type, year, and journal
zotero-pp-cli library stats


# audit metadata health: missing PDFs, abstracts, DOIs
zotero-pp-cli items audit


# export all highlights from a collection to markdown
zotero-pp-cli annotations export --collection IDTUAULN --format markdown

```

## Unique Features

These capabilities aren't available in any other tool for this API.

### Library hygiene

- **`tags audit`** — Find and fix tag drift: groups tags that differ only by case or variant, shows item counts, and generates ready-to-run merge commands.

  _Use this before any literature review handoff to clean up tag taxonomy; dirty tags produce unreliable filtered exports._

  ```bash
  zotero-pp-cli tags audit --json
  ```
- **`items missing-pdf`** — List journal articles and book chapters that have no attached PDF — your download queue, ready to script.

  _Use this to batch-generate a download list for Unpaywall or Sci-Hub scripts._

  ```bash
  zotero-pp-cli items missing-pdf --type journalArticle --json | jq '.[].data.DOI'
  ```
- **`items audit`** — Count and list items missing PDFs, abstracts, DOIs, or tags — one command for a complete metadata health report.

  _Use this before a systematic review export to identify items that need metadata enrichment._

  ```bash
  zotero-pp-cli items audit --missing-abstract --missing-doi --json
  ```
- **`items enrich`** — Turn those audit gaps into fixes: resolve missing DOIs and abstracts from CrossRef and attach open-access PDF links from Unpaywall, then write them back to Zotero.

  _Previews a patch plan by default; pass `--yes` (or `--agent`) to apply. Field changes record provenance in the item's Extra field._

  ```bash
  # Preview proposed DOIs (safe; no writes)
  zotero-pp-cli items enrich --missing-doi --dry-run --agent
  # Apply DOI + abstract enrichment
  zotero-pp-cli items enrich --missing-doi --missing-abstract --yes
  # Attach open-access PDF links (needs a contact email for Unpaywall)
  zotero-pp-cli items enrich --missing-pdf --email you@example.com --yes
  ```
- **`library stats`** — See your library broken down by item type, publication year, and top journals — a dashboard in one command.

  _Use this to understand the shape and bias of a library before a systematic review or citation audit._

  ```bash
  zotero-pp-cli library stats --json --agent
  ```
- **`items unfiled`** — List items sitting in the library root with no collection assignment — your organizational debt.

  _Use this to identify items imported via browser connector that were never organized._

  ```bash
  zotero-pp-cli items unfiled --json | jq 'length'
  ```
- **`tags inventory`** — List all tags used in a collection with item counts — see which tags are local to a project vs. shared library-wide.

  _Use this to audit tag taxonomy consistency across sub-projects before a systematic review merge._

  ```bash
  zotero-pp-cli tags inventory --collection IDTUAULN --json
  ```
- **`items venues`** — List every journal and publication venue in your library with item counts and year ranges — understand where your sources come from.

  _Use this to scope a systematic review by journal or identify over-reliance on a single venue._

  ```bash
  zotero-pp-cli items venues --top 20 --json --agent
  ```
- **`items stale`** — Find items added long ago with no PDF and no annotations — candidates for pruning or enrichment.

  _Use this quarterly to identify items that were imported but never engaged with — candidates for deletion or PDF retrieval._

  ```bash
  zotero-pp-cli items stale --days 365 --no-pdf --json
  ```

### Reading workflow

- **`reading-list`** — Surface your oldest unread papers sorted by date added — your reading backlog, oldest-first, with abstract preview.

  _Use this to fetch the next paper an agent should fetch fulltext for, or to triage a reading session._

  ```bash
  zotero-pp-cli reading-list --limit 10 --agent
  ```
- **`annotations export`** — Export all highlights and notes from a collection or tag set as a single markdown or JSON file, one section per paper.

  _Use this to pull a week of reading annotations into a markdown document for synthesis or AI summarization._

  ```bash
  zotero-pp-cli annotations export --collection IDTUAULN --format markdown > reading-notes.md
  ```
- **`annotations timeline`** — See your annotations ordered by date — find what you were reading and highlighting in any time window.

  _Use this to extract a week's reading highlights for synthesis or to reconstruct a research trail._

  ```bash
  zotero-pp-cli annotations timeline --since 2026-05-01 --format markdown
  ```
- **`items open`** — Jump from CLI search results directly to the item in the Zotero desktop app.

  _Use this after finding an item via CLI search to open it for reading without leaving the terminal flow._

  ```bash
  zotero-pp-cli items open 9UXV5R7L --launch
  ```
- **`items note-template`** — Generate a pre-filled markdown reading note (frontmatter + abstract + empty Annotations section) for any item — paste into Obsidian or Logseq.

  _Use this to initialize a reading note in a PKM system without manually copying fields from the Zotero UI._

  ```bash
  zotero-pp-cli items note-template 9UXV5R7L --format obsidian >> notes/reading.md
  ```

### Vault sync & write-back

Keep a Markdown vault (Obsidian or Logseq) in step with Zotero in both directions: `vault sync` writes a note per item, you edit each note's user-owned `## Notes` region in your PKM, then `vault push` mirrors those edits back into Zotero and `vault pull` brings remote note edits in — all conflict-safe.

- **`vault sync`** — Generate one Markdown note per item from the local store (run `sync` first), idempotent on re-run. Managed frontmatter and a fenced annotations block are refreshed each run while your own prose is preserved; human-readable `collection_names` render alongside the collection keys.

  _Resolves the output directory and format from your `[vault]` config, so `--out` is optional; pass `--out`/`--format` to override._

  ```bash
  zotero-pp-cli vault sync
  ```
- **`vault push`** — Mirror each note's `## Notes` region back to one managed Zotero child note (Obsidian → Zotero). Conflict-safe: a remotely-diverged note is never overwritten — it is written as a conflict artifact under `_vault-zotero-conflicts/` and reported instead. Reads local, writes the web API.

  _Pass `--dry-run` to preview the write-back before it touches Zotero._

  ```bash
  zotero-pp-cli vault push --dry-run
  ```
- **`vault pull`** — Bring remote child-note edits into the `## Notes` region (Zotero → Obsidian), fast-forward only: it applies only when your local region is unchanged since the last sync. If both the local region and the remote note changed, it is reported as a conflict and never merged.

  ```bash
  zotero-pp-cli vault pull --dry-run
  ```
- **`vault conflicts`** / **`vault resolve`** — List unresolved write-back conflict artifacts, then resolve one by citekey or item key: `--keep-vault` republishes the vault copy over the remote (using the live version as a precondition), or `--recreate` re-creates a child note deleted in Zotero.

  ```bash
  zotero-pp-cli vault conflicts
  zotero-pp-cli vault resolve smith2023 --keep-vault
  ```

Configure the vault location and format once in `~/.config/zotero-pp-cli/config.toml`:

```toml
[vault]
root = "~/Vaults/dev"   # ~ is expanded; base output dir
notes_dir = "Zotero"     # notes land in <root>/<notes_dir>
format = "obsidian"      # or "logseq"
```

The `--out` and `--format` flags override these values. The write-back commands (`vault push`, `vault pull`, `vault resolve`) require a configured web API key — see [Authentication](#authentication).

### Export & citations

- **`collections export`** — Export an entire collection and all its subcollections as a single BibTeX or CSL-JSON file, preserving structure in comments.

  _Use this to hand a complete literature snapshot to LaTeX or to another researcher without losing the organizational hierarchy._

  ```bash
  zotero-pp-cli collections export IDTUAULN --format bibtex > philosophy.bib
  ```
- **`items citekey-conflicts`** — Find items without a Better BibTeX citation key or with duplicate keys — prevent LaTeX compilation failures before they happen.

  _Use this before exporting BibTeX for a LaTeX manuscript to catch key conflicts that cause \cite{} failures._

  ```bash
  zotero-pp-cli items citekey-conflicts --missing --json
  ```

## Usage

Run `zotero-pp-cli --help` for the full command reference and flag list.

## Commands

### collections

Manage collections in your Zotero library

- **`zotero-pp-cli collections create`** - Create one or more collections
- **`zotero-pp-cli collections delete`** - Delete a collection (does not delete items)
- **`zotero-pp-cli collections get`** - Get a specific collection
- **`zotero-pp-cli collections items`** - List all items in a collection
- **`zotero-pp-cli collections list`** - List all collections
- **`zotero-pp-cli collections subcollections`** - List subcollections of a collection
- **`zotero-pp-cli collections tags`** - List tags used within a collection
- **`zotero-pp-cli collections top`** - List only top-level collections (no parents)
- **`zotero-pp-cli collections update`** - Update a collection

### items

Manage items in your Zotero library

- **`zotero-pp-cli items annotations`** - List annotation children of an item
- **`zotero-pp-cli items children`** - Get child items (attachments and notes) for an item
- **`zotero-pp-cli items create`** - Create one or more items
- **`zotero-pp-cli items delete`** - Delete an item (moves to trash)
- **`zotero-pp-cli items file`** - Resolve the on-disk path (file:// URL) of an item's PDF attachment
- **`zotero-pp-cli items fulltext`** - Get extracted full text from an item's PDF attachment
- **`zotero-pp-cli items get`** - Get a single item by key
- **`zotero-pp-cli items list`** - List all items in the library
- **`zotero-pp-cli items tags`** - Get tags for a specific item
- **`zotero-pp-cli items top`** - List top-level items only (excludes attachments and notes)
- **`zotero-pp-cli items trash`** - List items in the trash
- **`zotero-pp-cli items update`** - Update a specific item

### schema

Zotero item type and field schema

- **`zotero-pp-cli schema creator-fields`** - List all creator fields (firstName, lastName, name)
- **`zotero-pp-cli schema drift`** - Detect item-type, field, and creator-field changes vs a saved baseline (run after a Zotero upgrade)
- **`zotero-pp-cli schema item-fields`** - List all available item fields
- **`zotero-pp-cli schema item-type-creator-types`** - List valid creator types for an item type
- **`zotero-pp-cli schema item-type-fields`** - List valid fields for a specific item type
- **`zotero-pp-cli schema item-types`** - List all available Zotero item types
- **`zotero-pp-cli schema new-item-template`** - Get a blank template for creating a new item of a given type

### searches

Manage saved searches in your Zotero library

- **`zotero-pp-cli searches get`** - Get a specific saved search
- **`zotero-pp-cli searches list`** - List all saved searches

### tags

Manage tags across your Zotero library

- **`zotero-pp-cli tags get`** - Get a specific tag by name
- **`zotero-pp-cli tags list`** - List all tags in the library

### vault

Sync your library to a Markdown vault (Obsidian/Logseq) and write notes back

- **`zotero-pp-cli vault sync`** - Generate Markdown notes (one per item) from the local store
- **`zotero-pp-cli vault push`** - Mirror each note's `## Notes` region back to a Zotero child note
- **`zotero-pp-cli vault pull`** - Bring remote child-note edits into the `## Notes` region (fast-forward only)
- **`zotero-pp-cli vault conflicts`** - List unresolved write-back conflict artifacts
- **`zotero-pp-cli vault resolve`** - Resolve a write-back conflict (`--keep-vault` or `--recreate`)


## Output Formats

```bash
# Human-readable table (default in terminal, JSON when piped)
zotero-pp-cli collections list

# JSON for scripting and agents
zotero-pp-cli collections list --json

# Filter to specific fields
zotero-pp-cli collections list --json --select id,name,status

# Dry run — show the request without sending
zotero-pp-cli collections list --dry-run

# Agent mode — JSON + compact + no prompts in one flag
zotero-pp-cli collections list --agent
```

## Agent Usage

This CLI is designed for AI agent consumption:

- **Non-interactive** - never prompts, every input is a flag
- **Pipeable** - `--json` output to stdout, errors to stderr
- **Filterable** - `--select id,name` returns only fields you need
- **Previewable** - `--dry-run` shows the request without sending
- **Explicit retries** - add `--idempotent` to create retries and `--ignore-missing` to delete retries when a no-op success is acceptable
- **Confirmable** - `--yes` for explicit confirmation of destructive actions
- **Piped input** - write commands can accept structured input when their help lists `--stdin`
- **Offline-friendly** - sync/search commands can use the local SQLite store when available
- **Agent-safe by default** - no colors or formatting unless `--human-friendly` is set

Exit codes: `0` success, `2` usage error, `3` not found, `4` auth error, `5` API error, `7` rate limited, `10` config error.

## Use with Claude Code

Install the focused skill — it auto-installs the CLI on first invocation:

```bash
npx skills add mvanhorn/printing-press-library/cli-skills/pp-zotero -g
```

Then invoke `/pp-zotero <query>` in Claude Code. The skill is the most efficient path — Claude Code drives the CLI directly without an MCP server in the middle.

<details>
<summary>Use as an MCP server in Claude Code (advanced)</summary>

If you'd rather register this CLI as an MCP server in Claude Code, install the MCP binary first:


Install the MCP binary from this CLI's published public-library entry or pre-built release.

Then register it:

```bash
claude mcp add zotero zotero-pp-mcp -e ZOTERO_API_KEY=<your-key>
```

The `-e ZOTERO_API_KEY=<your-key>` is optional for read-only local-desktop use; the local API at localhost:23119 needs no key. Set it to enable write operations (which route to the Zotero web API) and to reach group libraries or your library while the desktop app is closed.

Beyond the typed tools, the MCP server exposes Zotero context as **resources** — `zotero://context` (API taxonomy and query tips), `zotero://agent-context` (CLI command/flag/auth description), `zotero://status` (local archive sync state), `zotero://schema` (local SQLite DDL), and the templates `zotero://collections/{key}` (collection manifest) and `zotero://items/{key}` (item + annotations bundle) — plus guided **prompts** (`inspect-library`, `export-reading-notes`, `prepare-citation-export`). Hosts can discover library state and common workflows without shelling through mirrored commands.

</details>

## Use with Claude Desktop

This CLI ships an [MCPB](https://github.com/modelcontextprotocol/mcpb) bundle — Claude Desktop's standard format for one-click MCP extension installs (no JSON config required).

To install:

1. Download the `.mcpb` for your platform from the [latest release](https://github.com/mvanhorn/printing-press-library/releases/tag/zotero-current).
2. Double-click the `.mcpb` file. Claude Desktop opens and walks you through the install.
3. Fill in `ZOTERO_API_KEY` when Claude Desktop prompts you.

Requires Claude Desktop 1.0.0 or later. Pre-built bundles ship for macOS Apple Silicon (`darwin-arm64`) and Windows (`amd64`, `arm64`); for other platforms, use the manual config below.

<details>
<summary>Manual JSON config (advanced)</summary>

If you can't use the MCPB bundle (older Claude Desktop, unsupported platform), install the MCP binary and configure it manually.


Install the MCP binary from this CLI's published public-library entry or pre-built release.

Add to your Claude Desktop config (`~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "zotero": {
      "command": "zotero-pp-mcp",
      "env": {
        "ZOTERO_API_KEY": "<your-key>"
      }
    }
  }
}
```

</details>

## Health Check

```bash
zotero-pp-cli doctor
```

Verifies configuration, credentials, and connectivity to the API.

## Configuration

Config file: `~/.config/zotero-pp-cli/config.toml`

Static request headers can be configured under `headers`; per-command header overrides take precedence.

Environment variables:

| Name | Kind | Required | Description |
| --- | --- | --- | --- |
| `ZOTERO_API_KEY` | per_call | No for reads | Required for write operations (`items create`/`update`/`delete`, `vault push`/`pull`/`resolve`), which route to the Zotero web API; also needed for group libraries or while the desktop app is closed. Local desktop reads at localhost:23119 need no key. Configure once via `zotero-pp-cli auth set-token <key>`. |

## Troubleshooting
**Authentication errors (exit code 4)**
- Run `zotero-pp-cli doctor` to check credentials
- Verify the environment variable is set: `echo $ZOTERO_API_KEY`
**Not found errors (exit code 3)**
- Check the resource ID is correct
- Run the `list` command to see available items

### API-specific

- **doctor: connection refused** — Open Zotero desktop and enable: Preferences → Advanced → Allow other applications to communicate with Zotero
- **items missing-pdf returns nothing** — Run `sync` first to populate the local store from the running Zotero library
- **annotations export outputs empty sections** — PDF annotations must be made in Zotero's built-in PDF reader, not an external app
- **citekey-conflicts finds no keys** — Install the Better BibTeX extension for Zotero; citation keys appear in the 'extra' field

---

## Sources & Inspiration

This CLI was built by studying these projects and resources:

- [**cli-anything-zotero**](https://github.com/PiaoyangGuohai1/cli-anything-zotero) — TypeScript
- [**54yyyu/zotero-mcp**](https://github.com/54yyyu/zotero-mcp) — Python
- [**pyzotero**](https://github.com/urschrei/pyzotero) — Python
- [**kujenga/zotero-mcp**](https://github.com/kujenga/zotero-mcp) — Python
- [**jbaiter/zotero-cli**](https://github.com/jbaiter/zotero-cli) — Python
- [**dhondta/zotero-cli**](https://github.com/dhondta/zotero-cli) — Python

Generated by [CLI Printing Press](https://github.com/mvanhorn/cli-printing-press)
