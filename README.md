# Zotero CLI

**Every Zotero feature in the terminal, plus offline search, annotation export, and library analytics no existing tool offers.**

This CLI connects directly to your running Zotero desktop app — no API key needed. It syncs your library to a local SQLite store for offline search and compound queries, then adds 18 features (reading queues, tag audits, annotation timelines, collection exports) that Zotero's UI and pyzotero can't do. With `--data-source local`, item reads apply the same `--item-type`, `--tag`, collection, `--sort`/`--direction`, and `--limit`/`--start` scopes as live API calls, so offline results match online ones.

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

The CLI connects to your Zotero desktop app at `localhost:23119` — **no API key required when Zotero is running**. The `ZOTERO_API_KEY` env var is only needed if you want to reach the web API at `api.zotero.org` instead (group libraries, or accessing your library while the desktop app is closed). Generate a web-API key at https://www.zotero.org/settings/keys.

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

- **`zotero-pp-cli items children`** - Get child items (attachments and notes) for an item
- **`zotero-pp-cli items create`** - Create one or more items
- **`zotero-pp-cli items delete`** - Delete an item (moves to trash)
- **`zotero-pp-cli items get`** - Get a single item by key
- **`zotero-pp-cli items list`** - List all items in the library
- **`zotero-pp-cli items tags`** - Get tags for a specific item
- **`zotero-pp-cli items top`** - List top-level items only (excludes attachments and notes)
- **`zotero-pp-cli items trash`** - List items in the trash
- **`zotero-pp-cli items update`** - Update a specific item

### schema

Zotero item type and field schema

- **`zotero-pp-cli schema creator-fields`** - List all creator fields (firstName, lastName, name)
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

The `-e ZOTERO_API_KEY=<your-key>` is optional for local-desktop use; the local API at localhost:23119 needs no key. Set it only for the Zotero web API (group libraries, or while the desktop app is closed).

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
| `ZOTERO_API_KEY` | per_call | No (web API only) | Only needed for the Zotero web API (group libraries, or while the desktop app is closed). The local desktop API at localhost:23119 needs no key. |

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
