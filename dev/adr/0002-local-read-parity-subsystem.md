# ADR 0002 — Local read parity is a first-class zotio subsystem

- **Status:** Accepted (2026-07-01).
- **Scope:** `internal/store/query.go` (the local query layer) and the `resolveLocal*` read path in `internal/cli/data_source.go`.
- **Deciders:** enieuwy.
- **Supersedes framing of:** a proposed generic planner layer for local read parity across resource workflows.

## Context

`zotio` targets Zotero's **local API** for reads and mirrors synced data into a local SQLite store so `--data-source local` (and `auto` fallback) work offline. The promise of offline mode is **parity**: a command that returns a key set live returns the same key set locally after sync, with the same scoping — not an accidental whole-library dump.

That parity is not free. The store is a generic `resource_type → JSON` cache; reproducing a live endpoint's semantics locally means replaying its filters/sort/pagination against JSON columns. This began as a Zotero-aware **item** query planner (`store.ItemQuery` + `QueryItems`, curated FTS via `buildSearchDocument`/`SearchByType`). Since then the layer has quietly accreted:

- `internal/store/query.go` is **~375 lines** of Zotero-specific read logic (item scoping, sort-field mapping, FTS document construction, FTS5 query normalization).
- **`QueryItems`/`ItemQuery` now has 8+ consumers** beyond the offline read path: `collections_bundle.go`, `import_scan.go`, `items_summarize.go`, `vault_sync.go`, `internal/mcp/resources.go`, plus `Search`/`SearchByType` used by `scope.go`, `search.go`, and `internal/mcp/tools.go`.

The proposal that triggered this ADR promoted `ItemQuery` into a **broad generic query-planner layer with per-resource planner interfaces** for collections, tags, searches, children, saved-search results, and export scopes — routing *all* local reads through planner interfaces with provenance flags.

Two facts make the framing, not just the priority, wrong:

1. **The layer already exists and is load-bearing.** It is not a candidate abstraction to be *introduced*; it is an owned subsystem to be *named and bounded*. The open question was never "should there be a query layer" — there is one, with a dozen callers — but "is it deliberate, or an accident we keep patching."
2. **A generic per-endpoint translator is over-built.** Zotero read endpoints have genuinely different shapes (item scoping vs. tag name filters vs. saved-search condition evaluation). A uniform planner interface would abstract over differences that don't rhyme, to serve offline traffic most of which is item reads. That is exactly the speculative abstraction to resist.

## Decision

**Declare local read parity a first-class, deliberately-bounded zotio subsystem, and grow it per demonstrated per-resource need — not as a generic planner layer.**

Concretely:

1. **`internal/store/query.go` is the owned local read model.** New scoped local reads add a typed query (fields + one SQL builder) here, mirroring `ItemQuery`/`QueryItems`. This is a hand-written Zotero read model, not a generated artifact — it is a subsystem, documented by this ADR.

2. **`resolveLocal` dispatches by path to per-resource planners.** The dispatch pattern is fixed: `resolveLocalItemList` returns `(data, handled, err)`; `handled=false` falls through to the generic dump. New resources add a sibling `resolveLocal<Resource>` with the same tri-state contract. The generic dump remains the honest fallback — it applies reproducible params (`limit`/`start` via `paginateLocalRows`) and warns only for genuinely unreproducible filters (`hasUnreproducibleParams`).

3. **Provenance carries a scoped flag.** A planner-served read sets `prov.Scoped = true`; a generic-dump read does not. Callers and agents can distinguish "this local result reproduced the live scope" from "this is a best-effort dump." This is the requested *provenance flag* — kept, but as a boolean on the existing envelope, not a new interface layer.

4. **Grow by demonstrated value, one resource at a time.** No planner is built ahead of a real consumer. Precedence: item children (done — feeds offline annotation export) > generic pagination (done) > tag-resource filtering (done, see Amendments) > saved-search results (done as a scope fallback, see Amendments) > unifying `search` through `resolveRead` (deferred, cleanup).

### What we explicitly do NOT build

- A generic `Planner` interface abstracting over all resource shapes.
- Per-resource planners for endpoints with no offline consumer (speculative parity).
- Local evaluation of saved-search condition sets **beyond the supported operator subset**. Inside the subset it exists (see Amendments); outside it the evaluator refuses rather than approximates.

## Consequences

