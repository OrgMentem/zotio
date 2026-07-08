---
name: zotio
description: "Use when the user wants to search, script, or automate a Zotero library — even if they don't say \"Zotero\" or \"BibTeX\": local-first library search, CI-gateable health reports, preview-first writes with undo, reviewable PDF/DOI import, annotation export, Obsidian/Logseq vault sync, plus an MCP server for agents. Trigger phrases: `search my Zotero library`, `check my library health`, `import this DOI`, `export BibTeX`, `find papers missing PDFs`, `export my annotations`, `undo that Zotero change`, `audit my tags`, `use zotero`, `open this paper`."
license: "MIT"
compatibility: "Requires the zotio binary on PATH and a running Zotero desktop app (local API); writes need a Zotero Web API key."
argument-hint: "<command> [args] | install cli|mcp"
allowed-tools: "Read Bash"
metadata:
  author: "OrgMentem"
  openclaw:
    requires:
      bins:
        - zotio
---

# zotio — Zotero automation CLI

<!-- PATCH: agentskills.io conformance: spec-clean frontmatter (added compatibility, moved author under metadata), a ## Gotchas section, and a ~295-line / ~5k-token budget by compressing Hero Capabilities and deferring the full command tree to runtime discovery. Kept single-file distribution: the docs distribute the skill as a lone SKILL.md (copy or raw URL), so no bundled reference files. -->

