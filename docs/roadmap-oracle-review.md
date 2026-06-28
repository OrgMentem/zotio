# Revised PRD: `zotero-pp-cli`

## Product thesis

`zotero-pp-cli` is **the trust-and-automation layer for Zotero**: local-fast reads for searching and auditing, preview-first Web API writes for safe change, and bounded context surfaces for humans, scripts, and MCP agents.

The product should not be “every Zotero endpoint in the terminal.” It should be the tool researchers use when the GUI becomes too manual: **prove a library is ready, fix it safely, ingest material with review, and give agents/vaults trustworthy context.**

Grounding constraints: Zotero’s local API is fast/offline but currently `GET`-only; writes must route to the Web API with a key, and stale local reads can cause `412` conflicts after cloud writes. Schema endpoints are global, and `/items/new` is not available from the local API. The attached coverage doc also notes Zotero’s fast 6–10 week release rhythm, so schema drift and capability freshness need to be product features, not maintenance trivia.  Official docs match the key local-API constraint: local API accepts only `GET`, has no auth, exposes local-only file-path endpoints, and differs slightly from Web API search behavior. ([zotero.org][1])

---

## The 3 hero capabilities

### 1. **Make my Zotero library review- and submission-ready**

Outcome: a researcher can run one command and know whether a collection/library is safe to export, cite, share, or hand to an agent.

This is the north star. It absorbs `items audit`, `items missing-pdf`, `items citekey-conflicts`, duplicates, tag drift, broken attachment checks, and citation-readiness into one ranked health report with a remediation plan.

Primary UX:

```bash
zotero-pp-cli library health --scope collection:ABCD1234 --profile systematic-review
zotero-pp-cli library health --scope collection:ABCD1234 --fail-on high --json
zotero-pp-cli library health fix --scope collection:ABCD1234 --only missing_doi,tag_drift --dry-run
zotero-pp-cli library health fix --scope collection:ABCD1234 --only missing_doi --yes
```

This should be the product’s flagship because it delivers visible value without requiring new Zotero endpoints. Most checks are local-read/local-store work; fixes reuse existing preview-first Web API mutations.

### 2. **Import PDFs and external references without making a mess**

Outcome: a researcher can point the CLI at a folder or identifier list and get a reviewable manifest: new items, duplicates, attach-candidates, unresolved files, metadata confidence, and exactly what will be written.

Do **not** sell this as “Zotero Retrieve Metadata for PDF in the CLI.” There is no documented Zotero API for the desktop recognizer. Sell it as **reviewable ingest**: DOI/PMID/arXiv/ISBN/URL/ref manifest → metadata resolution → schema-valid item creation → optional attachment behavior.

Primary UX:

```bash
zotero-pp-cli import scan ~/Downloads/papers --emit import.plan.json
zotero-pp-cli import resolve import.plan.json --providers doi,pmid,arxiv,isbn --json
zotero-pp-cli import apply import.plan.json --attach-mode linked-file --dry-run
zotero-pp-cli import apply import.plan.json --attach-mode upload --yes
```

Attachment modes must be explicit:

```text
--attach-mode none         create/update metadata only
--attach-mode linked-file  create linked_file attachment metadata pointing to local path
--attach-mode upload       upload stored file via Zotero Web API file protocol
```

Start with `none` and `linked-file`; gate `upload` behind a later phase. Zotero’s Web API supports attachment item creation and file upload, but stored-file upload is a multi-step protocol: create an attachment item, request upload authorization, POST the file to the returned upload URL, then register the upload. It also has quota, permission, conflict, and rate-limit failure modes. ([zotero.org][2]) Linked files are lower implementation risk but lower portability: Zotero stores a path, linked files do not sync through Zotero, are not usable in group libraries, and are not supported on Zotero mobile apps. ([zotero.org][3])

### 3. **Let agents, vaults, and scripts use my library safely**

Outcome: an MCP host, Obsidian/Logseq vault, CI job, or shell script can discover what is safe, what is fresh, what is writable, and what context is bounded enough to consume.

This merges agent readiness, provenance, scope, bounded MCP resources, vault audit, and reproducible exports. It should not become “86 tools plus more tools.” The UX win is **predictable contracts**: one scope grammar, one freshness model, one mutation envelope, one finding envelope, one capability registry.

Primary UX:

```bash
zotero-pp-cli scope preview collection:ABCD1234 --json
zotero-pp-cli library health --scope saved-search:REVIEW_READY --require-fresh 24h
zotero-pp-cli vault audit --scope tag:to-read
zotero-pp-cli export snapshot --scope collection:ABCD1234 --format csljson --manifest review.lock.json
```

