---
name: zotio
description: "Zotero automation CLI: local-first library search, CI-gateable health reports, preview-first writes with undo, reviewable PDF/DOI import, annotation export, Obsidian vault sync, and an MCP server for agents. Trigger phrases: `search my Zotero library`, `check my Zotero library health`, `import this DOI into Zotero`, `export BibTeX from Zotero`, `find papers missing PDFs`, `export annotations from Zotero`, `undo that Zotero change`, `audit my Zotero tags`, `use zotero`, `open this paper in Zotero`."
author: "OrgMentem"
license: "MIT"
argument-hint: "<command> [args] | install cli|mcp"
allowed-tools: "Read Bash"
metadata:
  openclaw:
    requires:
      bins:
        - zotio
---

# zotio — Zotero Printing Press CLI

## Prerequisites: Install the CLI

This skill drives the `zotio` binary. **You must verify the CLI is installed before invoking any command from this skill.** If it is missing, install it first:

1. Install via the Printing Press installer:
   ```bash
   npx -y @mvanhorn/printing-press install zotero --cli-only
   ```
2. Verify: `zotio --version`
3. Ensure `$GOPATH/bin` (or `$HOME/go/bin`) is on `$PATH`.

If the `npx` install fails before this CLI has a public-library category, install Node or use the category-specific Go fallback after publish.

If `--version` reports "command not found" after install, the install step did not put the binary on `$PATH`. Do not proceed with skill commands until verification succeeds.

This CLI connects directly to your running Zotero desktop app — reads need no API key. Writes (`items create/update/delete/enrich`, `import apply`, `vault push`) require a Zotero Web API key, configured once via `auth set-token`, and are preview-first: every mutation plans by default, applies only under `--yes`, and is recorded to an undoable journal. It syncs your library to a local SQLite store for offline search and compound queries, then layers on the capabilities below — health reports, reviewable import, metadata enrichment, reproducible exports, and vault sync.

## When to Use This CLI

Use zotio when you need to script or automate your Zotero library: batch-export a collection's BibTeX before a deadline, find papers missing PDFs for a download script, extract this week's annotations for synthesis, or audit tag consistency before sharing a group library. It is especially useful as an MCP tool for agents that need to search or read a researcher's Zotero library.

## Hero Capabilities

The curated feature set — the same index `zotio which "<goal>"` resolves natural-language queries against.

### Library trust & health

- **`library health`** — Ranked, CI-gateable health report: citekey conflicts, duplicates, missing metadata, tag drift, broken attachments — with `--for` presets for citation or systematic-review readiness.

  _Run this before any bibliography export or screening handoff; gate CI with `--fail-on` (exit 11) and publish a shields.io badge with `--badge`._

  ```bash
  zotio library health --for citation --fail-on high --json
  zotio library health --badge > zotero-health-badge.json   # shields.io endpoint JSON
  ```
- **`items duplicates`** — Detect likely duplicate items by DOI or normalized title, then merge them safely with `duplicates resolve` — preview-first.

  _Duplicates corrupt PRISMA counts and double-cite sources; resolve them before they reach a manuscript._

  ```bash
  zotio items duplicates --json
  zotio items duplicates resolve --doi --dry-run
  ```
- **`items retract-check`** — Check every DOI-bearing item against Crossref's Retraction Watch data: retractions, expressions of concern, and corrections, with notice DOIs and dates.

  _Citing a retracted paper is a career-level embarrassment; catch it before a reviewer does. Also gates `library health` via `--check-retractions`._

  ```bash
  zotio items retract-check --json
  zotio library health --for citation --check-retractions --fail-on high
  ```
- **`collections gaps`** — Rank the papers your collection cites most that are missing from your library — citation-graph gap analysis via OpenCitations and Semantic Scholar.

  _A reviewer will ask why you didn't cite the field's most-cited work; find the gap before they do._

  ```bash
  zotio collections gaps IDTUAULN --top 20 --json
  ```
- **`tags audit`** — Find and fix tag drift: groups tags that differ only by case or variant, shows item counts, and generates ready-to-run merge commands.

  _Use this before any literature review handoff to clean up tag taxonomy; dirty tags produce unreliable filtered exports._

  ```bash
  zotio tags audit --json
  ```
