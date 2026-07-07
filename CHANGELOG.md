# Changelog

Notable changes to zotio. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow [SemVer](https://semver.org/).

## [0.1.2] — 2026-07-07

### Added
- **MCPB bundles for Claude Desktop** — every release now ships per-platform `zotio-mcp_<version>_<os>_<arch>.mcpb` bundles (manifest + binary, one-click install).
- **CI guide** on the docs site — [CI for your bibliography](https://orgmentem.github.io/zotio/guide/ci/): the GitHub Action, manuscript gating, badge publishing, exit codes.
- Grouped, conventional-commit release notes (goreleaser changelog) and this curated CHANGELOG.

### Changed
- Install documentation now leads with the first-party channels: `brew install orgmentem/tap/zotio`, signed release binaries, and build-from-source — replacing broken external installer links.
- MCPB manifest refreshed: MIT license, OrgMentem authorship, release-pinned version, brand-consistent display name.
- Zotero trademark disclaimer added to the README, docs footer, and companion action.

## [0.1.1] — 2026-07-07

### Added
- Automatic Homebrew tap publishing on tagged releases (scoped `HOMEBREW_TAP_GITHUB_TOKEN`; formula lands in `Formula/`).
- Live bibliography badge on the README — the docs deploy syncs the maintainer's real library and publishes shields.io endpoint JSON (weekly refresh).

### Fixed
- Honest Homebrew formula description (removed print-time overclaim).
- goimports grouping and test-file permissions flagged by CI.

## [0.1.0] — 2026-07-07

First tagged release: the trust-and-automation layer for Zotero.

### Added
- **Library trust** — `library health` (ranked, CI-gateable report with `--for` presets, `--fail-on` exit-code gate, shields.io `--badge`), `items retract-check` (Crossref Retraction Watch data; opt-in health gate via `--check-retractions`), `items duplicates` + `resolve`, `tags audit` + `fix`, `items audit`, `schema drift`, `collections gaps` (citation-graph gap analysis via OpenCitations/Semantic Scholar).
- **Safe writes** — one preview-first mutation engine behind every write (`--dry-run`/`--yes`, version-guarded PATCHes), reviewable import (`import scan → resolve → apply`, plus `import doi|pmid|arxiv|isbn`), `items enrich` (CrossRef/OpenAlex/Semantic Scholar/Unpaywall, `--validate`), `items preprint-check` + `fix`, append-only `journal` with `journal undo`.
- **Manuscript side** — `items bibcheck <manuscript>` resolves `\cite{}`/pandoc `@citekeys` against the library (`--fail-on-unknown`), `items citekey-conflicts`.
- **Agent surface** — `zotio-mcp` MCP server, machine-readable trust plane (`agent-context`, `capabilities`, freshness), `items summarize` bounded context bundles, `zotio which` goal-to-command resolution.
- **Sync & automation** — local SQLite mirror (`sync`, `watch`, `--health` drift notifications with webhook delivery), reproducible `export snapshot` with content-hash lockfile, `workflow run`.
- **Reading & PKM** — two-way Obsidian/Logseq `vault` sync with conflict-safe write-back, `annotations export`/`timeline`, `reading-list`, `items note-template`, `library wrapped` year-in-review with shareable SVG card.
- **Onboarding** — `zotio init` guided setup (Zotero detection, local API, key, first sync, health check).
- Release engineering: goreleaser builds for 6 platforms, cosign-signed checksums, SBOMs, Homebrew tap.

[0.1.2]: https://github.com/OrgMentem/zotio/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/OrgMentem/zotio/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/OrgMentem/zotio/releases/tag/v0.1.0
