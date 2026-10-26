# Local read parity

`zotio` keeps a synced SQLite mirror of your library so reads can run **offline** and **fast** — full-text search, analytics, and audits don't need the desktop app open or the network up.

## Data sources

Every read command takes `--data-source`:

- `auto` (default) — live API with local fallback (annotation search is the exception: it is local-first, see [Freshness](#freshness))
- `live` — API only
- `local` — synced mirror only

Populate and refresh the mirror with `sync` and `watch`. `tail` is a change-event feed: it streams changes and keeps its own cursor, but it does not update the mirrored rows, so run `sync` or `watch` to refresh offline data:

```bash
zotio sync                                   # full sync to local SQLite
zotio search 'automation trust' --data-source local --json
zotio watch                                  # keep the mirror fresh
```

`sync --full` walks items in `dateAdded` order. If the API cannot provide stable ordering, sync skips the deletion sweep and reports `sweep_skipped_unstable_order`.

## Supported local scopes

The synced mirror supports these resource-specific reads:

- Item lists across the library, within a collection, for a parent's children, or restricted to top-level items. Filters include item type, tag, quick search, sorting, and pagination.
- Tag lists, including collection tags, with name search and pagination; individual tags by name.
- A collection's direct subcollections, with pagination.
- Trash lists, with pagination.
- Saved-search results through `searches run` and `--scope saved-search:KEY`, within the supported condition subset. This includes text fields, creators, tags, collections, item types, dates, and selected parent/child flags. Unsupported conditions refuse the whole search.

Top-level collection lists have no local planner and return an unsupported-scope error. Local item lists refuse `--since-version` and `--format` with an exit-9 precondition requiring the live API; they never fall back to an unfiltered dump.


## A deliberate, per-resource subsystem

Local read parity is Zotero-aware and grown **on demand, per resource** (`internal/store/query.go` and the `resolveLocal*` path) — it is *not* a generic query-planner layer. A local scope is added for a resource only where users actually pass filters against it, keeping the surface small and the behavior faithful to the live API rather than a lowest-common-denominator abstraction.

The reasoning, boundaries, and the rule for adding a new `--data-source local` scope are summarized in [Architecture decisions › Local read parity](../contributing/architecture-decisions.md#local-read-parity), with the full record in the repo. Read it before extending local reads.

## Freshness

`zotio doctor` reports cache freshness, and the MCP server exposes `zotero://freshness`. The generic `auto` resolver tries live first and falls back to the local mirror when the live call fails with a network error or when the desktop API lacks the endpoint ("No endpoint found"). Other HTTP errors propagate. There is no freshness threshold, so the fallback can serve arbitrarily old rows. `annotations search` is a deliberate exception: under `auto` it searches the local index first, because its stemming, Boolean operators, and ranking have no live equivalent, and it goes live only when there is no synced mirror. Pass `--refresh` or `--data-source live` to make `annotations search` read live. Inspect provenance, or pass `--require-fresh` where supported, when a decision needs current data.