- **`items audit`** — Count and list items missing PDFs, abstracts, DOIs, tags, or core citation fields (`--missing-citation`), and verify PDF files exist on disk (`--verify-files`) — one command for a complete metadata health report.

  _Use this before a systematic review export to identify items that need metadata enrichment._

  ```bash
  zotio items audit --missing-abstract --missing-doi --json
  ```
- **`items missing-pdf`** — List journal articles and book chapters that have no attached PDF — your download queue, ready to script.

  _Use this to batch-generate a download list for Unpaywall or Sci-Hub scripts._

  ```bash
  zotio items missing-pdf --type journalArticle --json | jq '.[].data.DOI'
  ```
- **`library stats`** — See your library broken down by item type, publication year, and top journals — a dashboard in one command.

  _Use this to understand the shape and bias of a library before a systematic review or citation audit._

  ```bash
  zotio library stats --json --agent
  ```
- **`schema drift`** — Detect what a Zotero upgrade changed: new or removed item types, fields, and creator fields vs a saved baseline.

  _Use this after upgrading Zotero to find item types or fields a new version added that your tooling may not model yet._

  ```bash
  zotio schema drift --json
  ```

### Safe writes & import

- **`import scan`** — Reviewable ingest: triage a folder of PDFs against your library (new vs duplicate vs attach-candidate), resolve metadata, then apply schema-valid creates from an editable manifest.

  _Bulk-import without making a mess — every create is previewed, deduplicated, and schema-validated before it touches your library._

  ```bash
  zotio import scan ~/Downloads/papers --json
  zotio import resolve manifest.json && zotio import apply manifest.json --dry-run
  ```
- **`import doi`** — Turn a DOI, PMID, arXiv ID, or ISBN into a schema-valid Zotero item — one command per identifier (`import doi|pmid|arxiv|isbn`).

  _Add a paper from a citation you found without opening a browser or hand-typing metadata._

  ```bash
  zotio import doi 10.1038/s41586-020-2649-2 --dry-run
  ```
- **`items enrich`** — Fill missing DOIs, abstracts, and open-access PDF links from CrossRef, OpenAlex, Semantic Scholar, and Unpaywall — preview-first, with provenance appended to each item; `--validate` cross-checks stored DOIs read-only.

  _Turn the audit's missing-metadata queue into applied fixes._

  ```bash
  zotio items enrich --missing-doi --dry-run
  ```
- **`items preprint-check`** — Find arXiv preprints that have since been published in a journal (via CrossRef) — and upgrade them with the published DOI using `preprint-check fix`, preview-first.

  _Citing a preprint when a journal version exists undermines a bibliography; this catches and fixes it in one pass._

  ```bash
  zotio items preprint-check --json
  zotio items preprint-check fix --dry-run
  ```
- **`journal undo`** — Every applied write is journaled; `journal undo <run-id>` reverses reversible runs (tag renames, collection moves) and loudly refuses the rest.

  _Batch writes are only safe when you can see what ran and take it back._

  ```bash
  zotio journal list
  zotio journal undo <run-id>
  ```

### Agent & automation surface

- **`items summarize`** — Assemble a bounded, synthesis-ready context bundle for an item or collection — citation, abstract, your annotations, a capped fulltext excerpt — without ever calling a model.

  _Hand an LLM exactly the high-signal context it needs for a literature synthesis, bounded and provenance-tagged._

  ```bash
  zotio items summarize 9UXV5R7L --json
  ```
- **`export snapshot`** — Reproducible, resumable full-library export (JSONL) with a lockfile recording each item's key, version, and content hash.

  _Prove what changed between two review handoffs by diffing lockfiles instead of eyeballing exports._

  ```bash
  zotio export snapshot library -o snapshot.jsonl
  zotio export snapshot library -o snapshot.jsonl --resume   # continue an interrupted run
  ```
- **`watch`** — Keep the local store fresh with periodic incremental syncs (`--interval`, `--once`); with `--health`, diff library health between cycles and report new findings to stdout or a webhook.

  _Run it in the background so agents and scripts never read stale data — and hear about new problems the cycle they appear._

  ```bash
  zotio watch --interval 5m --health --health-webhook https://example.com/hook
  ```
- **`workflow run`** — Run a declarative multi-step workflow spec (JSON) in-process, with per-step status and continue-on-error control.

  _Chain sync → audit → export into one reviewable spec instead of a brittle shell script._

  ```bash
  zotio workflow run nightly.json --json
  ```
