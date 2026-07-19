# Changelog

Notable changes to zotio. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow [SemVer](https://semver.org/).

## [0.11.0] — 2026-07-20
### Added
- Local read-parity coverage extended (ADR-0002 scope): `resolveRead` now routes `annotations` search/timeline/export and `items` collections-of/note-template/fulltext against the synced local store, and `--refresh` vs `--data-source local` conflicts are rejected explicitly instead of silently preferring one source.
- The MCP mirror surface (`ZOTIO_MCP_SURFACE=mirror`) now exposes the endpoint commands that were missing from it (`collections_create`, …), bringing the per-command mirror back in line with the CLI command tree (golden regenerated). `workflow run` failed-step checkpoints are resumable over MCP, archive pagination carries real Zotero keys through `start`/`limit`, and capability metadata reports per-command safety.
- Install & packaging: the README install story is now per-OS tabbed (macOS/Linux/Windows/Prebuilt/From-source), documenting the previously undocumented Linux distro packages (`.deb`/`.rpm`/`.apk`) and the Windows Scoop bucket.

### Changed — breaking
- **More commands now exit non-zero on partial failure instead of a silent `0`.** Extending the 0.10.0 degraded-exit contract: `watch` exits non-zero when it stops without a successful sync cycle, `vault push`/`sync` exit degraded on unreadable/partial content, and JSONL `import`/import-apply exit non-zero on per-record failures. Scripts keying on a `0` exit from these must inspect the error/warnings.
- **`items create/update/delete/restore` are now preview-by-default; mutation applies only under the resolved apply mode.** A bare `items create` (and the update/delete/restore paths) renders a dry-run preview rather than applying immediately; scripted/agent callers must pass the apply flag. `items update`/`delete` also require a `version` (or `--dry-run`) up front.
- **`collections create` now accepts an object *or* an array of objects on stdin** (a non-object/array payload is rejected) and threads `parentCollection` (including `parentCollection:false` for top-level). Callers that relied on single-object-only parsing are unaffected; those parsing the response should expect the array-payload contract.
- **`/collections/top` no longer resolves the literal segment `top` as a local collection key** — it now returns an explicit unsupported-local-scope error (ADR-0002).
- **Homebrew distribution migrated from a formula to a cask** (`brews` → `homebrew_casks`). `brew install orgmentem/tap/zotio` still works on macOS and now installs both `zotio` and `zotio-mcp` (with a quarantine-stripping post-install hook), but **casks are macOS-only** — Linuxbrew is no longer a supported install path; Linux users use the `.deb`/`.rpm`/`.apk` packages.

### Fixed
- Local resolver: the get-by-ID fallback now `url.PathUnescape`s the path segment before using it as the store key (percent-escaped tag names previously missed) and wraps malformed-escape errors instead of passing them through.
- Dry-run isolation: `items move`, `tags add`/`remove`, and file `import` (including the `--via connector` route) short-circuit before client creation and version-precondition/desktop-connector fetches, and write clients skip hybrid route resolution (`keys/current`) under `--dry-run` — so a preview never touches the network or resolves a write target.
- Local store: pure reads open the store read-only and migration-free (writable reopens only for ORCID evidence and write-through mirrors); negative `limit`/`start` are rejected; completed-but-empty syncs return `[]` rather than a missing-data error; keyed single-object live reads are write-through cached; and store-open failures now surface contextually in `import scan` and `items summarize`.
- Error propagation across the read/sync paths: sync page-decode and empty-envelope classification, checkpoint-read propagation, `--full` cursor reset, store `ListIDs`/`ResolveByName` scan errors, `search` decode errors, and `items enrich` local-read fan-out failures are no longer swallowed.
- Assorted correctness: CrossRef empty-envelope rejection, trashed PDFs excluded from `import` coverage, `feedback list` surfaces corrupt journal lines, demo seeding propagates sync-state errors, MCP archive status propagates scan failures, `AdaptiveLimiter` releases slots for cancelled waiters, external fetch reuses memoized transports, `export` honors an explicit local data source, and `items summarize` fulltext uses a streaming, parent-scoped join.
- Contracts/retry: the safe-retry predicate now only retries idempotent requests — GET/HEAD, or writes carrying a `Zotero-Write-Token` or an RFC conditional precondition (`If-Unmodified-Since-Version`/`If-Match`/`If-None-Match`) — so a rate-limited (429) or transport-failed non-idempotent write is no longer blindly retried; `export` gains pagination with scope-fingerprinted resume, CSL-JSON output is a single array, `items recent` pages the full candidate set, and linked-url PDF `contentType` is set.
- CI now gates releases on the tagged commit: `go test ./...` runs before GoReleaser, the test matrix adds `macos-latest` and `-race`, a cross-build job compiles/vets windows/darwin/linux, vuln scanning moved to a weekly cron, and local git hooks (`make hooks`) enforce identity/gofmt/vet at commit and merge/am time.

## [0.10.0] — 2026-07-18
### Added
- Opt-in release-update discovery, surfaced in `zotio doctor`. Disabled by default — a nil `[updates]` config section means no checks; `zotio init` offers to enable it, and `doctor` then reports when a newer public release is available with a channel-appropriate upgrade hint (Homebrew vs. source build). The check is one anonymous GET to the public GitHub releases endpoint, cached in the data dir and rate-limited to once a day; it collects no user data, and every network/cache/decoding failure is soft (you get the last cached result or nothing, never a surfaced error).
- Per-command context propagation on the CLI path: the root command runs under the interrupt context and each command's `cmd.Context()` is seeded into the HTTP client (`Client.SetContext`), so per-command deadlines and MCP request cancellation now abort in-flight Zotero/provider requests — previously only process-level Ctrl-C/SIGTERM did.
- Brand: the README header wordmark (`logo-wordmark.svg`, `logo-wordmark-dark.svg`) is now an animated SVG. On a calm ~10s loop the ring draws on, the z snaps into place, the wordmark rises in, and the gold i-dot rolls along the ring rim, wakes up, realises it is off its mark, leaps into the break with a squash landing, blinks, and gives a little left-eye wink before settling. The ring now tracks the wordmark's ink/paper text color per theme (ink on light, paper on dark) while the z keeps a fixed indigo identity (a lighter `#6366F1` in the dark variant, matching the docs' own dark-mode indigo token, since the light-mode hex reads too flat against a dark background) — mirroring sister project *papio*'s ring-tracks-text / letterform-carries-the-brand-hue split. Pure CSS keyframes inside the SVG (no scripts or SMIL, safe for GitHub's `<img>` rendering); the resting state is identical to the prior static logo and `prefers-reduced-motion: reduce` shows it with no animation. The static standalone icon (`logo.svg`) gets the same ink-ring/indigo-z split; `logo-mono.svg` (the flat header-bar mark, always rendered on `mkdocs.yml`'s indigo `primary` background) intentionally keeps its single flat color.

### Changed — breaking
- **Read/report commands now exit non-zero (13, "degraded") with a machine-readable `warnings[]` instead of silently succeeding while dropping errors.** `vault push`, the `doctor` cache report, `import scan`, `items summarize`, `export`, and `collections export` previously omitted unreadable notes/rows/attachments/PDFs (or a truncated write) and still exited `0`; they now surface the failure and exit `13`. Scripts and agents that keyed on a `0` exit from these commands must treat `13` as "completed with warnings" (inspect `warnings[]`) rather than a hard failure.

### Fixed
- Swallowed errors are now surfaced across the read, write, sync, and MCP paths (35 static-audit findings over two passes). Writes fail closed: `items update`/`delete` abort on a failed version read instead of masking it behind a later 428 (delete keeps the 404 idempotent no-op); `workflow archive` stops advancing its cursor and exits non-zero on per-resource failures; fulltext sync and `tail` no longer advance the checkpoint past a fetch/persist/delivery failure; a store upsert fails its transaction on an FTS-index error so a committed row is always searchable; and an applied mutation whose journal write fails reports degraded rather than claiming reversibility. MCP argument serialization is fixed too: array flags serialize as repeated `--flag` pairs (facade, mirror, and `workflow_submit`), explicit `false` bools render `--flag=false`, the shell arg parser handles backslash escapes and single quotes, and the `sql` tool always checks `rows.Err()`.
- Local-mirror writes are now version-monotonic: a strictly-older Zotero version can no longer clobber a newer local row (the FTS rebuild is skipped when the row is retained, keeping the index consistent), and the library-version checkpoint takes `MAX(existing, incoming)` so a slower concurrent `sync`/`tail` cannot regress it. Closes an out-of-order live-read regression, a `tail` cursor regression, and a data-loss vector under concurrent sync; equal/newer and versionless payloads still update, preserving idempotent re-sync.
- Assorted correctness and concurrency-safety fixes: the `QueryItems` FTS join is scoped by `resource_type` so a same-keyed collection/tag/search row can no longer surface or duplicate an item; a `query:` scope now enumerates the full cohort (a negative limit means unlimited) instead of capping at 50; ORCID sidecar upserts route through a write-serialized path; cache and profile writes use a unique temp file plus atomic rename so a concurrent reader never observes truncated JSON; and the MCP orchestration tree builds under the same lock command execution holds, so `command_search` cannot race `command_run` on package-global output flags.
- MCP `capabilities` and `analytics` now return their output through the command writer instead of `os.Stdout`, so in-process MCP execution captures the payload (previously empty) and no longer leaks it to the server process stdout.

## [0.9.0] — 2026-07-15
### Added
- `workflow run` is now transactional: without `--yes` it renders one consolidated preview (mutating steps are forced to `--dry-run`; read-only steps run normally), a single `--yes` on `workflow run` is the one approval for every step (specs that embed their own `--yes`/`--dry-run` are rejected), every step applied under that approval records its journal entry with a shared `workflow_run_id` (`journal list --workflow <id>` filters to one run), and an interrupted apply leaves a `<spec>.checkpoint.json` sidecar so `workflow run --yes --resume` continues where it stopped (spec-hash-verified, succeeded steps skipped, same run id) — re-running without `--resume` while a checkpoint exists is refused.
- `workflow run` specs are now expressive: top-level `vars` with `${vars.NAME}` placeholders in step args (overridable per run with repeatable `--var NAME=value`; undeclared names are refused), inter-step data-flow — a named step's captured output is addressable as `${steps.NAME.output}` in later args and pipeable into a later step's stdin with `"stdin_from"` (so `library health --json` can feed `items enrich --keys-from -` as one workflow) — and per-step conditionals via `"when": {"step": ..., "is": "ok"|"failed"|"skipped"}`. All references are validated at load time (unknown/forward references and malformed placeholders are rejected loudly); the resume checkpoint (schema v2) records resolved variables and completed-step outputs, so `--resume` refuses a changed `--var` set and cross-interruption data-flow keeps working.
- Workflows can now be triggered and agent-submitted: `watch --workflow <spec.json>` runs a workflow after every successful sync cycle and `tail --workflow <spec.json>` runs one only after a poll cycle that emitted change events (quiet when nothing changed) — triggered runs preview unless the invocation carries `--yes`, a trigger failure never stops the loop, and a failed applied run leaves its checkpoint so later applied triggers refuse loudly until resumed; the MCP server gains a dedicated `workflow_submit` tool (both facade and mirror surfaces) that accepts an inline validated step schema — each step names a mirrorable command and is checked against the same per-command safe-flag allowlist as `command_run`, closing the bypass that kept `workflow run` MCP-hidden — then executes through the transactional runner (preview unless `yes`, one journal run id, temp spec and checkpoint always cleaned up; failed applies are re-submittable, not resumable).
- New `library prisma` reports PRISMA 2020 identification-stage counts for a screening corpus from the synced local store: records identified with a per-source-database breakdown (Zotero's libraryCatalog provenance), duplicate records removed (DOI + normalized-title detectors with cross-detector cluster merging so double-flagged pairs count once), and records after deduplication — the input to screening — scoped to a collection or tag via `--scope`, with a `prisma` JSON block that maps one-to-one onto the flow-diagram boxes; screening itself stays out of scope by design (Rayyan/ASReview own it — the wedge is arriving there with a certified, deduped, counted corpus).

### Changed — breaking
- **`workflow run` is now preview-by-default.** In 0.8.0 `zotio workflow run <spec>` executed every step immediately; it now renders one consolidated dry-run preview and applies only with an explicit `--yes` on the `workflow run` command. Specs that embed their own per-step `--yes`/`--dry-run` are rejected at load — the workflow owns approval. Scripts or agents that relied on `workflow run` applying without `--yes`, or on step-level approval flags inside a spec, must pass `--yes` to `workflow run` and drop the step-level flags.

### Fixed
- MCP-applied mutations now record journal entries and write through to the local mirror. Since the `command_run` facade shipped (0.7.0) the `zotio-mcp` server never installed the journal/mirror hooks (only `cli.Execute` did), so writes applied over MCP left no audit trail and could leave the `search`/`sql` tools reading stale local state until the next sync; the server now installs them at startup, covering both `command_run` and `workflow_submit`.

## [0.8.0] — 2026-07-12

### Added
- `items similar <itemKey>` ranks locally similar items with explainable signals — Jaccard overlap on shared collections (0.30), tags (0.25), and creators (0.10), an exact-match venue signal (0.10), plus synced-fulltext rare-word overlap (0.25). Deterministic, offline, no embeddings; every hit carries human-readable "why" reasons, per-signal scores in `--json`, and `--limit`/`--min-score` filters. Complements `items related` (explicit relation edges) with discovered similarity. Requires a synced local store (`zotio sync`; text signal needs `zotio sync --fulltext`).
- `items enrich --missing-pdf` can now download the open-access PDF instead of only linking it: `--attach-mode linked-url|linked-file` (default `linked-url`, unchanged behavior). `linked-file` downloads the Unpaywall-resolved PDF to `--pdf-dir` — content-type check (`application/pdf`, `application/octet-stream`, or absent), `%PDF-` magic-header validation, 100 MiB streaming cap, non-public destination addresses rejected at dial time, never clobbers an existing file — and creates a `linked_file` child attachment. Downloads happen only at apply time; preview names the mode and destination. Stored (imported-file) retro-attachment waits on the deferred Web API upload protocol and is refused with that reason.
- Colored terminal output is now on by default at a TTY (previously gated behind `--human-friendly`): bold card titles, dim labels and timestamps, cyan item types. Kill switches unchanged — `--no-color`, `NO_COLOR`, and `TERM=dumb` always win; piped output still auto-switches to JSON, so agents are unaffected. `--human-friendly` now forces color on for non-TTY output.
- `search` renders human-readable cards/tables at a terminal like the other list commands, instead of dumping raw JSON envelopes.

### Fixed
- `tags list` no longer warns "8/8 tags items skipped (no extractable ID field found)" on every run: the store and sync each had their own resource ID-override map and the store's copy was empty, so `UpsertBatch` could never key tags by name. There is now one shared map, which also means live tag lookups are write-through cached for offline use instead of silently dropped.
- Synced local reads no longer surface stale live copies of items moved to Zotero's trash. Store schema v4 atomically reconciles `items`/`items-trash` by Zotero object version (trash wins ties), migrates existing stores and removes stale FTS rows; selecting `items` for sync now also fetches `items-trash`. `items trash --data-source local` now reads the correct resource, preserves Zotero's `dateModified` ordering before `--start`/`--limit`, and distinguishes synced-empty libraries from unsynced stores.
- Outbound HTTP policy now rejects cross-origin redirects for fixed metadata providers and Zotero Web API requests, refuses every Connector redirect, enforces public-IP checks at the actual dial (including IPv4-mapped IPv6 forms), and prevents injected HTTP clients or redirect callbacks from weakening those invariants. `/keys/current` responses are capped at 1 MiB.
- Container builds now stamp an explicit version and pin both base images by digest; Official MCP Registry publication verifies the pinned publisher's SHA-256 before OIDC execution and runs in a separately recoverable post-release job.
- Human tables and cards now align on display width: ANSI style codes no longer skew tabwriter padding, East Asian wide runes count as two columns, and the card label column was off by one for the longest label. Cell truncation is rune-safe (no more mojibake on long non-ASCII titles).
- Nested Zotero objects in card output render domain summaries instead of raw JSON: tags show the tag name, creators show "First Last" (annotated with non-author roles); the previous generic summarizer targeted shop-order shapes (`qty`/`price`/`Side1`) that never occur in Zotero payloads.
- Card and table field order is deterministic: fields sort alphabetically within priority tiers instead of following map iteration order, which shuffled output between runs.

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
- `library wrapped --card` gains `--card-style overview|rhythm|picks|cycle`: three share-card layouts (overview: hero + type mix + highlights; rhythm: streak/busiest-day/weekday stat blocks with a large labeled month chart; picks: deep cut, most annotated, top tag, ranked venues/authors) plus `cycle`, a single SVG that crossfades through all three with CSS keyframes (works in GitHub READMEs, honors prefers-reduced-motion). The README embeds the cycling card instead of a terminal GIF.
- The demo library fixture gains a 4-day addition streak and a 2-item busiest day in June 2026 so sandbox wrapped output exercises every highlight.
- The wrapped SVG share card is redesigned to match the terminal overhaul: gradient background with accent strip, hero counter with annotation/streak chips, a full-width type-mix ratio bar with legend, a Highlights list (deep cut, most annotated, busiest day, top tag), peak-highlighted month chart, severity-colored PDF-coverage meter, and a "computed locally" footer. Sections with no data are omitted.
- `which` renders through the styled table path (bold headers, aligned display-width columns, clipped cells) instead of raw `%-24s` formatting; the README tour now closes on `which 'undo a bad edit'` so retraction checking isn't shown twice across the demo media.
- `library wrapped` redesigned: hero counters, monthly bars with a highlighted peak, a stacked type-mix ratio bar with color legend, a Highlights block (busiest day, favorite weekday, longest streak, deep cut, hot-off-the-press count, most-annotated item, top tag), full first-author names ("LeCun, Yann"), severity-colored PDF-coverage bar, and a share-card hint. The SVG card gains a streak/busiest-day footer. JSON output is additive (`highlights` object); existing fields unchanged.
- `items audit` and `tags audit` summaries render through the styled table path instead of raw tabwriter/markdown headings; table headers show `DATE ADDED` instead of `DATE_ADDED`; provenance lines are dim and pluralize correctly ("1 result").
- The write-safety diagram's flow arrows are consistent and orthogonal (no more bezier that appeared to route REFUSE into APPLY); gate annotations no longer clip.
- `library stats` renders proportional bar charts with aligned counts instead of bare tabwriter columns.
- `printTable` commands (`retract-check`, `bibcheck`, `groups`, `which`, importer listings) render through the width-aware styler: bold headers, dim keys/dates, severity-colored STATUS cells (red retracted, yellow correction, green ok), and cells clipped to 48 columns so rows stay terminal-sized (JSON output keeps full values).
- Demo GIFs re-recorded against the styled output; the docs/README tour now walks search, duplicate detection, stats, and goal resolution.
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

[0.11.0]: https://github.com/OrgMentem/zotio/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/OrgMentem/zotio/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/OrgMentem/zotio/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/OrgMentem/zotio/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/OrgMentem/zotio/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/OrgMentem/zotio/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/OrgMentem/zotio/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/OrgMentem/zotio/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/OrgMentem/zotio/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/OrgMentem/zotio/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/OrgMentem/zotio/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/OrgMentem/zotio/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/OrgMentem/zotio/releases/tag/v0.1.0
