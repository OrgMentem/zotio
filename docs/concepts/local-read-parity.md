# Local read parity

`zotio` keeps a synced SQLite mirror of your library so reads can run **offline** and **fast** — full-text search, analytics, and audits don't need the desktop app open or the network up.

## Data sources

Every read command takes `--data-source`:

- `auto` (default) — live API with local fallback
- `live` — API only
- `local` — synced mirror only

Populate and refresh the mirror with `sync`, `watch`, and `tail`:

```bash
zotio sync                                   # full sync to local SQLite
zotio search 'automation trust' --data-source local --json
zotio watch                                  # keep the mirror fresh
```

## A deliberate, per-resource subsystem

Local read parity is Zotero-aware and grown **on demand, per resource** (`internal/store/query.go` and the `resolveLocal*` path) — it is *not* a generic query-planner layer. A local scope is added for a resource only where users actually pass filters against it, keeping the surface small and the behavior faithful to the live API rather than a lowest-common-denominator abstraction.

The reasoning, boundaries, and the rule for adding a new `--data-source local` scope are summarized in [Architecture decisions › Local read parity](../contributing/architecture-decisions.md#local-read-parity), with the full record in the repo. Read it before extending local reads.

## Freshness

`zotio doctor` reports cache freshness, and the MCP server exposes `zotero://freshness`. When a local read might be stale, `auto` falls back to live so you don't silently read old data.