- **`init`** — Guided first run: detect Zotero, enable the local API, set the Web API key, first sync, and a quick health check — one command from install to working setup.

  _Idempotent, and agent-safe under `--no-input` (unmet steps exit 9 with a machine-readable step report)._

  ```bash
  zotio init            # interactive walkthrough
  zotio init --no-input --json   # agent/CI probe
  ```

Vault sync (Obsidian/Logseq round-trip) is its own section below.

### Reading workflow

- **`reading-list`** — Surface your oldest unread papers sorted by date added — your reading backlog, oldest-first, with abstract preview.

  _Use this to fetch the next paper an agent should fetch fulltext for, or to triage a reading session._

  ```bash
  zotio reading-list --limit 10 --agent
  ```
- **`annotations export`** — Export all highlights and notes from a collection or tag set as a single markdown or JSON file, one section per paper.

  _Use this to pull a week of reading annotations into a markdown document for synthesis or AI summarization._

  ```bash
  zotio annotations export --collection IDTUAULN --format markdown > reading-notes.md
  ```
- **`annotations timeline`** — See your annotations ordered by date — find what you were reading and highlighting in any time window.

  _Use this to extract a week's reading highlights for synthesis or to reconstruct a research trail._

  ```bash
  zotio annotations timeline --since 2026-05-01 --format markdown
  ```
- **`items open`** — Jump from CLI search results directly to the item in the Zotero desktop app.

  _Use this after finding an item via CLI search to open it for reading without leaving the terminal flow._

  ```bash
  zotio items open 9UXV5R7L --launch
  ```
- **`items note-template`** — Generate a pre-filled markdown reading note (frontmatter + abstract + empty Annotations section) for any item — paste into Obsidian or Logseq.

  _Use this to initialize a reading note in a PKM system without manually copying fields from the Zotero UI._

  ```bash
  zotio items note-template 9UXV5R7L --format obsidian >> notes/reading.md
  ```
- **`library wrapped`** — Your Zotero year in review: items added by month and type, top venues and authors, annotation activity, PDF coverage — with a shareable SVG card.

  _The fun one: see (and share) what your reading year actually looked like, straight from the local store._

  ```bash
  zotio library wrapped --year 2026 --card wrapped-2026.svg
  ```

### Export & citations

- **`collections export`** — Export an entire collection and all its subcollections as a single BibTeX or CSL-JSON file, preserving structure in comments.

  _Use this to hand a complete literature snapshot to LaTeX or to another researcher without losing the organizational hierarchy._

  ```bash
  zotio collections export IDTUAULN --format bibtex > philosophy.bib
  ```
- **`items citekey-conflicts`** — Find items without a Better BibTeX citation key or with duplicate keys — prevent LaTeX compilation failures before they happen.

  _Use this before exporting BibTeX for a LaTeX manuscript to catch key conflicts that cause \cite{} failures._

  ```bash
  zotio items citekey-conflicts --missing --json
  ```
- **`items bibcheck`** — Check a manuscript (`.tex` or pandoc Markdown) against your library: every `\cite`/`@citekey` resolved, unknown and ambiguous keys flagged.

  _Run it before submission — unknown citekeys are LaTeX build failures waiting to happen; `--fail-on-unknown` exits 11 for CI._

  ```bash
  zotio items bibcheck thesis.tex --json
  zotio items bibcheck paper.md --fail-on-unknown
  ```

### Vault sync & write-back

Round-trip your library to a Markdown vault (Obsidian or Logseq) and back. Run `sync` first so the local store is populated, then push your edits to Zotero and pull remote changes back.

- **`vault sync`** — Export Zotero → Obsidian/Logseq Markdown notes, one file per item. Reads from the local store and is idempotent: it refreshes a managed frontmatter block and a fenced annotations block on each run while preserving your own prose, and renders human-readable `collection_names` alongside the collection keys. Resolves the output dir and format from the `[vault]` config block, so `--out` is optional.

  _Use this to keep a PKM vault in sync with your Zotero library without clobbering your notes._

  ```bash
  zotio vault sync
  ```
- **`vault push`** — Write-back: Obsidian → Zotero. Mirrors each note's user-owned `## Notes` region into one managed Zotero child note. Conflict-safe — it never overwrites a remotely-diverged note; instead it writes a conflict artifact under `_vault-zotero-conflicts/` and reports it. Reads local, writes the Web API (key required).

  _Use this to push reading notes you wrote in Obsidian back into Zotero. Pass `--dry-run` to preview._

  ```bash
  zotio vault push --dry-run
  ```