MCP resources should expose bounded, decision-ready context: `zotero://freshness`, `zotero://capabilities`, `zotero://health/{scope}`, `zotero://items/{key}/context`, and collection trees with limits. The CLI itself remains no-LLM-by-design; `items summarize` should continue producing context bundles, not summaries.

---

## Cut / merge / add

| Decision        | Item                                                                               | Rationale                                                                                                                                                               |
| --------------- | ---------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Cut**         | Slack/email/GitHub `--deliver` adapters                                            | Scope creep. Keep `stdout`, `file`, and `webhook`; users can pipe to `gh`, `mail`, Slack CLIs, or automation platforms.                                                 |
| **Cut**         | CLI-owned semantic/RAG search                                                      | Violates no-LLM-by-design if the CLI computes embeddings. Zotero API does not provide semantic search. Keep FTS/fulltext and revisit only as external-vector ingestion. |
| **Cut for now** | Cross-platform desktop deep links                                                  | Nice UX, not core value. Existing item/file open paths cover enough.                                                                                                    |
| **Cut**         | My Publications support                                                            | Coverage doc marks it as a niche gap; it does not serve the three hero outcomes.                                                                                        |
| **Defer**       | Full query-planner parity as its own project                                       | Build only the parity needed by `scope`, `health`, export, and import. “Full parity” is an architecture goal, not a user outcome.                                       |
| **Defer**       | Universal undo                                                                     | Journal every mutation first. Add targeted undo only where the inverse is reliable, such as tag rename or collection membership changes.                                |
| **Merge**       | Audit flags, citekey checks, duplicate checks, tag drift, broken attachment checks | Make them sub-checks of `library health`, with old commands retained as compatibility aliases.                                                                          |
| **Merge**       | `--fail-on`, audit exit codes, CI health checks                                    | One “quality gate” contract under `library health`.                                                                                                                     |
| **Merge**       | `scope.Spec`, freshness/provenance, query planner                                  | Users experience these as one selection contract: “what set did you operate on, how fresh is it, and where did it come from?”                                           |
| **Merge**       | Capability registry + `agent-context` + MCP resources                              | One capability source of truth: read/write/destructive/local/web/schema/external/freshness requirements.                                                                |
| **Merge**       | PDF ingest, identifier adapters, schema-backed builders, bulk manifest             | One `import plan → resolve → apply` journey.                                                                                                                            |
| **Merge**       | Vault audit/repair preflight with vault sync                                       | `vault audit` should be the safe preflight before `vault push/pull/sync`, not a separate product pillar.                                                                |
| **Add**         | `export snapshot` / review lockfile                                                | Missing high-value journey: reproducible review handoff with item keys, versions, freshness, export hash, and known health gaps.                                        |
| **Add**         | Attachment-mode contract                                                           | Make file ingest honest: `none`, `linked-file`, `upload`; each with portability and API limitations.                                                                    |
| **Add**         | Freshness gates                                                                    | `--require-fresh 24h` or `--max-staleness 24h` should prevent stale local reads from driving high-risk writes.                                                          |
| **Add**         | Finding taxonomy                                                                   | Every audit finding needs `severity`, `kind`, `evidence`, `autofixable`, `recommended_action`, and `source`.                                                            |
| **Add**         | Import manifest as a first-class artifact                                          | Users should be able to edit/review the plan before apply; agents should be able to pass it around safely.                                                              |

---

## Re-sequenced phase plan

### Phase 0 — Product cleanup and explicit deferrals

Ship no code unless needed for docs/help.

Decisions: semantic/RAG is deferred unless it consumes host-provided vectors; `--deliver` adapters beyond existing sinks are cut; “PDF ingest” is renamed to “reviewable import” until stored-file upload exists; security-generator work remains upstream.

Reasoning: this prevents the roadmap from promising value the API or product philosophy cannot deliver.

---

### Phase 1 — `library health` MVP: the flagship read-only outcome

Build:

```bash
zotero-pp-cli library health
zotero-pp-cli library health --scope collection:KEY
zotero-pp-cli library health --profile systematic-review
zotero-pp-cli library health --fail-on high
```

Checks to include first:

```text
missing_pdf
missing_doi
missing_abstract
missing_citation_core
duplicate_candidates
citekey_conflicts
tag_drift
broken_attachment_file
stale_local_data
schema_drift_warning
```