> Full command tree: ask the CLI at runtime — `zotio --help`, `zotio <command> --help`, or `zotio agent-context --pretty`. Installation, auth, and longer usage live in `README.md` and the [docs site](https://orgmentem.github.io/zotio/).

## Prerequisites: Install the CLI

This skill drives the `zotio` binary. **You must verify the CLI is installed before invoking any command from this skill.** If it is missing, install it first:

1. Install via Homebrew (macOS / Linux):
   ```bash
   brew install orgmentem/tap/zotio
   ```
   Or download a signed binary from the [releases page](https://github.com/OrgMentem/zotio/releases) and put it on your `$PATH`.
2. Verify: `zotio version`
3. From source: `go build -o zotio ./cmd/zotio`, then put the binary on your `$PATH`.

If `zotio version` reports "command not found" after install, the install step did not put the binary on `$PATH`. Do not proceed with skill commands until verification succeeds.

This CLI connects directly to your running Zotero desktop app — reads need no API key. Writes (`items create/update/delete/enrich`, `import apply`, `vault push`) require a Zotero Web API key, configured once via `auth set-token`, and are preview-first: every mutation plans by default, applies only under `--yes`, and is recorded to an undoable journal. It syncs your library to a local SQLite store for offline search and compound queries, then layers on the capabilities below — health reports, reviewable import, metadata enrichment, reproducible exports, and vault sync.

## When to Use This CLI

Use zotio when you need to script or automate your Zotero library: batch-export a collection's BibTeX before a deadline, find papers missing PDFs for a download script, extract this week's annotations for synthesis, or audit tag consistency before sharing a group library. It is especially useful as an MCP tool for agents that need to search or read a researcher's Zotero library.

## Gotchas

Non-obvious facts that defy reasonable assumptions — read before running commands:

- **Reads are local and keyless; writes are not.** Reads hit the Zotero desktop local API (`localhost:23119`) and need no key, but only while the app is running. **Group libraries** and reads while Zotero is **closed** require a Web API key.
- **The local API is GET-only.** Mutating commands against a local base **auto-route to the Web API** (`api.zotero.org`) and require a key set via `auth set-token`; with no key they fail the read-only guard. A one-time stderr notice names the write target on the first write. Web API writes sync back down to the desktop.
- **`--agent` does NOT auto-apply writes.** It expands to `--json --compact --no-input --no-color` only; mutating commands still preview by default — pass `--yes` to apply.
- **`--agent`/piped output is a provenance envelope**, not bare data: parse `.results` for the payload and `.meta.source` (`live` vs `local`) for freshness.
- **Verify setup with `zotio doctor`** — its `writes:` line reports whether writes are available (key present / auto-routed) or read-only.

## Hero Capabilities

The curated feature set — the same index `zotio which "<goal>"` resolves natural-language queries against. One line each; run `zotio <command> --help` for flags and examples.

### Library trust & health

- **`library health`** — Ranked, CI-gateable report (citekey conflicts, duplicates, missing metadata, tag drift, broken attachments) with `--for` citation/review presets; gate CI with `--fail-on` (exit 11) and publish a shields.io badge with `--badge`.
- **`items duplicates`** — Detect likely duplicates by DOI or normalized title; `duplicates resolve` merges them preview-first. Duplicates corrupt PRISMA counts before a manuscript.
- **`items retract-check`** — Check DOI-bearing items against Crossref's Retraction Watch data; also gates `library health` via `--check-retractions`. Catch a retracted citation before a reviewer does.
- **`collections gaps`** — Rank most-cited papers missing from your library (citation-graph gap analysis via OpenCitations + Semantic Scholar).
- **`tags audit`** — Group tags differing only by case/variant, with counts and ready-to-run merge commands. Dirty tags produce unreliable filtered exports.
- **`items audit`** — Count/list items missing PDFs, abstracts, DOIs, tags, or citation fields (`--missing-citation`); `--verify-files` checks PDFs exist on disk.
- **`items missing-pdf`** — List articles/chapters with no attached PDF — a scriptable download queue (e.g. for Unpaywall).
- **`library stats`** — Library broken down by item type, year, and top journals — a dashboard in one command.
- **`schema drift`** — Detect item types/fields/creator fields a Zotero upgrade added or removed vs a saved baseline. Run after upgrading Zotero.

### Safe writes & import

- **`import scan`** — Reviewable ingest: triage a PDF folder (new vs duplicate vs attach-candidate), resolve metadata, apply schema-valid creates from an editable manifest. Every create previewed, deduplicated, validated.
- **`import doi|pmid|arxiv|isbn`** — Turn an identifier into a schema-valid Zotero item without opening a browser.
- **`items enrich`** — Fill missing DOIs, abstracts, and OA PDF links from CrossRef/OpenAlex/Semantic Scholar/Unpaywall, preview-first with provenance; `--validate` cross-checks stored DOIs read-only.
- **`items preprint-check`** — Find arXiv preprints since published in a journal (CrossRef); `preprint-check fix` upgrades them to the published DOI, preview-first.
- **`journal undo`** — Every applied write is journaled; `journal undo <run-id>` reverses reversible runs (tag renames, collection moves) and loudly refuses the rest.

### Agent & automation surface

- **`items summarize`** — Assemble a bounded, provenance-tagged context bundle (citation, abstract, your annotations, capped fulltext excerpt) for an item or collection — never calls a model.
- **`export snapshot`** — Reproducible, resumable full-library JSONL export with a lockfile (key, version, content hash) — diff lockfiles to prove what changed between handoffs.
- **`watch`** — Periodic incremental syncs (`--interval`, `--once`); `--health` diffs library health between cycles and reports new findings to stdout or a webhook.
- **`workflow run`** — Run a declarative multi-step spec (JSON) in-process with per-step status and continue-on-error — replaces brittle shell chains.
- **`init`** — Guided first run (detect Zotero, enable local API, set key, first sync, health check); agent-safe under `--no-input` (unmet steps exit 9 with a step report).

### Reading workflow

- **`reading-list`** — Oldest unread papers by date added, with abstract preview — the next paper to triage or fetch fulltext for.
- **`annotations export`** — Export highlights and notes from a collection or tag set as one markdown/JSON file, one section per paper.
- **`annotations timeline`** — Annotations ordered by date — reconstruct what you read in any time window.
- **`items open`** — Jump from CLI results to the item in the Zotero desktop app (`--launch`).
- **`items note-template`** — Generate a pre-filled markdown reading note (frontmatter + abstract + empty Annotations section) for Obsidian/Logseq.
- **`library wrapped`** — Your Zotero year in review (items by month/type, top venues/authors, annotation activity, PDF coverage) with a shareable SVG card.

### Export & citations

- **`collections export`** — Export a collection and all subcollections as one BibTeX or CSL-JSON file, preserving structure in comments.
- **`items citekey-conflicts`** — Find items missing a Better BibTeX key or with duplicate keys before they break LaTeX compilation.
- **`items bibcheck`** — Check a manuscript (`.tex` or pandoc Markdown) against your library: every `\cite`/`@citekey` resolved, unknown/ambiguous keys flagged; `--fail-on-unknown` exits 11 for CI.

### Vault sync & write-back

Round-trip your library to an Obsidian/Logseq Markdown vault and back. Run `vault sync` first (populates from the local store), then `push` your edits to Zotero and `pull` remote changes back.

- **`vault sync`** — Export Zotero → Markdown notes, one file per item. Idempotent: refreshes a managed frontmatter block and fenced annotations block while preserving your prose. Resolves output dir/format from the `[vault]` config, so `--out` is optional.
- **`vault push`** — Write-back Obsidian → Zotero: mirrors each note's user-owned `## Notes` region into one managed child note. Conflict-safe — never overwrites a diverged note; writes a conflict artifact instead. Reads local, writes the Web API (key required).
- **`vault pull`** — Fold remote child-note edits into the `## Notes` region, fast-forward only; reports a conflict (never merges) if both sides changed.
- **`vault conflicts`** — List unresolved write-back conflict artifacts.
- **`vault resolve`** — Resolve a conflict by citekey/item key: `--keep-vault` (republish vault over remote), `--keep-remote` (pull remote over vault, discarding local edits), or `--recreate` (re-create a child note deleted in Zotero).

Configure the vault location and format once in `~/.config/zotio/config.toml` (flags `--out`/`--format` override):

```toml
[vault]
root = "~/Vaults/dev"   # ~ is expanded; base output dir
notes_dir = "Zotero"     # notes land in <root>/<notes_dir>
format = "obsidian"      # or "logseq"
```

## Command Reference

For the full command tree, ask the CLI at runtime — `zotio --help`, `zotio <command> --help`, or `zotio agent-context --pretty` (always current) — or see the docs site's Reference → Commands.


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

The `zotio-mcp` binary ships alongside the CLI — the Homebrew formula (`brew install orgmentem/tap/zotio`) and every [release](https://github.com/OrgMentem/zotio/releases) archive include both binaries. Once `zotio-mcp` is on your `$PATH`, register it:

```bash
claude mcp add zotero zotio-mcp -e ZOTERO_API_KEY=<your-key>
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
