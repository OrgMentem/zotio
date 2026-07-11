# zotio — Product Roadmap

## Provenance & status

- **Date:** 2026-06-27 · **Branch:** `reprint-4.25.0` (HEAD `97775bb`).
- **How this was derived:** reviewed 26 feature-opportunity records → drafted a
  value-first PRD → had it improved by a second model (GPT-5.5 Pro via Oracle, session
  `zotero-roadmap-pdr-4`; reattach: `oracle session zotero-roadmap-pdr-4 --render`) → reconciled
  every recommendation against the live codebase and `docs/zotero-api-coverage.md`.
- **Intent:** maximize end-user value and UX, **not** match the issue tracker. This document is
  the source of truth for sequencing; the source records are raw material, not a checklist.
- **Grounding rule:** every item here is feasible against Zotero's *actual* API. The hard
  constraints (local API is GET-only; writes route to the Web API with a key; stale local reads
  vs. cloud writes race to `412`; schema endpoints are global; `/items/new` is Web-only; fast
  6–10 week release cadence) live in `docs/zotero-api-coverage.md` and are assumed throughout.

## Product thesis

`zotio` is **the trust-and-automation layer for Zotero**: local-fast reads for searching
and auditing, preview-first Web API writes for safe change, and bounded, provenance-tagged context
for humans, scripts, and MCP agents.

It is **not** "every Zotero endpoint in a terminal." It is the tool you reach for when the GUI
becomes too manual: **find the problems that bite downstream and fix them safely, ingest material
with review, and give agents/vaults trustworthy context.** The CLI is **no-LLM-by-design** — it
assembles context bundles (`items summarize`) but never calls a model itself.

## The three hero capabilities

### 1. Catch the reference problems that break downstream — before they do

A read-only diagnostic, `library health`, with a `--for <preset>` that declares **which** downstream
the checks target. ("Submission-ready" was dropped as a slogan: a Zotero library is almost never
itself submitted; it *feeds* something that is. The preset names that something.)

| `--for` | The job — and where it's "submitted" | Checks | default `--fail-on` |
|---|---|---|---|
| `citation` | manuscript/thesis bibliography → journal/committee (via Better BibTeX `.bib`, the Word/Docs plugin, or CSL-JSON) | citekey conflicts/missing, citation-core fields, duplicates | `high` |
| `systematic-review` | PRISMA screening corpus → review manuscript + flow diagram (+ dataset) → journal / PROSPERO / OSF | duplicates (with dedup count), screenable metadata (title/abstract), full-text PDF present | `high` |
| `quick` *(default)* | "anything obviously broken" | citekey conflicts, broken attachments, duplicates | none |

Failures prevented are concrete: undefined/duplicate `\cite{}` keys, references rendered with blank
volume/pages/publisher, the same source cited twice, corrupted PRISMA counts, un-screenable records,
broken full-text PDFs that block data extraction.

### 2. Import references without making a mess

`import scan → resolve → apply` over an **editable manifest**. Sold as *reviewable ingest*, not
"Zotero's PDF metadata recognizer in the CLI" (there is no documented API for that recognizer).
Identifier resolution (DOI/PMID/arXiv/ISBN/URL), schema-valid item creation, duplicate/attach-candidate
matching, metadata-confidence scoring, then a previewed write. Attachment behavior is explicit (see
the attachment-mode contract below).

### 3. Give agents, vaults, and scripts a safe surface

One scope grammar, one freshness model, one mutation envelope, one finding envelope, one capability
registry — so an MCP host, vault, CI job, or shell script can discover what's safe, what's fresh,
what's writable, and what context is bounded. Not "86 tools plus more tools": **predictable contracts**.

## UX principles

1. **Local-first reads, preview-first writes.** Reads hit the synced SQLite mirror / local API;
   writes auto-route to the Web API and preview unless `--yes`.
2. **Provenance everywhere.** Because reads are local (maybe stale) and writes are cloud, every
   result carries source + freshness so a human or agent knows whether to trust it.
