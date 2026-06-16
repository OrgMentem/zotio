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