- **The subsystem is legible.** A reader/agent knows `query.go` + `resolveLocal*` is *the* local read model, owned here, with a fixed extension pattern — not a pile of endpoint patches.
- **`Scoped` provenance is a stable contract.** Offline/agent workflows can gate on "was this reproduced or dumped."
- **Bounded blast radius.** Each new resource is a typed query + a `resolveLocal<Resource>` + parity tests; it can't silently change item behavior.
- **The proposed generic planner layer is rejected as over-built** and split into per-resource follow-up work (tags and saved-search, both now done; unify search, deferred), each gated on its own demonstrated need. The umbrella stays deferred.
- **Tradeoff (accepted):** we forgo a single uniform read abstraction. Adding a resource is a small amount of near-duplicated builder code rather than implementing an interface. For a read model over heterogeneous Zotero endpoints, explicit-per-shape is more honest than a lowest-common-denominator interface.
- **Regeneration risk is now documented:** this ADR is the carry-forward record for the subsystem.

## Alternatives considered

| Option | Why not |
|---|---|
| **Generic query-planner layer** (proposal) | Over-built: uniform interface over heterogeneous endpoint shapes to serve mostly-item offline traffic. Speculative parity for endpoints with no consumer. |
| **Leave it unnamed** (no ADR) | The layer is already load-bearing (12 callers); leaving it unnamed guarantees drift and repeated "is this the right place" questions. |
| **Move parity upstream to cli-printing-press** | The generic *dump* + freshness plumbing is generic and could go upstream; the *Zotero-aware* scoping (item types, sort fields, saved-search operators, FTS document) is domain-specific and belongs to zotio. Generic mechanics may still be filed upstream separately. |
| **Per-resource planners, grown on demand (chosen)** | Matches the code that already exists, keeps the extension pattern fixed and testable, and refuses abstraction ahead of need. |

## Validation

- Consumer census (grep): `QueryItems`/`ItemQuery` in `collections_bundle.go`, `import_scan.go`, `items_summarize.go`, `vault_sync.go`, `mcp/resources.go`; `Search`/`SearchByType` in `scope.go`, `search.go`, `mcp/tools.go`.
- Slices A (item children) and B (generic pagination + warning scoping) implemented under this pattern, each with fixture-backed parity tests and mutation checks; `go build ./...` and `go test ./internal/cli/ ./internal/store/` green.

## References

- AGENTS.md — "Zotero API Surface" (local API is GET-only / evolving surface).

## Amendments

- **2026-07-08:** The generator was retired; this ADR is the carry-forward record for the local read parity subsystem.
- **2026-10-12:** Tag-resource filtering is implemented (`resolveLocalTagList` in `data_source.go`, covered by `data_source_local_query_planner_test.go`; commits `a443d2a`, `b8051a4`). Decision item 4 no longer lists it as deferred.
- **2026-10-12:** Saved-search evaluation is implemented as an offline fallback for `--scope saved-search:KEY` (`internal/cli/savedsearch_local.go`, wired into `resolveScopeLive` and `searches run`; covered by `savedsearch_local_test.go`). Decision item 4 no longer lists it as deferred, and the "do NOT build" line now bounds it instead of forbidding it. The decisions:
  - **Live stays authoritative.** Zotero evaluates every condition type, so a reachable desktop answers first. The mirror is consulted only when the desktop is not the configured base, does not answer, or refuses with HTTP 403; `--data-source live` disables the fallback. The result carries `scopeResult.LocalEvaluation` (which search, how fresh the mirror, why live was skipped) and a stderr notice.
  - **Explicit operator subset, no partial evaluation.** The evaluator replays Zotero's `search.js` semantics for text fields, `creator`/`author`/`editor`/`bookAuthor`, `tag`, `collection` (+ `recursive`), `itemType`, `date`/`dateAdded`/`dateModified` (`is`/`isNot`/`isBefore`/`isAfter`), `joinMode`, `noChildren`, and `includeParents`/`includeChildren`/`includeParentsAndChildren`. A definition with any other condition, operator, or value (full text, notes, `anyField`, nested saved searches, condition groups, `isInTheLast`, LIKE wildcards, a collection the mirror does not hold, a mirrored item date it cannot parse) is refused whole. The precondition detail names each offending part; the unmet `live_local_api` precondition (exit 9) is the outcome, never a smaller cohort.
  - **Deviation from the "typed query in `query.go`" pattern.** Zotero matches text after accent, case, quote, and dash folding (`normalizeForSearch`), which SQLite cannot reproduce over the JSON mirror. The evaluator therefore loads the mirrored items and evaluates in Go. `query.go` is unchanged.
  - **`searches run` uses it too.** `--data-source local`, and the auto fallback when the results endpoint cannot execute the search (network error or 404), evaluate the synced definition and print the matched item payloads (children included, as Zotero's search returns them) in item-list order. `--data-source live` never falls back. An unsupported condition keeps the `live_local_api` refusal. The capability entry became three routes (default declares nothing, `local` requires `synced_store`, `live` requires `live_local_api`), probed by `TestCapabilityRequiresMatchesOfflineBehaviour`.
