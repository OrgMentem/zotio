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
| `field-report-2026-08-08-walktest.md` | Adversarial walk-test of the deployed build against reports #1–#3, all mutations reverted (fingerprint verified). Confirms the earlier fixes and adds 10 findings. **Retracts report #3's New-1 root cause**: version-less mirror rows were a correlation, not the cause. The real one is two stacked failures — `tags rename` selected against the read plane while writing to the write plane, and the response cache then pinned the empty result for its 5-minute TTL, so **taking a preview made the following apply silently rename nothing and report success**. Also **`items new` on the connector route reporting `failed` for writes that had already succeeded**, so a retrying caller minted a duplicate per attempt. All fixed except W-4 (`items trash` default data source cannot see a just-trashed item). |
| `field-report-2026-08-08-walktest-2.md` | Second adversarial walk-test pass. |
| `field-report-2026-08-08-walktest-3.md` | Third adversarial walk-test pass. |
| `field-report-2026-08-08-walktest-4.md` | Fourth adversarial walk-test pass. |
| `field-report-2026-08-22-papio.md` | First papio-side integration session against a live library. |
| `field-report-2026-08-22-papio-round2.md` | **The source of the storage-naming invariant.** Verified against Zotero's server and client source, then measured live: every storage name — local directory, upload zip, and both WebDAV remote names — derives from the **attachment's** own key, never its parent's, so a re-parent relocates no bytes. Records the only completed connector re-parent (`PATCH … HTTP 204`), against a credentialed library on a much older tree. Cited by `AGENTS.md`. |
| `field-report-2026-08-22-papio-arxiv.md` | arXiv ingest path, papio side. |
| `field-report-2026-08-23-papio-capability-routes.md` | Capability/route negotiation between papio and zotio. |
| `field-report-2026-08-28-connector-live-verification.md` | **The connector import contract, measured live for the first time** against a running Zotero 7 in an isolated profile — the opposite of the 2026-08-22 report, which never contacted a live desktop. Proves the URL fragment survives a real write and is the sole discriminator under growing same-title/same-MD5 ambiguity. Finding 3 records `items delete` failing with `428 Zotero-Server-ID not provided` in a credential-free profile. |
| `field-report-2026-08-30-connector-reverify-at-head.md` | Re-verification at release HEAD after `a24287e` rewrote the connector write client's borrow-and-restore contract. Import path did not regress. **Finding B is the standing residual**: the re-parent `PATCH` is NOT VERIFIABLE in a credential-free profile, so a *completed* re-parent has never been measured on a current tree (`zotio-26a2e7ab6bad82ab`). |
| `field-report-2026-08-30-release-gate-real-library.md` | Release gate 3 for 0.22.0, executed against the operator's **real** WebDAV-backed library with an independent paged oracle per plane, both planes, both directions. Revert detector byte-identical. Names four paths it could not reach under `## Not closed by this run`. |
| `field-report-2026-08-30-conflict-contract-live.md` | Closes the first of those four. `ZOTIO_TEST_STALE_VERSION` forces a stale precondition, provoking Zotero's own 412, so the 0.22.0 breaking claim — a conflict returns the engine's error (exit 1), not `degradedErr` (exit 13) — is verified live rather than by unit tests alone. Revert detector byte-identical. |
| `field-report-2026-08-30-connector-reparent-live.md` | Closes the oldest standing residual: **the first completed connector re-parent measured on a current tree**, on the real WebDAV library the route exists to serve, with the storage-naming invariant verified live (directory named for the attachment, none for the temp parent). Found and fixed a real race — the desktop's own file registration bumps the attachment's version between the route's read and its guarded PATCH, so a 412 is now retried under an ownership check. Also records that trashing a parent does NOT trash its attachment, caught only by the revert detector. |
| `field-report-2026-08-30-vault-push-version-cache.md` | Closes the last write path with no live evidence. `vault push` read its batch version map through the CACHED `Get` while `getNote` beside it used the uncached `GetWithVersion`, so a no-op push left a stale map: proved A/B on the real library that the pre-fix binary reports `1 unchanged` for a managed note an independent oracle confirms is `404`, while the fixed binary reports `remote_deleted` with a resolve hint and a non-zero exit. Revert detector byte-identical. |
| `zotero-api-coverage.md` | Zotero endpoint coverage matrix + the **refresh procedure** to re-run after a Zotero upgrade. See `AGENTS.md`. |
| `obsidian-positioning.md` | Where `zotio` sits vs. the Obsidian/Zotero plugin ecosystem (design positioning). The user-facing vault workflow lives at `docs/guide/vault.md`. |
| `adr/` | Full Architecture Decision Records (technical). User-facing summaries live at `docs/contributing/architecture-decisions.md`. |

Published, user-facing docs live under `docs/` and are built by Zensical — see
the repo `README.md` and `mkdocs.yml`.
