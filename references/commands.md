# zotio Command Reference

A grouped map of the command surface. This is a partial, curated view — for the always-current tree, ask the CLI at runtime: `zotio --help`, `zotio <command> --help`, or `zotio agent-context --pretty`. To resolve a capability to a command, use `zotio which "<goal>"`.

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

**demo** — Zero-setup trial sandbox

- `zotio demo` — Seed a bundled sample library into a sandbox (`--reset` re-seeds); `ZOTIO_DEMO=1 zotio <command>` runs any command against it, never touching the real store or credentials

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