- **`vault pull`** — Bring remote child-note edits into the `## Notes` region, fast-forward only: it applies only when the local region is unchanged since the last sync. If both the local region and the remote note changed, it is reported as a conflict and never merged.

  _Use this to fold edits made in the Zotero app back into your vault notes. Pass `--dry-run` to preview._

  ```bash
  zotio vault pull --dry-run
  ```
- **`vault conflicts`** — List unresolved write-back conflict artifacts.

  ```bash
  zotio vault conflicts
  ```
- **`vault resolve`** — Resolve a conflict by citekey or item key, picking a direction. `--keep-vault` republishes the vault copy over the remote (using the live version as a precondition); `--keep-remote` pulls the remote note over the vault `## Notes` region (discarding local edits); `--recreate` re-creates a child note that was deleted in Zotero.

  ```bash
  zotio vault resolve smith2024 --keep-vault
  zotio vault resolve smith2024 --keep-remote
  ```

Configure the vault location and format once in `~/.config/zotio/config.toml`:

```toml
[vault]
root = "~/Vaults/dev"   # ~ is expanded; base output dir
notes_dir = "Zotero"     # notes land in <root>/<notes_dir>
format = "obsidian"      # or "logseq"
```

The `--out` and `--format` flags override these values.

## Command Reference

Partial map of the surface — run `zotio --help` (or see the docs site Reference → Commands) for the complete, always-current tree.

**collections** — Manage collections in your Zotero library

- `zotio collections create` — Create one or more collections
- `zotio collections delete` — Delete a collection (does not delete items)
- `zotio collections gaps` — Rank cited-but-missing papers for a collection (citation-graph gap analysis)
- `zotio collections get` — Get a specific collection
- `zotio collections items` — List all items in a collection
- `zotio collections list` — List all collections
- `zotio collections subcollections` — List subcollections of a collection
- `zotio collections tags` — List tags used within a collection
- `zotio collections top` — List only top-level collections (no parents)
- `zotio collections update` — Update a collection

**export** — Bibliography and snapshot exports

- `zotio export snapshot` — Reproducible, resumable paginated export with a content lockfile

**import** — Reviewable ingest of PDFs and identifiers

- `zotio import scan` — Triage a folder of PDFs against your library (read-only): new vs duplicate vs attach-candidate
- `zotio import resolve` — Resolve PDFs into an editable import manifest
- `zotio import apply` — Apply a reviewed import manifest (preview-first)
- `zotio import doi|pmid|arxiv|isbn` — Import one item from an identifier (CrossRef, PubMed, arXiv, Open Library)

**items** — Manage items in your Zotero library

