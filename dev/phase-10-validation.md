# Phase 10 validation — 2026-07-09

Pre-build validation of the five Phase 10 items in `notes/roadmap.md`, from the dev side
(three read-only codebase scouts) and the user side (Zotero-forum demand evidence,
competitive landscape, marketing-channel intel). Verdict format per item: dev feasibility,
user demand, wedge, decision.

Method notes: no live library was available on this machine (store had 0 item rows), so
real-world relation/ORCID density is an open evidence gap; density claims below rest on
API-shape guarantees, not measured data. External demand evidence is linked inline.

## Summary table

| # | Item | Dev | Demand | Wedge | Decision |
|---|------|-----|--------|-------|----------|
| 1 | Citation-graph discovery → import manifest | Backward: half-built. Forward: new work | Strong (fresh forum requests; incumbents don't land in Zotero) | Strong | **GO**, backward first |
| 2 | Related-items graph | Cheaper than assumed (no schema migration for MVP) | Moderate ("Related Items is almost useful") | Moderate | **GO**, first slice |
| 3 | Creator audit → fix | Audit cheap; fix needs tiering + undo caveat | Strong (15 years of threads; no plugin exists) | Strong | **GO**, reshaped: tiered |
| 4 | ORCID persistence | "Nearly free" is FALSE for persistence | Real but upstream-blocked (Zotero has no field) | Weak standalone | **RESHAPE**: capture-as-evidence, merged into #3 |
| 5 | Screening + PRISMA | Readiness preset already shipped | Channel-validated for *pre-screening hygiene* only | Strong (narrow) / weak (broad) | **NARROW**: PRISMA-input counts; no screening |

## 1. Citation-graph discovery (GO — backward first)

**Dev.** Backward chasing is half-built: `collections gaps` already does OpenCitations COCI
references with Semantic Scholar fallback, library-DOI exclusion, ranking, and CrossRef
title fill (`internal/cli/collections_gaps.go:222-283`). CrossRef `reference` arrays and
OpenAlex `referenced_works` are fetched-and-dropped by current structs (`import_doi.go:15-28`,
`items_enrich.go:772-780`) — an immediate backward-edge source on endpoints we already call.
Forward chasing is genuinely new (needs OpenAlex work IDs we currently drop, or COCI v2
`/citations/{id}`, plus pagination/bounding). Manifest integration is moderate: a discovery
producer can emit `importManifest` entries (`action=create`, `status=resolved`, empty `path`
is legal for metadata-only creates), **but**:
- `import apply` performs NO library dedupe (`import_apply.go:107-111`); scan-time dedupe is
  DOI-only (`import_scan.go:139-167`). Snowballing multiplies duplicates — the producer MUST
  dedupe (DOI + normalized title, reusing `items_duplicates.go:131-174` logic) before emitting,
  or the reviewable-import safety claim regresses.
- Manifest schema v1 is PDF-shaped; discovery provenance (source item, edge direction,
  provider, rank) needs a schema extension, not `note` overloading.
- Wire the dormant `internal/cache` here (key = `provider|GET|url|fields`, raw-JSON values,
  2xx-only). The Zotero client cache (`internal/client`, 5-min TTL) is separate; don't collide.
- Semantic Scholar `/references` truncates silently at 1000; surface truncation.

**User.** Fresh demand: [June 2025 forum request "Automatically Import a Paper's References"](https://forums.zotero.org/discussion/124709/) plus a long thread history (77659, 65441, 112322).
Incumbents: [zotero-reference](https://github.com/MuiseDestiny/zotero-reference) (GUI popup
inside Zotero, per-paper, marks in-library by DOI); [citationchaser](https://github.com/nealhaddaway/citationchaser)
(R/Shiny, transparent batch fwd+back chasing, exports RIS — no Zotero integration, no
library dedupe). Nothing does batch, headless, deduped-against-your-library, preview-first.

**Wedge (strong):** "Snowball a whole collection and get the missing papers as a reviewable,
deduped import manifest in your own library" — CI/agent-compatible, which neither incumbent is.
citationchaser is Haddaway's tool; the open outreach bead to him is a validation channel, not
competition (his tool's PRISMA-transparency framing is the vocabulary to adopt).

**Slices:** (a) extract a shared discovery producer from `collections gaps`, backward-only,
manifest output, dedupe-before-emit, provider cache; (b) forward chase; keep `collections gaps`
as a thin consumer of the producer.

## 2. Related-items graph (GO — cheapest slice, do first)

**Dev.** The roadmap's "synced today, discarded" is HALF right: relations persist in raw item
JSON in `resources.data` (nothing strips them at ingest — `store.go:618-701`), but are
unindexed, unqueried, and stripped only from `--compact` presentation output (`helpers.go:641-716`,
the likely source of the "discarded" belief). MVP needs NO schema migration: `json_each` over
`$.data.relations` + a URI parser (`http://zotero.org/users|groups/<id>/items/<KEY>` — values
are URIs, never bare keys; predicates like `dc:relation` contain colons, so parse as URLs).
Incoming edges = library scan capped at the existing `graphNodeCap=500` pattern; a normalized
`item_relations` projection is a later optimization via the empty `migrateExtras` hook.
Off-store/cross-library targets must be preserved as external edges (`target_uri` +
`target_present:false`), never dropped. Output: boring JSON matching the Phase-5 MCP graph
resource pattern (top-level key + array + `truncated`); no DOT/nodes-edges framework exists
and none should be invented.

**User.** Moderate: ["'Related Items' is almost useful"](https://forums.zotero.org/discussion/120613/),
[search-by-relationship request](https://forums.zotero.org/discussion/123402/). Also the
natural landing surface for discovery edges (#1) and the MCP/agent graph story.

**Slice:** `items related <key>` (outgoing + capped incoming, `--data-source local`) + MCP
`zotero://items/{key}/related`. Requires `synced_store` for incoming; document that live-only
gives outgoing.

## 3. Creator audit → fix (GO — reshaped into confidence tiers)

**Dev.** Audit is cheap: creators are queryable via `json_each` today (`items_authors.go:59-92`
already inventories them). Fix is a per-item full-`creators`-array PATCH with version
precondition — same shape as `tags rename` (`tags_rename.go:117-125`). Three real costs the
roadmap missed:
- **Detection is NOT tags-normalization.** Tags audit folds case/whitespace only, deliberately
  no fuzzy tier (`tags_audit.go:295-297`). "J. Smith"/"John Smith"/"Smith, John" needs a
  creator-specific heuristic (last-name exact + compatible-initials) with confidence classes.
- **Journal undo refuses creator renames** (only tag/collection membership is reversible).
  Ship as journaled-but-not-undoable, documented — consistent with the recorded undo decision.
- Write-through (`write_through.go:136-163`) only supports tags/collections/scalar fields;
  creator arrays need explicit support or a store-refresh fallback.

**User.** Strongest demand of the five. Forum threads spanning 2009–2024, and Zotero's own
people confirming the gap in [July 2024](https://forums.zotero.org/discussion/115879/):
adamsmith — "I've always thought that'd be an area where Zotero could do a lot more, but I'm
not aware of any work on this." No plugin does it. **Critical caveat from the same community:**
the canonical name form is *style-dependent and contested* (Chicago wants fullest form, APA
wants initials; changing stored names changes citations). An aggressive autofix would be wrong.

**Reshaped contract:** `creators audit` reports three tiers — (1) exact-after-normalization
variants (case/whitespace/punctuation): safe, auto-fixable; (2) initial-vs-full-name variants:
listed with evidence, fixable only via explicit `--map from=to` style consent; (3) ambiguous
common surnames: diagnostic only, unless ORCID corroborates (see #4). `creators audit fix`
touches tier 1 by default, tier 2 only with explicit mappings. Non-goal, stated in help:
deciding the *style-correct* form of a name.

## 4. ORCID (RESHAPE — capture-as-evidence, merged into #3)

**Dev.** The roadmap's "nearly free" claim is FALSE for persistence and the doc has been
corrected. Parsing IS nearly free (CrossRef `author.ORCID`, OpenAlex `authorships[].author.orcid`
are in payloads we already fetch and currently drop). But Zotero has NO creator ORCID field —
`/creatorFields` is exactly {firstName, lastName, name} — so durable persistence forces a
design choice:
- **Extra-line convention:** Zotero-visible and syncs, but creator-scoped data in an item
  field needs a multi-author-safe syntax, and (blocking) the enrich Extra path currently
  *clobbers* Extra — see bug below. Not viable until that contract is fixed and a syntax is
  designed.
- **Local sidecar table** (via the empty `migrateExtras` hook): structured, queryable,
  honest about being local-only. Right shape for *corroboration evidence*.

**User.** Real demand, but the gap is upstream: Zotero's lead dev [names ORCID as the
grouping mechanism for name variants and confirms there's nowhere to store it today](https://forums.zotero.org/discussion/115879/)
(dstillman/aborel, July 2024). zotio can't fix the upstream schema; it CAN use ORCIDs as
disambiguation evidence.

**Decision:** demote from standalone item to a sub-feature of #3: during creator audit (and
opportunistically during enrich), capture provider ORCIDs into a local sidecar keyed
(library, item_key, creator_index, orcid, source), and use agreement/disagreement to promote
or demote variant-tier confidence. No Zotero write. Revisit Extra persistence only if a
documented convention emerges upstream.

**Incidental P1 found during validation (`inscribi-skfdi`):** `items enrich --yes` replaces
the item's entire Extra with the provenance line (`items_enrich.go:503` +
`currentExtra` placeholder at `:606-614`) — destroys Better BibTeX citation keys; preview
doesn't show the overwrite. Fix independently of Phase 10.

## 5. Screening + PRISMA (NARROW — PRISMA-input reporting only)

**Dev.** Screening *readiness* already shipped in v0.5.0's world: `library health
--for systematic-review` (duplicates, missing abstract, missing PDF, attachments; gate at
high). The open marketing beads (SR Toolbox, Haddaway, ESMIG, r/PhD) already pitch exactly
this + `export snapshot` lockfile as "reproducible pre-screening hygiene."

**User.** Incumbents own screening itself: Rayyan (web UI, PRISMA flow diagrams, dedupe
auto-resolver), ASReview (active-learning screening + `datatools` CLI dedupe),
prisma-review-tool (Python CLI for PRISMA tracking). Screening decisions are also where
no-LLM zotio has nothing to add. What incumbents do NOT own: the Zotero-native corpus, its
hygiene certification, and reproducible handoff.

**Decision:** do NOT build screening/decision logs. Add one small slice when (and only when)
the SR Toolbox/Haddaway outreach produces a demand signal: PRISMA-*input* reporting — machine-
readable identification-stage counts (records identified per source, duplicates removed with
method, records after dedupe) emitted from existing dedupe/health machinery, suitable for
pasting into a PRISMA 2020 flow diagram, plus the already-shipped lockfile for handoff.
Complement, never compete: the wedge is "arrive at Rayyan/ASReview with a certified, deduped,
counted corpus."

## Revised Phase 10 sequencing

1. **`items related`** (#2) — smallest, no migration, unlocks graph story.
2. **`creators audit`** read-only with tiers + ORCID sidecar capture (#3a + #4).
3. **`creators audit fix`** tier-1/tier-2-with-mapping (#3b).
4. **Discovery producer, backward** + manifest provenance + dedupe-before-emit + provider
   cache (#1a). `collections gaps` becomes a consumer.
5. **Forward chase** (#1b).
6. **PRISMA-input counts** (#5) — gated on outreach signal.

Prerequisite fix (independent): enrich Extra clobber (`inscribi-skfdi`).

Open evidence gaps: relation/ORCID density in real libraries (measure on first synced
library available); whether `--map`-style consent for tier-2 creator fixes matches how users
actually want to express canonical names (validate in the forum threads when announcing).