3. **One selection vocabulary.** A single `scope` consumed by every read, audit, export, enrich, write.
4. **Composable verbs, stable envelopes.** One mutation plan/result shape, one finding shape, one
   exit-code contract. Learn the grammar once.
5. **Schema-aware, never schema-guessing.** Validate against live `itemTypeFields` before writing.
6. **Declared preconditions, loud remediation — never silent degradation.** (See next section.)
7. **Honest about limits.** Surface Zotero gotchas (local-write rejection, `412` conflict, schema
   drift) as actionable guidance, not opaque errors.

## The precondition contract

A capability that needs the desktop app, a key, or a sync is **never dropped or silently emptied**.
It declares what it needs, checks up front, and when unmet returns a loud, machine-actionable signal
the agent can act on — alert the user, or launch Zotero itself, then retry.

**Precondition taxonomy:**

- `live_local_api` — Zotero desktop running + local API enabled: saved-search execution
  (`/searches/<key>/items`), on-disk attachment paths (`items file`), `--verify-files`, live reads.
- `web_api_key` — all writes; `schema new-item-template` (`/items/new` is Web-only).
- `synced_store` — `--data-source local` when the store is empty/stale.
- `better_bibtex` — citekey checks (keys live in the `extra` field).

**Three surfaces:**

1. **Proactive (declarative).** The capability registry declares `requires: [...]` per command;
   `agent-context`, `which`, and MCP tool descriptions carry it, so an agent knows *before calling*
   that e.g. `searches run` needs Zotero open. No surprise empties.
2. **Reactive (preflight refusal).** When a declared precondition is unmet, never return
   empty-success — return a structured envelope and a distinct exit code (`9`, setup required):

   ```json
   {
     "ok": false,
     "status": "precondition_unmet",
     "capability": "searches run",
     "precondition": "live_local_api",
     "detail": "Saved-search execution requires Zotero desktop running with the local API enabled.",
     "remediation": [
       {"action": "launch_zotero", "command": "zotio doctor --ensure-live --launch"},
       {"action": "instruct_user", "text": "Open Zotero, then Settings -> Advanced -> enable 'Allow other applications...'"}
     ],
     "retry_after_remediation": true
   }
   ```

