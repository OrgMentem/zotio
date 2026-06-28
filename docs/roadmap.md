# zotero-pp-cli — Product Roadmap

## Provenance & status

- **Date:** 2026-06-27 · **Branch:** `reprint-4.25.0` (HEAD `97775bb`).
- **How this was derived:** reviewed the 26 open Glean feature-opportunity beads → drafted a
  value-first PRD → had it improved by a second model (GPT-5.5 Pro via Oracle, session
  `zotero-roadmap-pdr-4`; reattach: `oracle session zotero-roadmap-pdr-4 --render`) → reconciled
  every recommendation against the live codebase and `docs/zotero-api-coverage.md`.
- **Intent:** maximize end-user value and UX, **not** match the issue tracker. This document is
  the source of truth for sequencing; the beads are raw material, not a checklist.
- **Grounding rule:** every item here is feasible against Zotero's *actual* API. The hard
  constraints (local API is GET-only; writes route to the Web API with a key; stale local reads
  vs. cloud writes race to `412`; schema endpoints are global; `/items/new` is Web-only; fast
  6–10 week release cadence) live in `docs/zotero-api-coverage.md` and are assumed throughout.

## Product thesis

`zotero-pp-cli` is **the trust-and-automation layer for Zotero**: local-fast reads for searching
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
       {"action": "launch_zotero", "command": "zotero-pp-cli doctor --ensure-live --launch"},
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
  Fix: zotero-pp-cli doctor --ensure-live --launch, then re-run.
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
  "recommended_action": {"command": "zotero-pp-cli items enrich --missing-doi --keys-from -"}
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
- **Phase 4 — Reviewable import.** `import scan → resolve → apply` over an editable manifest;
  DOI/PMID/arXiv/ISBN adapters; schema-valid creation via Web `/items/new` (refuses loudly offline);
  `--attach-mode none|linked-file` now, `upload` deferred; enrich providers (Semantic Scholar /
  OpenCitations — OpenAlex already shipped) + `--validate` discrepancy mode.
- **Phase 5 — Agent/vault trust plane.** `zotero://capabilities`, `zotero://freshness`,
  `zotero://health/{scope}`, bounded graph resources (tree/children/attachments/context with limits);
  `vault audit` preflight; guided MCP prompts (prepare-library-health, prepare-import, sync-vault-safely).
- **Phase 6 — Reproducible export.** `export snapshot` lockfile (structured formats, not formatted
  bibliography which ignores `limit`); resumable pagination. *(Validate demand during Phase 1–5.)*
- **Phase 7 — Packaging & niceties.** MCP install honoring profiles/groups/base-url; group readiness
  preflight; watch-mode sync (polling/incremental, not push); capability-drift detection.

## Bead → phase mapping

All 26 open beads as of 2026-06-27:

| Bead | Title | Disposition |
|---|---|---|
| `cdd5b64b` (P1) | Capability/routing registry | **Phase 2** — owns preconditions; fills nil discovery |
| `2465cdde` (P1) | Query-planner / local read parity | **Phase 2** minimal; *full parity deferred* |
| `58a31bf6` (P1) | Unified `mutation.Plan` package | **Phase 3** — promote at rule-of-three |
| `3df91067` (P1) | MCP honors profiles/groups/config | **Phase 7** |
| `3f0b8763` (P2) | `--fail-on` audit exit codes | **Phase 1/2** — exit `11` under `library health` |
| `dxut` (P2) | Systematic-review health + citation audit | **Largely SHIPPED** (`--missing-citation`, `--verify-files`); absorbed into `library health`; style-specific **cut** → close |
| `725cb43f` (P2) | Vault audit/repair preflight | **Phase 5** |
| `04f41aa8` (P2) | Bounded MCP graph resources | **Phase 5** |
| `943783579` (P2) | Freshness/provenance to agents | **Phase 2** |
| `556c94b6` (P2) | Reusable `scope.Spec` | **Phase 2** |
| `q1ia` (P2) | PDF→item ingest | **Phase 4** — write side of `import scan`, attach-mode |
| `37849fc0` (P2) | PMID/arXiv/ISBN adapters | **Phase 4** |
| `7e799ea9` (P2) | Bulk-import review + apply manifest | **Phase 4** |
| `748370c5` (P2) | Schema-backed `items new` builders | **Phase 4** |
| `mmmd` (P2) | Enrich providers + `--validate` | **Phase 4** — partial (OpenAlex shipped); add Semantic Scholar/OpenCitations |
| `d27f99d4` (P2) | Paginated/resumable export | **Phase 6** — folds into `export snapshot` |
| `0cabee79` (P2) | Watch-mode sync | **Phase 7** |
| `860e00b7` (P2) | `--deliver` adapter registry | **CUT** |
| `d130ba9e` (P2) | API capability-drift detection | **Phase 7** |
| `1b05b22` (P2, security) | MCP path-param URL-encoding | **Upstream** (generated `makeAPIHandler`); out of scope here |
| `a3d0987d` (P2) | MCP-guided cleanup workflows | **Phase 5** |
| `awc7` (P2) | Generator extension layer | **Upstream** (cli-printing-press) |
| `a05b9f86` (P3) | Mutation run-journal + undo | **Phase 3** — journal yes; undo only where reversible |
| `9200a793` (P3) | Group readiness preflight | **Phase 7** |
| `rlvp` (P3) | Local semantic/RAG search | **CUT** — revisit only as host-provided vectors |
| `8r0o` (P3) | Cross-platform desktop deep links | **Phase 2/3** (launch+readiness slice); *richness deferred* |

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

`zotero-pp-cli library health` — read-only, scoped, ranked findings, `--for` presets, `--fail-on` —
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