Output should rank by actionability and consequence, not by internal command source. Example sections:

```text
Health: needs attention
Scope: collection:ABCD1234 · 184 items · local sync 18m ago

Critical
  3 broken PDF attachments
  2 duplicate citekeys

High
  17 citeable items missing required core citation fields
  21 journal articles missing DOI

Suggested next steps
  zotero-pp-cli library health fix --only missing_doi --scope collection:ABCD1234 --dry-run
  zotero-pp-cli tags audit fix --scope collection:ABCD1234 --dry-run
```

Reasoning: this creates the product’s strongest “aha” without needing new Web API write surface. Broken attachment checks are feasible when Zotero desktop/local API is available because the local API exposes file path endpoints; they should degrade gracefully when desktop is closed. ([zotero.org][1])

---

### Phase 2 — Scope + freshness + exit-code contracts

Build the trust substrate only where it improves Phase 1.

Commands/flags:

```bash
zotero-pp-cli scope preview collection:KEY --json
zotero-pp-cli scope preview tag:"to-read" --sort dateAdded --limit 50
zotero-pp-cli library health --scope saved-search:KEY --require-fresh 24h
zotero-pp-cli library health --fail-on high
```

Scope grammar:

```text
collection:KEY
tag:NAME
saved-search:KEY
query:TEXT
item:KEY
items:file.json
file:/path/to/import.plan.json
```

Exit-code contract:

```text
0   success / gates passed
2   usage error
3   not found
4   auth required
5   upstream API error
7   rate limited
10  config error
11  quality gate failed (--fail-on)
12  freshness gate failed (--require-fresh / --max-staleness)
13  mutation gate blocked (--max-changes / destructive opt-in)
```

Reasoning: scope and freshness are product features because local reads plus cloud writes are inherently racy. Zotero write requests require versions/preconditions, and stale versions can produce `412 Precondition Failed`; the attached doc calls out exactly this race in the hybrid read/write model.  The Web API write docs also require current item/library versions for updates/deletes and reject stale writes with `412`. ([zotero.org][4])

---

### Phase 3 — Safe remediation as plans, not magic fixes

Build:

```bash
zotero-pp-cli library health fix --only missing_doi --scope collection:KEY --dry-run
zotero-pp-cli library health fix --only tag_drift --scope collection:KEY --dry-run
zotero-pp-cli library health fix --plan health.plan.json --yes
```

Use the existing mutation envelope, but promote it from `internal/cli/mutate.go` into a reusable internal package only when multiple commands consume it. Do not make “unified mutation package” a standalone milestone.

Mutation envelope refinements:

```json
{
  "schema_version": 1,
  "ok": true,
  "operation": "library.health.fix",
  "mode": "preview",
  "scope": {
    "expr": "collection:ABCD1234",
    "selected": 184,
    "source": "local",
    "synced_at": "2026-06-27T02:14:00Z"
  },
  "plan": {
    "summary": {
      "selected": 184,
      "planned": 21,
      "no_op": 163,
      "destructive": 0
    },
    "operations": []
  },
  "journal": null
}
```

Reasoning: this gives users a single path from finding to fix while preserving preview-first writes. Zotero writes require Web API write access; local writes are not supported. 

---

### Phase 4 — Reviewable import, metadata first; file upload second

Build:

```bash
zotero-pp-cli import scan ~/PDFs --emit import.plan.json
zotero-pp-cli import resolve import.plan.json --providers doi,pmid,arxiv,isbn
zotero-pp-cli import apply import.plan.json --attach-mode none --dry-run
zotero-pp-cli import apply import.plan.json --attach-mode linked-file --yes
```

Phase 4A should support:

```text
identifier imports: DOI, URL, PMID, arXiv, ISBN
schema-valid item creation
duplicate detection by DOI/title/year
attach-candidate matching
metadata confidence
manual manifest edit/review
linked_file attachment creation for personal local workflows
```

Phase 4B should add:

```text
--attach-mode upload
upload quota and permission preflight
md5/mtime computation
upload authorization
S3/form upload
upload registration
retry/resume rules
```

Reasoning: `/items/new` templates are the right way to create schema-valid payloads, but the local API does not implement `/items/new`, so builders must call the global Web API/schema path and fail clearly offline or without credentials.  Zotero’s type/field docs recommend `/items/new` for clients preparing writes. ([zotero.org][5]) Stored-file upload is feasible but should not be first because it introduces a separate upload protocol and additional user-visible failures. ([zotero.org][2])

---

### Phase 5 — Agent/vault trust plane