- `zotio items annotations` — List annotation children of an item
- `zotio items audit` — Count and list items missing PDFs, abstracts, DOIs, tags, or citation fields
- `zotio items bibcheck` — Check a manuscript's `\cite`/`@citekey` references against the library (`--fail-on-unknown` exits 11)
- `zotio items children` — Get child items (attachments and notes) for an item
- `zotio items citekey-conflicts` — Find missing or duplicate Better BibTeX citation keys
- `zotio items create` — Create one or more items
- `zotio items delete` — Delete an item (moves to trash)
- `zotio items duplicates` — Find likely duplicate items; `duplicates resolve` merges them safely
- `zotio items enrich` — Fill or validate item metadata (DOI, abstract, OA PDF link) from external providers
- `zotio items file` — Resolve the on-disk path (file:// URL) of an item's PDF attachment
- `zotio items fulltext` — Get extracted full text from an item's PDF attachment
- `zotio items retract-check` — Check DOI-bearing items against Crossref retraction/concern/correction notices
- `zotio items missing-pdf` — List items with no attached PDF
- `zotio items open` — Print or launch a zotero:// deep link to an item
- `zotio items preprint-check` — Check arXiv preprints for published CrossRef records; `preprint-check fix` applies the published DOIs (preview-first)
- `zotio items summarize` — Assemble a bounded, synthesis-ready context bundle (citation, abstract, annotations, capped fulltext excerpt) for an item or collection
- `zotio items get` — Get a single item by key
- `zotio items list` — List all items in the library
- `zotio items tags` — Get tags for a specific item
- `zotio items top` — List top-level items only (excludes attachments and notes)
- `zotio items trash` — List items in the trash
- `zotio items update` — Update a specific item

**journal** — Append-only record of applied writes

- `zotio journal list` — List recorded mutation runs
- `zotio journal show` — Show one recorded run's operations
- `zotio journal undo` — Reverse a recorded run's reversible (tag/collection) changes

**library** — Whole-library reports

- `zotio library health` — Composite read-only health report with a CI gate (`--fail-on`, `--badge`, `--check-retractions`)
- `zotio library stats` — Items by type, year, and top venues in one dashboard
- `zotio library wrapped` — Year in review with a shareable SVG card (`--year`, `--card`)

**init** — Guided first-run setup

- `zotio init` — Detect Zotero, set the key, first sync, quick health check; `--no-input --json` for agents

**reading-list** — A `to-read` tag queue

- `zotio reading-list` — Oldest unread papers, with an `add` → `start` → `done` lifecycle

**schema** — Zotero item type and field schema

- `zotio schema creator-fields` — List all creator fields (firstName, lastName, name)
- `zotio schema drift` — Detect item-type/field/creator-field changes vs a saved baseline (run after a Zotero upgrade)
- `zotio schema item-fields` — List all available item fields
- `zotio schema item-type-creator-types` — List valid creator types for an item type
- `zotio schema item-type-fields` — List valid fields for a specific item type
- `zotio schema item-types` — List all available Zotero item types
- `zotio schema new-item-template` — Get a blank template for creating a new item of a given type

**searches** — Manage saved searches in your Zotero library

- `zotio searches get` — Get a specific saved search
- `zotio searches list` — List all saved searches

**tags** — Manage tags across your Zotero library

- `zotio tags get` — Get a specific tag by name
- `zotio tags list` — List all tags in the library

**vault** — Sync your library to a Markdown vault and write notes back

- `zotio vault conflicts` — List unresolved write-back conflicts
- `zotio vault pull` — Pull remote child-note edits into the vault's `## Notes` region (fast-forward only)
- `zotio vault push` — Write the vault's `## Notes` region back to Zotero child notes
- `zotio vault resolve` — Resolve a write-back conflict (`--keep-vault` / `--keep-remote` / `--recreate`)
- `zotio vault sync` — Export Zotero items to Obsidian/Logseq Markdown notes

**watch** — Background freshness

- `zotio watch` — Keep the local store fresh with periodic incremental syncs (`--interval`, `--once`); `--health` reports new findings per cycle

**workflow** — Declarative multi-step runs

- `zotio workflow run` — Run a JSON workflow spec in-process with per-step status


### Finding the right command

When you know what you want to do but not which command does it, ask the CLI directly:

```bash
zotio which "<capability in your own words>"
```

`which` resolves a natural-language capability query to the best matching command from this CLI's curated feature index. Exit code `0` means at least one match; exit code `2` means no confident match — fall back to `--help` or use a narrower query.

## Recipes


### Export reading annotations for the week

```bash
zotio annotations timeline --since 2026-05-04 --format markdown > this-week.md
```

Pull all highlights and notes from the past 7 days into a single markdown file for review or AI synthesis.

### Generate BibTeX for a collection branch

```bash
zotio collections export IDTUAULN --format bibtex > philosophy.bib
```

Export all items in a collection and its subcollections to a single .bib file for LaTeX use.

### Find papers missing PDFs and get their DOIs

```bash
zotio items missing-pdf --type journalArticle --agent --select data.DOI,data.title | jq '.[] | select(.data.DOI != null) | .data.DOI'
```

Get DOIs for journal articles without attached PDFs — pipe to a download script.

### Audit and fix tag drift

```bash
zotio tags audit --json
```

Find tags that differ only by case or variant (e.g., qualitative / Qualitative / qual) with item counts and merge suggestions.

### Check library venue distribution for a systematic review

```bash
zotio items venues --top 20 --agent --select venue,count,year_range
```

List the top 20 journals in your library with item counts — identify source distribution before a review.

## Auth Setup

**Reads** use the local Zotero desktop API at `localhost:23119` — no API key required while Zotero is running. **Writes** (`items create/update/delete`, `vault push`, `vault pull`, `vault resolve`) require a Zotero Web API key. Configure it once:

```bash
zotio auth set-token <key>
```

(or set the `ZOTERO_API_KEY` env var). When the configured base is the local API, writes auto-route to the Web API at `api.zotero.org` while reads stay local; a one-time stderr notice names the write target on your first write.

An API key is also needed to read **group libraries** or to read while the desktop app is **closed**.

Run `zotio doctor` to verify setup — it reports a `writes:` line (e.g. "available (auto-routed to Web API; reads stay local)", or read-only guidance when no key is set).

## Agent Mode

Add `--agent` to any command. Expands to: `--json --compact --no-input --no-color`. It does **not** auto-apply writes — mutating commands preview by default; pass `--yes` to apply.

- **Pipeable** — JSON on stdout, errors on stderr
- **Filterable** — `--select` keeps a subset of fields. Dotted paths descend into nested structures; arrays traverse element-wise. Critical for keeping context small on verbose APIs:

  ```bash
  zotio collections list --agent --select id,name,status
  ```
- **Previewable** — `--dry-run` shows the request without sending
- **Offline-friendly** — sync/search commands can use the local SQLite store when available
- **Non-interactive** — never prompts, every input is a flag
- **Explicit retries** — use `--idempotent` only when an already-existing create should count as success, and `--ignore-missing` only when a missing delete target should count as success

### Response envelope

Commands that read from the local store or the API wrap output in a provenance envelope:

```json
{
  "meta": {"source": "live" | "local", "synced_at": "...", "reason": "..."},
  "results": <data>
}
```

Parse `.results` for data and `.meta.source` to know whether it's live or local. A human-readable `N results (live)` summary is printed to stderr only when stdout is a terminal — piped/agent consumers get pure JSON on stdout.

## Agent Feedback

When you (or the agent) notice something off about this CLI, record it:

```
zotio feedback "the --since flag is inclusive but docs say exclusive"
zotio feedback --stdin < notes.txt
zotio feedback list --json --limit 10
```

Entries are stored locally at `~/.zotio/feedback.jsonl`. They are never POSTed unless `ZOTERO_FEEDBACK_ENDPOINT` is set AND either `--send` is passed or `ZOTERO_FEEDBACK_AUTO_SEND=true`. Default behavior is local-only.

Write what *surprised* you, not a bug report. Short, specific, one line: that is the part that compounds.

## Output Delivery

Every command accepts `--deliver <sink>`. The output goes to the named sink in addition to (or instead of) stdout, so agents can route command results without hand-piping. Three sinks are supported:

| Sink | Effect |
|------|--------|
| `stdout` | Default; write to stdout only |
| `file:<path>` | Atomically write output to `<path>` (tmp + rename) |
| `webhook:<url>` | POST the output body to the URL (`application/json` or `application/x-ndjson` when `--compact`) |

Unknown schemes are refused with a structured error naming the supported set. Webhook failures return non-zero and log the URL + HTTP status on stderr.

## Named Profiles

A profile is a saved set of flag values, reused across invocations. Use it when a scheduled agent calls the same command every run with the same configuration - HeyGen's "Beacon" pattern.

```
zotio profile save briefing --json
zotio --profile briefing collections list
zotio profile list --json
zotio profile show briefing
zotio profile delete briefing --yes
```

Explicit flags always win over profile values; profile values win over defaults. `agent-context` lists all available profiles under `available_profiles` so introspecting agents discover them at runtime.

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 2 | Usage error (wrong arguments) |
| 3 | Resource not found |
| 4 | Authentication required |
| 5 | API error (upstream issue) |
| 7 | Rate limited (wait and retry) |
| 10 | Config error |

## Argument Parsing

Parse `$ARGUMENTS`:

1. **Empty, `help`, or `--help`** → show `zotio --help` output
2. **Starts with `install`** → ends with `mcp` → MCP installation; otherwise → see Prerequisites above
3. **Anything else** → Direct Use (execute as CLI command with `--agent`)

## MCP Server Installation

Install the MCP binary from this CLI's published public-library entry or pre-built release, then register it:

```bash
claude mcp add zotio-mcp -- zotio-mcp
```

Verify: `claude mcp list`

## Direct Use

1. Check if installed: `which zotio`
   If not found, offer to install (see Prerequisites at the top of this skill).
2. Match the user query to the best command from the Unique Capabilities and Command Reference above.
3. Execute with the `--agent` flag:
   ```bash
   zotio <command> [subcommand] [args] --agent
   ```
4. If ambiguous, drill into subcommand help: `zotio <command> --help`.
