# Internal working notes

Engineering-facing notes kept in the repo for contributors and agents but **not
published** to the documentation site (`docs/`). They contain planning,
maintenance procedures, and design analysis rather than user-facing guidance.

| File | What it is |
| --- | --- |
| `roadmap.md` | Product sequencing and the source of truth for what ships next. |
| `releasing.md` | Release runbook: the tag→GoReleaser flow, version/breaking-change decisions, validation checklist, and footguns (WinGet classic-PAT, OIDC-outage triage, prepend-don't-replace notes). See `AGENTS.md`. |
| `roadmap-oracle-review.md` | Oracle review of the roadmap. |
| `oracle-ingestion-consult.md` | Oracle consult on ingestion design. |
| `feature-map.md` | Internal feature-to-command mapping. |
| `zotero-api-coverage.md` | Zotero endpoint coverage matrix + the **refresh procedure** to re-run after a Zotero upgrade. See `AGENTS.md`. |
| `obsidian-positioning.md` | Where `zotio` sits vs. the Obsidian/Zotero plugin ecosystem (design positioning). The user-facing vault workflow lives at `docs/guide/vault.md`. |
| `adr/` | Full Architecture Decision Records (technical). User-facing summaries live at `docs/contributing/architecture-decisions.md`. |

Published, user-facing docs live under `docs/` and are built by Zensical — see
the repo `README.md` and `mkdocs.yml`.
