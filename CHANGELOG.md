# Changelog

Notable changes to zotio. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow [SemVer](https://semver.org/).

## [Unreleased]

### Added
- `items similar <itemKey>` ranks locally similar items with explainable signals — Jaccard overlap on shared collections (0.30), tags (0.25), and creators (0.10), an exact-match venue signal (0.10), plus synced-fulltext rare-word overlap (0.25). Deterministic, offline, no embeddings; every hit carries human-readable "why" reasons, per-signal scores in `--json`, and `--limit`/`--min-score` filters. Complements `items related` (explicit relation edges) with discovered similarity. Requires a synced local store (`zotio sync`; text signal needs `zotio sync --fulltext`).
- `items enrich --missing-pdf` can now download the open-access PDF instead of only linking it: `--attach-mode linked-url|linked-file` (default `linked-url`, unchanged behavior). `linked-file` downloads the Unpaywall-resolved PDF to `--pdf-dir` — content-type check (`application/pdf`, `application/octet-stream`, or absent), `%PDF-` magic-header validation, 100 MiB streaming cap, non-public destination addresses rejected at dial time, never clobbers an existing file — and creates a `linked_file` child attachment. Downloads happen only at apply time; preview names the mode and destination. Stored (imported-file) retro-attachment waits on the deferred Web API upload protocol and is refused with that reason.

### Fixed
- Synced local reads no longer surface stale live copies of items moved to Zotero's trash. Store schema v4 atomically reconciles `items`/`items-trash` by Zotero object version (trash wins ties), migrates existing stores and removes stale FTS rows; selecting `items` for sync now also fetches `items-trash`. `items trash --data-source local` now reads the correct resource, preserves Zotero's `dateModified` ordering before `--start`/`--limit`, and distinguishes synced-empty libraries from unsynced stores.
- Outbound HTTP policy now rejects cross-origin redirects for fixed metadata providers and Zotero Web API requests, refuses every Connector redirect, enforces public-IP checks at the actual dial (including IPv4-mapped IPv6 forms), and prevents injected HTTP clients or redirect callbacks from weakening those invariants. `/keys/current` responses are capped at 1 MiB.
- Container builds now stamp an explicit version and pin both base images by digest; Official MCP Registry publication verifies the pinned publisher's SHA-256 before OIDC execution and runs in a separately recoverable post-release job.

## [0.7.0] — 2026-07-10

### Added
- `zotio-mcp` now reports its build version via `--version`, the MCP server's version field, and the startup banner (previously unversioned); the release workflow fails if either binary stops reporting the tag.

### Fixed
- Better BibTeX citekeys are now also read from the `citationKey` data field the BBT plugin exposes via the local API — previously only pinned `Citation Key:` Extra lines were recognized, so libraries with dynamic (unpinned) keys got a false `better_bibtex` precondition refusal from `items bibcheck` and empty results from `items citekey-conflicts`; `items find --citekey` matches the field too.
- `import discover` no longer aborts the whole chase when one source item's provider fetch fails (for example OpenCitations returning an oversized response for a heavily-cited paper): the failure is recorded per source in the summary and the remaining sources proceed; the run only errors when every source fails. OpenAlex forward pagination now requests only `id,doi`, keeping pages of heavily-cited works under the response cap.

### Changed — breaking
- **Removed the 28 typed spec-derived MCP endpoint tools** (`collections_*`, `items_get/list`, `schema_*`, `tags_*`, …). They were frozen at generator retirement, bypassed the CLI's mutation gates and fixes (e.g. the `schema` library-prefix 404), and already rejected writes. The CLI command tree is now the single MCP source of truth: the `zotio-mcp` server exposes framework tools (`context`/`search`/`sql`) plus the `command_search`/`command_run` facade by default, or the per-command mirror via `ZOTIO_MCP_SURFACE=mirror`. **Hosts pinned to the old typed tool names must switch to `command_run`** (facade) or the mirror surface. See `notes/adr/0003-retire-typed-mcp-endpoint-tools.md`.

## [0.6.0] — 2026-07-10

### Added
- `items related <itemKey>` lists an item's relation edges from the synced store — outgoing and incoming, predicate-tagged (`dc:relation`, `owl:sameAs`, …), preserving cross-library and off-store targets as external edges; also exposed as the MCP resource `zotero://items/{key}/related`.
- `creators audit` inventories creator-name variants in three confidence tiers (exact-after-normalization, compatible initials, ambiguous surnames) with canonical candidates and the shared findings envelope; `--orcid` corroborates variants against Crossref author ORCIDs, persisted in a local-only sidecar (never written to Zotero).
- `creators audit fix` renames creator variants preview-first: exact-normalization variants are auto-planned, initial-vs-full variants only via explicit `--map "J. Smith=John Smith"`, ambiguous variants never; applies as full-creators-array PATCHes with version preconditions (journaled; not undoable).
- `import discover --scope <expr>` chases citations backward (`--direction backward`, default), forward, or `both` via OpenCitations/Semantic Scholar/Crossref/OpenAlex, dedupes against the library (DOI and normalized title) before emitting, and writes ranked, provenance-tagged entries (`discovery.direction/provider/count/cited_by_keys`) into a reviewable import manifest for `import apply`.
- Import manifests are now schema v2 (optional per-entry `discovery` provenance); v1 manifests remain readable.
- External metadata-provider requests (OpenCitations, Semantic Scholar, Crossref, OpenAlex) used by `import discover` and `collections gaps` are cached for 7 days under the user cache dir; `--no-cache` bypasses.

### Fixed
- `items enrich --yes` no longer replaces the item's entire Extra field with the provenance line — existing Extra content (Better BibTeX `Citation Key:` lines, user notes) is preserved and the provenance line is appended; the mutation preview now shows the Extra change, and same-day re-runs do not duplicate the line.

## [0.5.0] — 2026-07-09

### Added
- `items bibliography` renders a scope-wide formatted bibliography in any CSL style, server-side via the Web API (`--scope`, `--style`, chunked at 50 keys per request).
- `items bibcheck` accepts multiple manuscripts, flags cited items missing citation-core fields (`incomplete_citation` findings with file:line evidence), emits the canonical findings envelope in JSON, and gains `--fail-on <high|any|none>` (exit `11`) alongside the existing `--fail-on-unknown`.
- `export snapshot verify <lockfile>` classifies drift against the current library as added/removed/changed/touched by comparing the recorded content SHA-256 — version-only churn is `touched`, never drift; `--fail-on-drift` exits `11`.
- Diagnostics (`library health`, `items audit`, `vault audit`, `items citekey-conflicts`, `items duplicates`, `items enrich --validate`, `items preprint-check`) emit a shared `findings` array (kind/severity/item_key envelope), and `--keys-from` ingests it: `zotio library health --json | zotio items enrich --missing-doi --keys-from -` now composes directly.
- **Package distribution expanded** — tagged releases now publish a Scoop manifest to `OrgMentem/scoop-bucket` (`scoop bucket add zotio https://github.com/OrgMentem/scoop-bucket && scoop install zotio`), open a WinGet manifest PR (`winget install OrgMentem.zotio`), and attach Linux `.deb`/`.rpm`/`.apk` packages; Homebrew (`brew install orgmentem/tap/zotio`) covers macOS and Linux.

### Changed
- `--deliver` now delivers rendered reports when a quality or freshness gate fails (exit `11`/`12`) — previously the report was dropped exactly when a CI consumer needed it. Usage and config errors still skip delivery, and a delivery failure never masks the command's exit code.
- Export snapshot lockfiles record each item's title and normalized content SHA-256.

### Changed — breaking
zotio is pre-1.0, so these ship in a minor release without a major-version signal. Scripted and agent consumers should review before upgrading:
- **Precondition enforcement replaces silent empty-success.** A command whose declared `requires` (synced store, live local API, Better BibTeX, desktop connector) is unmet now refuses with a structured `precondition_unmet` envelope and **exit `9`** instead of returning empty results with exit `0`. Scripts that treated an empty result as success will now observe a non-zero exit. MCP `command_search`/`command_run` command detail now carries `operation`/`requires`/`destructive`.
- **Diagnostic JSON shapes replaced by the canonical findings envelope** for `items citekey-conflicts`, `vault audit`, `items enrich --validate`, and `items preprint-check`. Consumers parsing the old per-command shapes must switch to the shared `findings` array. (`items audit` and `items duplicates` keep their existing fields and add `findings` alongside — non-breaking.)
- **`items cite --style <csl-id>` renders named CSL styles through the Web API** instead of silently falling back to Zotero's default style, and **refuses with exit `9` without an API key**. Scripts relying on the silent default-style fallback now fail loudly rather than emitting wrong-style output.

## [0.4.0] — 2026-07-09

### Security
- Redirect handling now strips the Zotero API key/Authorization header on any scheme-or-host change (previously an https→http downgrade on the same host kept the credential).
- Webhook delivery (`workflow run`, `watch --health`) and feedback submission now validate and dial the destination IP together, closing a DNS-rebinding SSRF gap where the resolved address could change between validation and the request.
- The local SQLite mirror and the mutation journal are now created with private permissions (`0700` directories, `0600` files, with a defensive `chmod` on pre-existing paths), matching the existing API response cache.
- `zotio-mcp --mcp-auth-token <value>` now refuses a literal token (visible in `ps`/shell history) with guidance; use the new `--mcp-auth-token-file` or `ZOTIO_MCP_TOKEN` instead.
- `zotio doctor` redacts userinfo passwords and token-like query parameters (`token`, `key`, `api_key`, `secret`, `password`, `auth`) from the reported base URL.
- OpenAlex abstract reconstruction (`items enrich`) rejects out-of-range word positions instead of sizing an allocation directly from provider-controlled input.
- Terminal table/card output strips C0 control bytes and DEL from synced item metadata, closing an ANSI/OSC terminal-escape injection path.
- The MCP `sql` tool now runs under a 15s deadline with a 5000-row cap (`{rows, truncated, row_limit}` response envelope); `sql` and `search` results are now clamped through the same response-budget limit as typed MCP tools.

### Fixed
- The API response cache key now includes request headers, so header-varying `GetWithHeaders` calls no longer collide.
- `collections move` now requires `--yes` to apply (preview makes no HTTP call) and sends a version-checked write, matching `items move`.
- `tags audit fix --max-changes` now counts actual per-item writes instead of tag aliases, so a popular alias can no longer slip thousands of writes past a small approved cap.
- The `sync` worker pool now checks for cancellation immediately after dequeuing a resource, so a canceled sync can no longer start another long resource pass.
- `sync` NDJSON events are now built with `encoding/json` instead of hand-escaped strings; control characters or backslashes in error messages no longer corrupt the event stream.
- `vault` sync now recognizes CRLF frontmatter delimiters, fixing key extraction and duplicate notes on Windows-synced vaults.
- `tail`'s file sink now creates missing parent directories instead of silently dropping events.
- The connector client no longer disables the shared HTTP client's timeout as a side effect of a single recognition request; its 2xx response reads are now capped instead of unbounded.

### Changed
- MCP server environment variables were renamed: `PP_MCP_SURFACE` → `ZOTIO_MCP_SURFACE` and `PP_MCP_TRANSPORT` → `ZOTIO_MCP_TRANSPORT`. The old `PP_*` names are no longer recognized.
- Cobra command annotation keys surfaced through `agent-context` were renamed from the `pp:` prefix to `zotio:` (e.g. `zotio:endpoint`, `zotio:method`, `zotio:path`, `zotio:destructive`).

### Removed
- CLI Printing Press generator scaffolding was retired: generation provenance (`.printing-press.json`), the patch catalog (`.printing-press-patches.json`, with history in git), vendored press dev skills, and the `Generated by CLI Printing Press ... DO NOT EDIT` headers on 97 hand-maintained Go files. The project is fully hand-maintained.
- Source comments, filenames, notes, and CI configs no longer reference the maintainer's local review tooling: patch markers and tracking IDs were scrubbed, and the README bootstrap attribution sentence was removed.

## [0.3.0] — 2026-07-08

### Added
- `library health --baseline <path>` compares the current findings with a saved baseline; a missing file is treated as an establishing run with zero new findings, and baseline-mode human output reports `New since baseline: N (resolved M)` or `Baseline established (N findings recorded)`.
- `library health --write-baseline <path>` atomically writes schema-versioned baseline JSON with an RFC3339 `generated_at`, the selected preset, and sorted finding identities shared with `watch --health`.
- `library health --fail-on-new <critical|high|info|any>` gates only findings that are new since `--baseline`; it is a usage error without `--baseline` and exits `11` when a new finding meets the threshold.
- `library health --report <path>` writes the full JSON health report sidecar in both human and badge modes, while the existing `--badge --json` conflict remains unchanged.
- `library health --fail-on none` disables the absolute findings gate, overriding the preset default so delta-only CI can combine `--baseline`, `--write-baseline`, and `--fail-on-new`.

## [0.2.0] — 2026-07-07

### Added
- **`zotio demo` — zero-setup trial sandbox.** Seeds a bundled sample library (34 classic papers — including one genuinely retracted — with duplicates, citekey conflicts, tag drift, annotations, and a reading queue) into a separate `demo.db`; `ZOTIO_DEMO=1` reroutes any command to the sandbox with a pristine, key-less config that never touches the real store, config file, or credentials.
- **Recorded demos** — VHS tapes (`docs/tapes/`, `make demos`) render deterministic GIFs of the hero features against the demo sandbox; embedded in the README and docs site.

### Changed
- `reading-list` now supports `--data-source local` read parity (works offline from the synced store — and in the demo sandbox).

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

[Unreleased]: https://github.com/OrgMentem/zotio/compare/v0.7.0...HEAD
[0.7.0]: https://github.com/OrgMentem/zotio/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/OrgMentem/zotio/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/OrgMentem/zotio/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/OrgMentem/zotio/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/OrgMentem/zotio/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/OrgMentem/zotio/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/OrgMentem/zotio/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/OrgMentem/zotio/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/OrgMentem/zotio/releases/tag/v0.1.0