Build:

```bash
zotero-pp-cli capabilities --json
zotero-pp-cli freshness --json
zotero-pp-cli vault audit --scope collection:KEY
```

MCP resources:

```text
zotero://capabilities
zotero://freshness
zotero://health/{scope}
zotero://collections/{key}/tree?limit=N
zotero://items/{key}/context?max_bytes=N
```

MCP prompts:

```text
prepare-library-health
fix-library-health
prepare-import
prepare-citation-export
sync-vault-safely
```

Reasoning: agents should not infer safety from command names. They need a declared capability matrix: read/write/destructive, data source, auth required, freshness required, max-change defaults, and whether the command can run in group libraries.

---

### Phase 6 — Reproducible export and data-infra polish

Build:

```bash
zotero-pp-cli export snapshot --scope collection:KEY --format csljson --manifest review.lock.json
zotero-pp-cli export snapshot --scope saved-search:KEY --include health,versions,files
zotero-pp-cli export resume review.lock.json
```

Snapshot manifest:

```json
{
  "schema_version": 1,
  "scope": "collection:ABCD1234",
  "created_at": "2026-06-27T03:10:00Z",
  "source": "local",
  "synced_at": "2026-06-27T02:55:00Z",
  "items": [
    {"key": "ITEM1234", "version": 87, "title": "...", "hash": "..."}
  ],
  "health": {
    "critical": 0,
    "high": 2
  },
  "exports": [
    {"format": "csljson", "sha256": "..."}
  ]
}
```

Reasoning: this is the high-value journey missing from the draft. Systematic-review users need a reproducible, auditable handoff: exact item set, versions, known gaps, and export hash. Zotero’s API has versions and export formats, but the GUI does not package them as a review lockfile. The Web API supports item export formats, but formatted bibliography output has pagination caveats, so snapshot export should prefer item-level structured formats for reproducibility. ([zotero.org][6])

---

### Phase 7 — Packaging, groups, watch mode, and niceties

Build only after the hero workflows work end to end:

```text
MCP install honoring profiles/groups/base-url
group readiness and permission preflight
watch-mode sync
capability drift detection
desktop deep links
```

Reasoning: these improve adoption and polish, but they do not define the product.

---

## UX and contract refinements

### Rename the flagship surface

Keep old commands, but document them as primitives:

```text
items audit                 primitive
tags audit                  primitive
items citekey-conflicts     primitive
items duplicates            primitive
library health              flagship composed workflow
```

### Use profiles for intent, not just config

Health profiles:

```text
--profile quick
--profile citation-export
--profile systematic-review
--profile vault
--profile agent
```

Example:

```bash
zotero-pp-cli library health --profile citation-export --scope collection:THESIS
```

Profile behavior should be transparent in JSON:

```json
"profile": {
  "name": "citation-export",
  "checks": ["missing_citation_core", "citekey_conflicts", "duplicates", "broken_attachment_file"],
  "fail_on": "high"
}
```

### Standardize findings

Every audit finding should look like this:

```json
{
  "id": "finding_000123",
  "kind": "missing_doi",
  "severity": "high",
  "item_key": "ABCD1234",
  "title": "Example Paper",
  "evidence": {
    "field": "DOI",
    "current": ""
  },
  "source": {
    "kind": "local",
    "synced_at": "2026-06-27T02:55:00Z"
  },
  "autofixable": true,
  "recommended_action": {
    "command": "zotero-pp-cli library health fix --only missing_doi --keys-from ..."
  }
}
```

### Keep `items summarize` honest

Rename in docs, not necessarily command name:

```text
items summarize = build context bundle
```

Output should include:

```json
{
  "summary_generated": false,
  "requires_host_llm": true,
  "bundle": {}
}
```

This prevents agents and users from thinking the CLI calls an LLM.

### Make import manifests editable

`import scan` should emit a stable plan:

```json
{
  "schema_version": 1,
  "files": [
    {
      "path": "/Users/me/Papers/example.pdf",
      "status": "attach_candidate",
      "doi": "10.xxxx/yyyy",
      "target_item_key": "ABCD1234",
      "proposed_action": "attach_linked_file",
      "confidence": 0.92,
      "user_decision": "accept"
    }
  ]
}
```

`import apply` should refuse ambiguous entries unless `user_decision` is set or `--accept-confidence >= N` is provided.

---

## Risky assumptions and Zotero feasibility gotchas

