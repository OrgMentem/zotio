---
template: home.html
---

`zotio` is **the trust-and-automation layer for Zotero**: local-first search and auditing, preview-first Web API writes for safe change, and bounded, provenance-tagged context for humans, scripts, and MCP agents. Use it to prove a library is ready, fix it safely, ingest material with review, or give agents and vaults trustworthy context.

## How it works

Reads stay on your machine. Writes split by intent: **creating a new item** (with its attachments/PDFs) prefers the local desktop connector (`localhost:23119`, no key — the same channel the browser "Save to Zotero" button uses), while **everything else** — field edits, deletes, enrichment, tag ops, moves, and `collections` create/update — routes to the Zotero Web API and needs a configured key. Every write is preview-first, the version read happens locally, and the applied change is replayed into your local mirror so a follow-up read sees it.

<div class="zotio-diagram-wrap">
--8<-- "docs/assets/architecture.svg"
</div>

| Plane | Backend | Needs a key? |
|---|---|---|
| **Read** | Local Zotero API (`localhost:23119`) + synced SQLite mirror | No |
| **Write — new item** | Local desktop connector when personal + desktop up; else Web API | No (connector path) |
| **Write — everything else** | Zotero Web API (`api.zotero.org`) — edits, deletes, enrich, tags, moves | Yes — configured once |
| **External** | CrossRef · OpenAlex · Semantic Scholar · Unpaywall · OpenCitations | No (feeds enrich/import) |
| **Local-only** | Files, desktop launch, vault, introspection | No |

Run [`zotio doctor`](reference/commands.md) any time to see connectivity, cache freshness, and a `writes:` line telling you whether write-back is available.

## Quickstart

```bash
zotio doctor                       # verify Zotero is running and reachable
zotio sync                         # mirror your library to local SQLite
zotio items retract-check          # check every DOI against Crossref's Retraction Watch data
zotio library health --for citation --fail-on high   # fit to cite? (exit 11 if not)
zotio items bibcheck paper.tex --fail-on-unknown     # every \cite{} resolves? (exit 11 if not)
zotio search 'automation trust' --data-source local --json
```

**No Zotero yet?** `zotio demo` seeds a bundled sample library — 34 classic papers, one genuinely retracted — so local checks (`retract-check`, `library health`, `bibcheck`, `search`) run with no desktop app and no API key under `ZOTIO_DEMO=1` and `--data-source local`; `doctor` and `sync` still need the real desktop. `ZOTIO_DEMO=1 zotio --data-source local items retract-check` catches the bundled library's retracted paper on the spot:

![zotio demo tour](assets/demos/demo-tour.gif)

New here? [Install the CLI](guide/install.md), then [authenticate](guide/authentication.md) if you need writes.

## Missing PDFs?

If your library has items without PDFs, [*papio*](https://github.com/orgmentem/papio) finds and downloads validated, provenance-tracked PDFs from open access and your own institutional subscriptions through your normal browser, then hands them to zotio for preview-first import.

## Where to go next

<div class="grid cards" markdown>

- **[Install](guide/install.md)** — the CLI, the agent skill, and the MCP server.
- **[Authentication](guide/authentication.md)** — keyless reads; when you need a Web API key.
- **[Obsidian / Logseq vault](guide/vault.md)** — conflict-safe, two-way note round-trip.
- **[Command reference](reference/commands.md)** — every command, generated from the binary.
- **[CI for your bibliography](guide/ci.md)** — gate a paper or thesis repo on library health, with a live badge.
- **[Workflows & triggers](guide/workflows.md)** — chain steps into one previewed, one-approval, resumable run; fire them automatically on library changes.
- **[Use in a coding agent](guide/agent-skill.md)** — drive the CLI from Claude Code or any agent (or via MCP).
- **[Highlights](reference/highlights.md)** — the hero features `zotio which` resolves against.
- **[Capabilities & trust model](reference/capabilities.md)** — read/write classification for every command.
- **[Zotero API behavior](concepts/zotero-api.md)** — local vs. Web API, GET-only, schema.
- **[Safe-by-default writes](concepts/write-safety.md)** — how the mutation engine protects your library.

</div>
