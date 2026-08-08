# `dev/` — internal working notes

Engineering-facing notes kept in the repo for contributors and agents but **not
published** to the documentation site (`docs/`). They contain planning,
maintenance procedures, and design analysis rather than user-facing guidance.

Throwaway artifacts (one-shot Oracle runs and other scratch) belong in
`dev/scratch/`, which is gitignored.

| File | What it is |
| --- | --- |
| `roadmap.md` | Product sequencing and the source of truth for what ships next. |
| `releasing.md` | Release runbook: the tag→GoReleaser flow, version/breaking-change decisions, validation checklist, and footguns (WinGet classic-PAT, OIDC-outage triage, prepend-don't-replace notes). See `AGENTS.md`. |
| `roadmap-oracle-review.md` | Oracle review of the roadmap. |
| `oracle-ingestion-consult.md` | Oracle consult on ingestion design. |
| `feature-map.md` | Internal feature-to-command mapping. |
| `field-report-2026-08-08.md` | Reproduced bug/friction findings from a live PDF-ingest-and-file session (0.16.1): read-after-write incoherence across the local/web planes, false `doctor`/`sync` health signals. Finding 1 is retracted — see the follow-up. |
| `field-report-2026-08-08-library-hygiene.md` | Follow-up from a live tag-normalisation session (0.16.1). **P0: `tags rename` cannot write at all** (`expected_version: 0` → every op rejected); plus audit denominator bugs, missing batch-apply, and `import pdf` creating duplicates it can already detect. |
| `field-report-2026-08-08-verification.md` | Verification pass over the fixes for the two reports above. 9 findings confirmed fixed (P0 rename writes; stale-plan guard holds — 71/71 no-op, zero writes); 9 new, incl. **`items delete` permanently destroying items while documenting "moves to trash"** and write-through rows losing their version so `tags rename` silently selects nothing. **All 9 are fixed** — see CHANGELOG `[Unreleased]`. One correction to the report: New-3's frozen mirror had a second cause it did not reach — row upserts are version-monotonic, so rows holding a web-plane version (11973) rejected the same item from the local plane (version 65) as "older", which defeated `--full` too. So the "three-week stale plan" the report credits the guard with surviving was not stale but *wrong*: those 53 merges had applied, and the mirror could not refresh to show it. |
| `zotero-api-coverage.md` | Zotero endpoint coverage matrix + the **refresh procedure** to re-run after a Zotero upgrade. See `AGENTS.md`. |
| `obsidian-positioning.md` | Where `zotio` sits vs. the Obsidian/Zotero plugin ecosystem (design positioning). The user-facing vault workflow lives at `docs/guide/vault.md`. |
| `adr/` | Full Architecture Decision Records (technical). User-facing summaries live at `docs/contributing/architecture-decisions.md`. |

Published, user-facing docs live under `docs/` and are built by Zensical — see
the repo `README.md` and `mkdocs.yml`.