| Item                              |          Feasibility | Gotcha / product constraint                                                                                                                                                                                                                                  |
| --------------------------------- | -------------------: | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `library health` read-only checks |                 High | Must surface local freshness. Local API is fast/offline but `GET`-only and can differ slightly from Web API quicksearch. ([zotero.org][1])                                                                                                                   |
| Broken attachment check           |          Medium-high | Feasible only when Zotero desktop/local API can resolve local file URLs. It should report `unavailable` rather than `broken` when desktop is closed or local API disabled.                                                                                   |
| Citation-style-required fields    |               Medium | Zotero can format citations/bibliographies by style, but it does not expose a “missing fields for this CSL style” endpoint. Ship generic citation-core checks first; label style-specific checks as heuristic unless backed by a citeproc validation engine. |
| `library health fix`              |                 High | Writes require Web API key and version preconditions; stale local versions produce `412`. Always preview and show sync/conflict remediation.                                                                                                                 |
| `scope saved-search:KEY`          |               Medium | Saved-search execution is local-only in this API surface; Web API exposes saved-search metadata but not execution. Coverage doc marks `/searches/<key>/items` as local-only.                                                                                 |
| Schema-backed `items new`         |          Medium-high | Correct direction, but `/items/new` is Web-API-only from this CLI’s perspective; local API does not implement it.                                                                                                                                            |
| Linked-file import                |               Medium | Faster and useful for local-first users, but linked files do not sync through Zotero, do not work in group libraries, and are not supported by Zotero mobile apps. ([zotero.org][3])                                                                         |
| Stored-file upload                | Medium-low initially | Feasible via Web API but multi-step and failure-prone: authorization, upload, registration, quota, permissions, conflicts, and rate limits. Build after import manifests are solid. ([zotero.org][2])                                                        |
| PDF metadata recognition          |                  Low | Do not promise Zotero GUI parity. There is no documented API for Zotero’s “Retrieve Metadata for PDF” recognizer. Use DOI extraction plus external providers.                                                                                                |
| Semantic search                   |       Low as drafted | Cut. Reframe later as `vectors import` / host-provided embeddings if truly needed. The CLI should not call a model.                                                                                                                                          |
| Watch-mode sync                   |               Medium | Useful, but should be polling/incremental sync, not assumed push notifications. Keep out of the first value release.                                                                                                                                         |
| Group readiness preflight         |               Medium | Local API group metadata is limited; permissions require Web API/auth checks. ([zotero.org][1])                                                                                                                                                              |
| Resumable export                  |                 High | Feasible, but structured exports are safer than formatted bibliography for large sets because formatted bibliography behavior can ignore limit parameters. ([zotero.org][6])                                                                                 |

---

## The P1 backbone that unlocks the most value

Build **minimal `scope.Spec` + freshness gates inside `library health` first**.

Do not start with a generic architecture package in isolation. A user-visible `library health --scope ... --require-fresh ... --fail-on ...` forces the right abstractions while immediately improving the product.

Priority order:

1. `library health` composed report.
2. Minimal `scope.Spec` used by health.
3. Freshness/provenance envelope and `--fail-on` exit codes.
4. Health remediation plans using the existing mutation substrate.
5. Only then promote mutation/capability registries into packages.

---

## Build this first

Build **`zotero-pp-cli library health` as the flagship command**, with scoped read-only checks, ranked findings, freshness metadata, and `--fail-on`.

That gives every audience immediate value:

Researchers get “is this collection ready?”
PKM users get “will vault sync be trustworthy?”
Agents get “what can I safely act on?”
Automation users get a CI-compatible gate.

Then add `library health fix --dry-run`, not PDF upload. The import pipeline is valuable, but file ingest has more API edge cases. Health is the fastest path to a differentiated, trustworthy product.

[1]: https://www.zotero.org/support/dev/web_api/v3/local_api "Zotero Local API | Zotero Documentation"
[2]: https://www.zotero.org/support/dev/web_api/v3/file_upload "Zotero Web API File Uploads | Zotero Documentation"
[3]: https://www.zotero.org/support/attaching_files?utm_source=chatgpt.com "Adding Files to your Zotero Library | Zotero Documentation"
[4]: https://www.zotero.org/support/dev/web_api/v3/write_requests "Zotero Web API Write Requests | Zotero Documentation"
[5]: https://www.zotero.org/support/dev/web_api/v3/types_and_fields "Zotero Web API Item Type/Field Requests | Zotero Documentation"
[6]: https://www.zotero.org/support/dev/web_api/v3/basics "Zotero Web API Documentation | Zotero Documentation"