3. **Agent-actionable launch.** `doctor --ensure-live [--launch]` (a.k.a. `zotero launch --wait`):
   cross-platform app launch (GOOS dispatch — generalizes `items open`'s macOS `open`) **+ poll the
   local API until it answers or times out.** This is what a host LLM invokes to "open Zotero itself."

**Composite-command rule — loud ≠ abort.** `library health` runs every offline check normally, and
for a live-only check (e.g. broken-attachment `--verify-files`) emits a **loud skipped-with-remedy
notice** in the report — never a silent omission, never a whole-command abort:

```
Skipped: broken_attachment_file — needs Zotero desktop (local API).
  Fix: zotio doctor --ensure-live --launch, then re-run.
```

This unifies three existing ad-hoc guards under one contract rather than inventing a mechanism:
`classifyAPIError`/`isLocalWriteRejection` (read-only write guard), the `searchesRunFallback`
shape in `searches run`, and `doctor`'s `writes:` reporting. Today `searches run` can't distinguish
"couldn't execute (Zotero closed)" from "ran, 0 hits" (`zoteroResultIsEmpty`) — the contract fixes
exactly that ambiguity.

## Contract specifications

**Scope grammar** (resolved once to ordered item keys + provenance):

```
collection:KEY      tag:NAME           query:TEXT
item:KEY            items:file.json    keys-from (existing bulk input)
saved-search:KEY    -> requires live_local_api; refuses loudly when Zotero closed
```

**Finding taxonomy** (stable across runs — keyed by `(kind, item_key)`/content hash, **not** a
run-local sequence, so agents/CI can diff):

```json
{
  "kind": "missing_doi", "severity": "high",
  "item_key": "ABCD1234", "title": "Example Paper",
  "evidence": {"field": "DOI", "current": ""},
  "source": {"kind": "local", "synced_at": "2026-06-27T02:55:00Z"},
  "autofixable": true,
  "recommended_action": {"command": "zotio items enrich --missing-doi --keys-from -"}
}
```

`recommended_action` points at the **existing dedicated fixer** (`items enrich`, `tags audit fix`,
`items duplicates resolve`) — not at a new aggregator (see decision D2).

**Exit codes** (existing `0/2/3/4/5/7/10` retained; additions slot in without collision):

```
0   success / gates passed        9   precondition / setup required (launch app, set key, sync)
2   usage error                   11  quality gate failed (--fail-on threshold crossed)
3   not found                     12  freshness gate failed (--require-fresh; remedy = sync + retry)
4   auth required                 (13 mutation-blocked: DEFERRED until a write consumer needs it)
5   upstream API error
7   rate limited
10  config error
```

**Mutation envelope** stays the shipped `internal/cli/mutate.go` shape (plan/result, gates,
`--dry-run`/`--yes`/`--max-changes`); extended with a `scope` block (`expr`, `selected`, `source`,
`synced_at`) and an optional `journal` pointer.

## Cut / merge / add / defer

| Decision | Item | Rationale |
|---|---|---|
| **Cut** | Slack/email/GitHub `--deliver` adapters | Scope creep. Keep `stdout`/`file`/`webhook`; pipe to `gh`/`mail`/Slack CLIs. |
| **Cut** | CLI-owned semantic/RAG search | Violates no-LLM-by-design (computing embeddings = calling a model); no Zotero semantic endpoint. Revisit only as ingestion of **host-provided** vectors. |
| **Cut** | Per-CSL-style citation checks | Would require embedding a citeproc engine for marginal value; the generic `--missing-citation` core-field check already catches what bites. Don't "defer" — decline. |
| **Cut** | My Publications, cross-platform deep-link *richness* | Niche / not core. (The launch+readiness *slice* of deep-links is kept — it serves the precondition contract.) |
| **Defer** | *Full* query-planner read parity | Build only the parity `scope`/`health`/`export` need. "Full parity" is an architecture goal, not a user outcome. |
| **Defer** | *Universal* undo | Journal every mutation; add targeted undo only where the inverse is reliable (tag rename, collection membership). |
| **Merge** | All audit flags, citekey/duplicate/tag/broken-attachment checks | Sub-checks of `library health`; keep the primitives as compatibility aliases. |
| **Merge** | `--fail-on` + audit exit codes + CI gating | One quality-gate contract under `library health` (exit `11`). |
| **Merge** | `scope.Spec` + freshness/provenance + query planner | One selection contract: *what set, how fresh, from where.* |
| **Merge** | Capability registry + `agent-context` + MCP resources + preconditions | One capability source of truth (read/write/destructive/data-source/auth/freshness/`requires`). |
| **Merge** | PDF ingest + identifier adapters + schema builders + bulk manifest | One `import plan -> resolve -> apply` journey. |
| **Merge** | Vault audit/repair into vault | `vault audit` = the safe preflight before `push/pull/sync`, not a separate pillar. |
| **Add** | Attachment-mode contract | `--attach-mode none\|linked-file\|upload`; each honest about portability/API limits. |
| **Add** | Freshness gates | `--require-fresh 24h` to stop stale local reads from driving high-risk writes. |
| **Add** | Precondition contract | Declared preconditions + loud remediation + launch primitive (this doc's central new principle). |
| **Add** *(validate first)* | `export snapshot` review lockfile | Reproducible review handoff (keys + versions + freshness + health gaps + export hash). Plausible but unvalidated; Phase 6, dampened — Zotero `version` ints bump on any edit, so a version-keyed lockfile reads "stale" easily. |

**Attachment-mode contract** (journey 2): start with `none` (metadata only) and `linked-file`
(create `linked_file` attachment metadata pointing at the on-disk path — lower risk, but doesn't sync,
no group libraries, no mobile). Gate `upload` (stored file via the multi-step Web API protocol:
create item → request authorization → POST to upload URL → register; plus quota/permission/conflict/
rate-limit failure modes) behind a later phase.

## Phase plan

```
P0  decisions only      ->  P1  library health MVP (flagship, read-only)
P2  trust contracts      ->  P3  safe remediation
P4  reviewable import    ->  P5  agent/vault trust plane
P6  reproducible export  ->  P7  packaging & niceties
```

- **Phase 0 — Decisions.** Record the cuts/defers above; no code beyond docs/help.
- **Phase 1 — `library health` MVP (flagship).** `library health [--scope] [--for] [--fail-on] [--json]`.
  Read-only, local-first, **composes existing primitives** (`items audit --missing-citation/--missing-pdf/
  --verify-files/--missing-abstract/--missing-doi/--missing-tags`, `items citekey-conflicts`,
  `items duplicates`, `tags audit`) into ranked findings with the finding taxonomy + the loud
  skipped-with-remedy notice for live-only checks. *Cheapest highest-value slice — the checks already exist.*
- **Phase 2 — Trust contracts (forced by P1).** Minimal `scope.Spec`; freshness gate `--require-fresh`;
  exit code `11`; **capability registry** (fills the nil `agent-context` discovery; owns `requires`
  preconditions); the **`doctor --ensure-live --launch` + readiness** primitive (exit code `9`).
- **Phase 3 — Safe remediation. SHIPPED.** Findings route to existing fixers (no new write
  engine): `library health` emits `remediation_plan` steps and `items enrich` accepts
  `--keys-from`, so missing DOI/abstract/PDF remediation is exact and preview-first. The shared
  write engine was promoted to the `internal/mutation` package (Options instead of `*rootFlags`;
  cli keeps a thin flags+render adapter). Every applied run is recorded to an append-only journal
  (`journal list`/`show`); `journal undo <run-id>` reverses the reversible (tag/collection
  membership) ops and loudly refuses the rest (merges, deletions, field overwrites, renames).
- **Phase 4 — Reviewable import. SHIPPED.** `import scan → resolve → apply` over an editable JSON
  manifest; DOI/PMID/arXiv/ISBN adapters (`import doi|pmid|arxiv|isbn`); schema-valid creation via
  Web `/items/new` (`items new`, refuses loudly offline); `import apply --attach-mode none|linked-file`
  (`upload` refused loudly, deferred); enrich gained Semantic Scholar/OpenCitations providers + a
  read-only `--validate` discrepancy mode (OpenAlex already shipped).
- **Phase 5 — Agent/vault trust plane. SHIPPED.** `zotero://freshness` (decision-ready sync ages),
  `zotero://health/{scope}` (ranked findings per scope), and bounded graph resources
  (`collections/{key}/tree`, `items/{key}/children|attachments|context`, depth/node-capped);
  `vault audit` read-only preflight (orphaned/stale/needs-boundary); guided MCP prompts
  (prepare-library-health, prepare-import, sync-vault-safely). `zotero://capabilities` shipped in Phase 2.
- **Phase 6 — Reproducible export. SHIPPED.** `export snapshot [library|collection:KEY|tag:NAME]`:
  truly paginated (start/limit across all pages), resumable (a checkpoint sidecar lets an
  interrupted run continue), streams structured item JSONL, and writes a `<output>.lock.json`
  lockfile recording each item's key+version + a sort-invariant content sha256 for reproducibility/
  drift detection (never the formatted-bibliography mode that ignores limit).
- **Phase 7 — Packaging & niceties. SHIPPED.** MCP installs honor env-selected library/profile
  (`ZOTERO_GROUP`/`ZOTERO_PROFILE` fall back into `--group`/`--profile` in the root pre-run, so every
  mirrored tool is scoped; the MCP server logs the active library/profile at startup); `groups inspect
  <id>` group-readiness preflight (accessible/readable/writable); `watch [resource...]` watch-mode
  incremental sync (polling, `--interval`/`--once`, SIGINT-graceful); `capabilities drift` probes core
  read endpoints for API drift.
- **Phase 8 — Read-your-writes. SHIPPED.** Surfaced during live validation: writes go to the cloud
  while reads default to the local mirror, so a write-then-read (re-audit after a fix, multi-step
  workflow, agent tool-then-resource) saw stale data until the next `sync`. Now an applied write is
  replayed into the mirror immediately (`--data-source local` reads-your-own-writes, no `sync`) and the
  post-write item state is returned in the mutation envelope (agents need no follow-up read). Best-effort
  for the writer's own changes; cross-client staleness stays handled by the freshness contract
  (`--require-fresh`, provenance). Creates and bulk/trash shapes reconcile on the next `sync`.

## Phase 9–11 (added 2026-07-09)

Derived from a full review of the open feature-opportunity ledger (78 findings triaged:
duplicates merged, already-shipped items closed) against the shipped Phase 0–8 surface.
Grounding rules and product thesis unchanged. The review's structural observation: nearly
every high-value opportunity extends an existing contract (health checks, import manifest,
scope grammar, journal, capability registry) rather than demanding a new subsystem — the
two exceptions (screening/PRISMA, translation-server capture) are gated on demand evidence.

### Phase 9 — Finish the loops already sold

Every item is a shipped contract whose last mile is missing; no new subsystems.

- **Scope-wide CSL bibliographies via the hybrid Web-API seam.** `items cite --style` today
  prints a stderr note and silently renders the default style — a live silent-degradation
  violation of UX principle 6. Route styled rendering through the Web API (`format=bib&style=`,
  server-side rendering; embedding citeproc stays cut per D5) and render whole scopes,
  producing the submission-ready artifact hero capability #1 stops short of.
- **Manuscript cite-check.** Validate a manuscript's actual `\cite{}`/`[@key]` usage against
  the library's citekeys: undefined/renamed keys, plus cited items missing citation-core
  fields. CI-gateable.
- **One finding envelope, consumable by `--keys-from`.** Diagnostics emit heterogeneous
  result shapes; `--keys-from` cannot parse the very report whose `recommended_action`
  points at it. Standardize diagnostics on the finding taxonomy and teach `--keys-from`
  to ingest it — diagnose → fix becomes a single pipe (the loop D2 promised).
- **`export snapshot verify`/`diff`.** The Phase 6 lockfile finally gets its consumer.
  Verification prefers the recorded content sha256 over Zotero version ints (edit-tolerant
  drift, resolving the Phase 6 dampening note).
- **Registry-driven preflight.** `Requires` preconditions are declared and exported but not
  enforced: the same unmet precondition is an empty-success on some commands and exit 9 on
  others. One central preflight (exit 9, `precondition_unmet` envelope) consumed by the CLI
  and MCP `command_run`, making the Phase 2 precondition contract real; the MCP facade gains
  per-command operation/requires/destructive annotations.
- **Deliver on gate failure.** `--deliver` drops the report exactly when a gate fails — the
  one moment a CI/agent consumer needs it. Outcome-aware delivery routing.
- **Rolling convention:** every command touched here adopts the shared scope grammar
  (`scope.Spec`) instead of growing bespoke selection flags.

### Phase 10 — Research-workflow depth (validated 2026-07-09; items 1–5 SHIPPED 2026-07-09)

Validated pre-build from dev and user perspectives; full evidence and reshaped contracts in
`notes/phase-10-validation.md`. Items 1–5 shipped as `items related`, `creators audit`,
`creators audit fix`, and `import discover` (backward/forward/both + manifest v2 provenance +
provider cache). Item 6 (PRISMA-input reporting) remains gated on an outreach demand signal.
Ordered by slice size:

1. **`items related`** — relations already persist in synced raw item JSON (unindexed, not
   "discarded"); MVP is a URI parser + `json_each` read, no schema migration. Outgoing +
   capped incoming edges, external targets preserved; extends the Phase-5 MCP graph resources.
2. **`creators audit`** (read-only) with confidence tiers — exact-normalization variants /
   initial-vs-full variants / ambiguous surnames — plus ORCID *capture* from provider payloads
   into a local sidecar as corroboration evidence. ORCID persistence to Zotero is off the
   table: Zotero has no creator ORCID field (the "nearly free" claim held only for parsing).
3. **`creators audit fix`** — tier 1 auto-fixable, tier 2 only via explicit mappings;
   journaled but not undoable (documented); never decides the style-correct form of a name.
4. **Discovery producer, backward chase** — extract from `collections gaps` (COCI +
   Semantic Scholar already implemented there), emit a provenance-extended import manifest,
   dedupe against the library (DOI + normalized title) BEFORE emitting, wire `internal/cache`
   into the provider path with it. `collections gaps` becomes a consumer.
5. **Forward chase** — new work (OpenAlex `cites:` needs work IDs we currently drop; COCI v2
   `/citations`); bounded, truncation-visible.
6. **PRISMA-input reporting** — identification-stage counts (identified / duplicates removed /
   after dedupe) from existing dedupe machinery, feeding PRISMA 2020 flow diagrams. Gated on
   a demand signal from the SR Toolbox / evidence-synthesis outreach. Screening itself is
   explicitly out: Rayyan/ASReview own it, and it is not a no-LLM strength. The wedge is
   "arrive at the screening tool with a certified, deduped, counted corpus."

Prerequisite fix, independent of Phase 10: `items enrich` Extra clobber (P1, destroys Better
BibTeX citation keys on apply — found during validation).

(`items retract-check`, originally slotted here, shipped ahead of plan.)

### Phase 11 — Automation substrate (strict order)

Transactional `workflow run` (one plan, one approval, one journal run-id, resume) → workflow
expressiveness (inter-step data-flow, variables, conditionals) → event triggers (`tail`/`watch`
bridge) + MCP inline workflow submission. Rationale: triggers and agent-submitted execution
multiply whatever safety model the runner has — make the envelope transactional before making
it expressive, expressive before wiring triggers. Triggered runs stay preview-only unless `--yes`.

### Cut / defer (2026-07-09)

| Decision | Item | Rationale |
|---|---|---|
| **Defer** | Journal before-images / broader undo | Relitigates the recorded "defer universal undo"; no demand evidence. Before-images are the principled path *if* demand shows. |
| **Defer** | Stored-file upload protocol | Still the riskiest import edge; the attach-mode contract already reserves `upload`. Revisit on group-library demand. |
| **Decline** | Multi-library workspace model | `--group all` read/diagnostic fan-out is the cheap 80%; a workspace registry is architecture without a user. |
| **Decline** | Beacon recurring scheduler | `watch --interval` + OS schedulers/CI cover it; a resident daemon is a large operational surface against the composability thesis. |
| **Park** | BYO-vector seam | The sanctioned form of the semantic-search cut, but build nothing until a host actually shows up with vectors. |
| **Park** | P3 long tail (analytics, digests, ZotFile-style renaming, notes, collaboration) | Promote individual items only on user signal from the outreach channels. |

### Competitive scan — MCP-registry Zotero servers (2026-07-11)

Five registry servers never studied as prior art were probed at source level:
`AlejandroArnaud/mcp-for-zotero` (hosted Web-API proxy SaaS, no public source),
`Combjellyshen/ZoteroBridge` (direct-SQLite read/write, TypeScript),
`RaulSimpetru/zotero-library-mcp` (pyzotero identifier ingest, Python),
`introfini/mcp-server-zotero-dev` (plugin-dev RDP control plane, TypeScript),
`piiinpiiins/zotero-mcp-local` (SQLite temp-copy reads + local similarity, Python).
Nearly their whole combined surface is already covered (identifier ingest incl. batch,
annotations, fulltext, collections/tags/notes, bibliography export, groups, identifier
lookup). The genuine deltas and their dispositions:

| Decision | Item | Rationale |
|---|---|---|
| **Build — SHIPPED 2026-07-11** | `items similar` — explainable local relatedness | zotero-mcp-local ranks library items by composite similarity over shared collections/tags/creators/venue plus fulltext rare-word overlap. Shipped: Jaccard on collections/tags/creators (0.30/0.25/0.10), exact-match venue (0.10), rare-term overlap over synced fulltext rows (0.25; DF-based, two-pass, memory-bounded). Deterministic, no-LLM, no network — does **not** relitigate the semantic-search cut (no embeddings). Every signal lives in zotio's mirror via typed store reads (ADR-0002). Complements shipped `items related` (explicit edges) with *discovered* similarity, with human-readable "why" reasons per hit. |
| **Build (bounded) — SHIPPED 2026-07-11** | Close the OA-PDF loop: download, not just link | zotero-library-mcp downloads Unpaywall/arXiv PDFs (size cap, content-type/PDF-header validation) and attaches the file; zotio's `items enrich --missing-pdf` only attached a `linked_url`. Shipped as `items enrich --attach-mode linked-url\|linked-file` (default linked-url, unchanged): linked-file downloads to `--pdf-dir` (content-type + `%PDF-` magic validation, 100 MiB streaming cap, non-public destinations rejected at dial time, no clobber) and creates a `linked_file` child via the import-apply seam. Downloads happen at apply time only; preview names mode and destination. A `stored` mode was built then removed in review: the desktop Connector can only parent attachments to same-session connector-created items, so stored retro-attach waits on the Web API stored-file *upload protocol*, which stays deferred. |
| **Decline** | Tag colors (`/settings/tagColors` PUT) | Cosmetic desktop-UI state, not trust/automation. Cheap (one settings endpoint) but no thesis fit; revisit only on user demand. |
| **Decline** | Programmatic annotation *creation* | zotero-library-mcp computes highlight rects via PyMuPDF and writes Zotero annotation items. Requires a PDF geometry engine in Go — heavy dependency for a write-risky niche. Reading annotations (shipped) is the trust surface; authoring them is not. |
| **Decline** | In-CLI PDF byte-level text extraction | zotio serves Zotero's own fulltext index (`sync --fulltext`, `items fulltext`); `items file` + `pdftotext` composes the fallback. A PDF parser dependency is not justified (composability thesis). |
| **Decline** | Direct `zotero.sqlite` access mode | ADR-0002 stands, now with evidence: ZoteroBridge ships default-writable whole-file `writeFileSync` overwrites of a live `zotero.sqlite` with no WAL handling — corruption-grade anti-prior-art. zotero-mcp-local's temp-copy read is the safe variant of what zotio's mirror already provides, minus sync provenance. |
| **Decline** | Hosted/remote MCP + OAuth token surface | mcp-for-zotero is a SaaS credential-custody product. zotio is local-first; the MCPB manifest covers desktop distribution. Different product, not a feature gap. |
| **Decline** | Plugin-dev tooling (RDP bridge, UI automation, log tailing) | mcp-server-zotero-dev is "DevTools for Zotero plugin authors" — zero overlap with library management. Prior art only if zotio ever wants a desktop QA harness. |

## Opportunity → phase mapping

All 26 open feature opportunities as of 2026-06-27:

| Title | Disposition |
|---|---|
| Capability/routing registry | **Phase 2** — owns preconditions; fills nil discovery |
| Query-planner / local read parity | **Phase 2** minimal; *full parity deferred* |
| Unified `mutation.Plan` package | **Phase 3** — promote at rule-of-three |
| MCP honors profiles/groups/config | **Phase 7** |
| `--fail-on` audit exit codes | **Phase 1/2** — exit `11` under `library health` |
| Systematic-review health + citation audit | **Largely SHIPPED** (`--missing-citation`, `--verify-files`); absorbed into `library health`; style-specific **cut** → close |
| Vault audit/repair preflight | **Phase 5** |
| Bounded MCP graph resources | **Phase 5** |
| Freshness/provenance to agents | **Phase 2** |
| Reusable `scope.Spec` | **Phase 2** |
| PDF→item ingest | **Phase 4** — write side of `import scan`, attach-mode |
| PMID/arXiv/ISBN adapters | **Phase 4** |
| Bulk-import review + apply manifest | **Phase 4** |
| Schema-backed `items new` builders | **Phase 4** |
| Enrich providers + `--validate` | **Phase 4** — partial (OpenAlex shipped); add Semantic Scholar/OpenCitations |
| Paginated/resumable export | **Phase 6** — folds into `export snapshot` |
| Watch-mode sync | **Phase 7** |
| `--deliver` adapter registry | **CUT** |
| API capability-drift detection | **Phase 7** |
| MCP path-param URL-encoding | **Upstream** (generated `makeAPIHandler`); out of scope here |
| MCP-guided cleanup workflows | **Phase 5** |
| Generator extension layer | **Upstream** (cli-printing-press) |
| Mutation run-journal + undo | **Phase 3** — journal yes; undo only where reversible |
| Group readiness preflight | **Phase 7** |
| Local semantic/RAG search | **CUT** — revisit only as host-provided vectors |
| Cross-platform desktop deep links | **Phase 2/3** (launch+readiness slice); *richness deferred* |

## Where we diverged from the oracle (recorded decisions)

The Oracle (GPT-5.5 Pro) review was adopted for its diagnosis (flagship `library health`, finding
taxonomy, freshness gates, attachment-mode honesty, the cuts). We **rejected five over-built remedies**:

- **D1 — `--for`, not `--profile`.** The oracle proposed overloading `--profile` for "intent," but
  `--profile` already means *a user-saved bundle of flags*. Built-in intent presets would collide with
  user profiles of the same name and conflate two concepts. `library health --for citation|systematic-review|quick`
  is a dedicated, tool-curated selector; `--profile` stays user flag-bundles.
- **D2 — No `library health fix` write engine.** The oracle's own finding schema pointed
  `recommended_action` back at a new `health fix` aggregator, which would duplicate and compete with the
  shipped, tested mutators (`items enrich`, `tags audit fix`, `items duplicates resolve`). **Health
  diagnoses; existing commands treat.** `recommended_action` points at the real fixer. A one-shot
  orchestrator, if ever added, must strictly delegate — never reimplement writes.
- **D3 — No exit-code proliferation.** Adopt `11` (quality gate) and `9` (precondition/setup) — the
  latter earns its slot because the agent's action is categorically different (provision the
  environment). Defer the oracle's `12` (freshness) and `13` (mutation-blocked); the JSON `.gate`
  reason suffices until a CI consumer must branch on them.
- **D4 — Keep `saved-search:` scope (precondition, not exclusion).** Initially proposed excluding it
  because execution needs live Zotero; **retracted** — excluding a capability because the app might be
  closed is itself a silent failure. It stays, declares `live_local_api`, and refuses loudly with
  remediation. This pushback produced the precondition contract above.
- **D5 — Decline per-CSL-style checks (don't defer).** The oracle would park them "unless backed by a
  citeproc engine." Embedding citeproc is a heavy dependency for marginal value; generic core-field
  checks already cover the real failures. Cut the ambition outright so `dxut` can close.

Minor: finding IDs are content/identity-keyed (not run-local sequences); `export snapshot` is treated
as plausible-but-unvalidated (Phase 6, validate demand first).

## Build first

`zotio library health` — read-only, scoped, ranked findings, `--for` presets, `--fail-on` —
then route findings to the existing fixers. **Not** file upload (the riskiest import edge cases).
Every audience gets value on day one: researcher "is this collection ready (for what)?", PKM "will
vault sync be trustworthy?", agent "what's safe to act on?", CI "gate on health." The Phase-1 checks
already exist as primitives, so the flagship is mostly composition + the finding/precondition envelopes.

## References

Zotero docs the feasibility calls rest on (see also `docs/zotero-api-coverage.md`):

- Local API (GET-only, no auth, local file paths): https://www.zotero.org/support/dev/web_api/v3/local_api
- Write requests (versions/preconditions, `412`): https://www.zotero.org/support/dev/web_api/v3/write_requests
- File uploads (multi-step stored-file protocol): https://www.zotero.org/support/dev/web_api/v3/file_upload
- Attaching files (linked vs stored, sync/mobile limits): https://www.zotero.org/support/attaching_files
- Item types & fields (`/items/new`): https://www.zotero.org/support/dev/web_api/v3/types_and_fields
- Web API basics (pagination, export formats): https://www.zotero.org/support/dev/web_api/v3/basics
