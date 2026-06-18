# Zotero, Obsidian, and `zotero-pp-cli`: how they fit together

This note explains where `zotero-pp-cli` sits relative to the Obsidian/Zotero
plugin ecosystem, so you (and future agents working on this repo) can decide
what belongs in this tool and what stays in a plugin. It is positioning, not a
feature spec.

## TL;DR

There are **two paradigms**, and they coexist by design:

- **Live / in-editor (pull):** Obsidian plugins + Zotero plugins. You are
  writing *right now*; you trigger an action; it reaches into Zotero (almost
  always via Better BibTeX's local JSON-RPC) and drops a citation, link, or
  annotation at your cursor. Great for cite-as-you-write. Fragile across Zotero
  major versions because of the BBT-RPC dependency.
- **Batch / agent (push):** `zotero-pp-cli`. It mirrors the library to a local
  SQLite store, then audits / dedups / enriches / searches / exports in bulk,
  scriptably, and exposes everything to LLM agents over MCP. Talks the **native
  Zotero local API** at `localhost:23119/api` — no Better BibTeX dependency.

**Keep the live tools for live writing. Let the CLI own the batch, maintenance,
and agent layer.** Don't add more sync/notes/AI plugins; that is the lane the
CLI is meant to consolidate.

## The three layers

```
1. DATA              Zotero + Better BibTeX        source of truth, citekeys, files
        │
2. LIVE / IN-EDITOR  Zotero Integration,           interactive, one item at a time,
   (pull, you-in-loop)  Actions & Tags links          needs the GUI, BBT-RPC-fragile
        │
3. BATCH / AGENT     zotero-pp-cli (+ MCP)          headless, bulk, idempotent,
   (push, automated)                                  scriptable, host-agnostic,
                                                       native localhost:23119 API
```

Layers 2 and 3 are complementary: different triggers, different time-scales.
Layer 2 = "I need a cite/annotation in this paragraph now." Layer 3 = "refresh
all my literature notes / audit the library / let an agent answer questions
across the whole library."

## A typical user stack (lean, not sprawl)

| Tool | Layer | Job |
|---|---|---|
| **Better BibTeX** (Zotero) | data | citekeys, BibTeX/CSL export, the JSON-RPC most Obsidian plugins ride |
| **Actions & Tags** + a "copy link" script (Zotero) | live | copy a labeled `zotero://select/...` / `zotero://open-pdf/...?annotation=` backlink while in Zotero (round-trip navigation, collection-aware) — **not** bibliography formatting |
| **Zotero Integration** (mgmeyers, Obsidian) | live | cite-as-you-write, import annotations into the current note, via BBT |

Three tools with clean roles. That is already disciplined. The sprawl *risk* is
adding a fourth/fifth sync, notes, or AI plugin.

## The plugin landscape, bucketed

Searching "zotero" in the Obsidian community store returns ~18 plugins. Grouped
by function (and maintenance reality as of late 2025 / Zotero 10 beta):

- **Cite-as-you-write / insertion:** Citations (stale, ~4 yr), Citation
  Extended, Zotero Citations (Pandoc), PandoCit, + Zotero Integration.
- **Literature notes (one note/item):** ZotLit (**broke on Zotero 9**), Simple
  Citations, Stratum, Zotero Direct, Zotero Notes Sync.
- **Library mirror into vault:** Zotero Sync (web API, read-only), Zotero Lib
  View, Local Zotero Mirror.
- **Linking:** Zotero Link, Zotero Cite PDF (overlap Actions & Tags scripts).
- **API bridge:** Zotero Bridge (ZotServer).
- **AI / RAG over the literature:** Zotero Research Assistant (Docling OCR +
  annotations + local Redis RAG chat), PaperForge (OCR + AI deep reading). Both
  "not reviewed by Obsidian staff," new, operationally heavy.

Two takeaways: most note/sync plugins are stale or version-fragile (the
ZotLit / Zotero-9 break is the canary), and the **AI/RAG plugins are the only
category that overlaps with where this CLI + MCP is heading** — agent access to
the library.

## Why the CLI is more version-durable

Every plugin above that does more than render text rides **Better BibTeX's
local JSON-RPC**, which is what breaks on each Zotero major version. This CLI
talks the **native Zotero local HTTP API** instead.

Verified against **Zotero 10.0-beta.7** on 2026-06-16: `GET
http://localhost:23119/api/users/0/items` returns full item objects — including
`data.citationKey` natively (no `extra` parsing needed), `data.collections`,
`version`, and `meta.numChildren` — exactly the shape this CLI's sync and query
layer expect. The native API is a more stable contract than the BBT-RPC stack
the plugins depend on.

> Run `zotero-pp-cli doctor --json` to confirm reachability and the resolved
> `base_url`/`library` on your machine.

## What to consolidate vs keep

**Keep in a plugin (the CLI should not try to own these):**

- **Cite-as-you-write** — inherently interactive at the cursor.
- **The `zotero://` deep-link gesture** — copy-a-backlink while browsing Zotero.

**Consolidate into the CLI (stop shopping for plugins here):**

- **Bulk literature notes / vault sync** — use `vault sync` (below) instead of
  ZotLit / Stratum / Simple Citations. Headless and version-durable.
- **Library Q&A / "chat with my library"** — use the CLI's **MCP server**
  (resources + prompts), host-agnostic (Claude Desktop/Code), instead of a
  heavyweight Redis/Docling Obsidian plugin.
- **Hygiene / dedup / enrichment / analytics / offline search** — CLI-only; no
  plugin does this well (`items audit`, `items dedup`, `items enrich`,
  `library stats`, `search`, `sql`).

**Decision rubric:**

- "Am I writing right now and need a cite/link/annotation in *this* note?"
  -> plugin (live).
- "Do I want to bulk-create/refresh notes, audit, dedup, enrich, or automate?"
  -> CLI.
- "Do I want an agent/LLM to query my library?" -> CLI (MCP).

## `vault sync` (the `49r4` feature) — scope and intent

`vault sync` materializes/refreshes one Markdown note per item into an Obsidian
or Logseq vault. It is deliberately the **batch, idempotent** counterpart to the
in-editor plugins — not a replacement for them:

- **Identity from the citekey.** Filenames key off `data.citationKey` (native in
  Zotero 7+/10), falling back to the Zotero item key. This lines up with your
  existing `[@citekey]` cites.
- **`zotero://` backlinks baked in.** Frontmatter carries
  `zotero://select/library/items/<key>`; each annotation links to
  `zotero://open-pdf/library/items/<parent>?annotation=<key>` — the same
  round-trip links you otherwise hand-copy with Actions & Tags, generated in
  bulk.
- **Idempotent, non-clobbering.** Re-running updates only the managed
  frontmatter keys and a fenced annotations block
  (`<!-- zotero-pp-cli:annotations ... -->`). Your prose and any extra
  frontmatter keys are preserved verbatim.
- **Scoped from the local store.** `--collection`, `--tag`, `--item-type`,
  `--limit` reuse the local query planner; run `sync` first.
- **Preview first.** `--dry-run` reports created/updated/unchanged without
  writing.

Use it for scheduled or one-shot bulk upkeep (e.g. "refresh every note in this
collection's annotations"); keep Zotero Integration for live, in-the-moment
writing. They write to the same vault without fighting: the CLI owns its fenced
block and managed keys, you own everything else.

## `vault push` / `vault pull` (the `15e0` feature) — note write-back

`vault sync` is one-way (Zotero → vault). `vault push` and `vault pull` add the
**opt-in reverse direction** for the user-owned `## Notes` region only: `push`
mirrors that region to a single tool-owned **Zotero child note**, and `pull`
folds remote edits to that note back into the region. Neither touches
bibliographic fields, and simultaneous edits on both sides are never
auto-merged — they surface as a conflict.

- **Reads stay local; writes go to the Web API.** Like every mutation in this
  CLI, the local API is GET-only, so `push` auto-routes writes to `api.zotero.org`
  (an `api_key` must be configured). Personal library by default; a `--group`
  target prints a one-time warning (members can read the note).
- **One managed child note per item.** First push **creates** it (POST, with a
  deterministic `Zotero-Write-Token` so an interrupted retry can't duplicate it);
  later pushes **PATCH** the same note (`If-Unmodified-Since-Version`). The note's
  first line is `Obsidian notes — <citekey>` so its origin is obvious in Zotero.
- **Verbatim renderer.** The Markdown is reproduced as readable `<p>` blocks with
  everything HTML-escaped and **nothing interpreted** — wikilinks, tables,
  callouts, and code are preserved exactly rather than half-rendered. (A richer
  opt-in renderer may come later; verbatim is the safe default.)
- **Conflict-safe.** A note is pushed only when its `## Notes` region changed
  since the last push *and* the remote note body has not diverged. If both sides
  changed, nothing is overwritten: a conflict file is written under
  `_vault-zotero-conflicts/` (local copy + remote HTML + the resolve command) and
  reported. A remote-only change on an otherwise-unchanged note is reported
  `remote_changed`, never silently hidden behind "unchanged".
- **Recovery.** `vault conflicts` lists unresolved artifacts; `vault resolve
  <citekey|key> --keep-vault` republishes the vault copy over the remote using the
  live version as the precondition (`--recreate` re-creates a note deleted in
  Zotero).
- **Preview first.** `vault push --dry-run` reports would-create / would-update /
  would-conflict and writes nothing — to the vault or to Zotero.
- **`vault pull` (fast-forward).** The reverse direction: when the remote child
  note changed but the local region is unchanged since the last sync, `pull`
  converts the note HTML back to text (reversing the verbatim renderer; only
  notes in the managed shape are touched — never arbitrary HTML) and rewrites the
  `## Notes` region. If both sides changed it reports a conflict and merges
  nothing. `--dry-run` previews.

> Mental model: **Obsidian is the editing surface; the Zotero child note is a
> managed mirror.** `vault pull` folds remote edits back on a clean fast-forward;
> simultaneous edits on both sides surface as a conflict, never an auto-merge.

### Note format contract (shared by sync and push)

`vault sync` establishes the structure `push` relies on, so run `sync` first:

- **Stable identity in frontmatter.** `zotero_key` (and `zotero_library`) make a
  note's item identity explicit. Re-syncs and pushes find a note by `zotero_key`,
  so renaming the file or changing the citekey never duplicates or orphans it.
  Notes created before `zotero_key` existed are recognized via their `zotero://`
  link and upgraded in place.
- **Managed vs user regions.** The tool owns the frontmatter keys, the title and
  abstract fences (`<!-- zotero-pp-cli:title/abstract -->`), and the annotations
  fence. You own the region between `<!-- zotero-pp-cli:notes-begin -->` and
  `<!-- zotero-pp-cli:notes-end -->` under `## Notes` — that, and only that, is
  what `push` sends to Zotero. A legacy single `## Notes` heading is migrated into
  markers automatically; an ambiguous layout is reported `needs_notes_boundary`
  and left untouched.
- **Hidden sync state.** Push records its baseline (`note_key`, `note_version`,
  `source_hash`, `remote_hash`, `renderer`) in a single
  `<!-- zotero-pp-cli:state {...} -->` comment, keeping Obsidian Properties free of
  bookkeeping.
- **Safe writes.** Vault files are written atomically (temp file + rename) and
  only when the on-disk bytes still match what was read, so a concurrent
  Obsidian/iCloud edit is reported (`file_busy`) rather than clobbered.

### `[vault]` config and collection names

Set defaults once instead of passing `--out` every run:

```toml
[vault]
root = "~/Vaults/dev"   # ~ is expanded
notes_dir = "Zotero"     # notes land in <root>/<notes_dir>
format = "obsidian"      # or "logseq"
```

`--out`/`--format` flags still override the config. Synced collections also render
human-readable names: notes carry `collection_names` (resolved from the local
store, falling back to the key when a collection isn't synced) alongside the
existing `collections` keys, which stay intact for Dataview queries.

### Round-trip in practice

```
zotero-pp-cli sync                       # refresh the local mirror
zotero-pp-cli vault sync                 # Zotero -> vault (uses [vault] config)
# ... write under "## Notes" in Obsidian ...
zotero-pp-cli vault push --dry-run       # preview Obsidian -> Zotero
zotero-pp-cli vault push                 # publish notes to Zotero child notes
zotero-pp-cli vault pull                 # fold edits made in the Zotero app back in
zotero-pp-cli vault conflicts            # if any push/pull reported a conflict
zotero-pp-cli vault resolve <citekey> --keep-vault
```
