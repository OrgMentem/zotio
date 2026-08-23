# zotio Agents Guide

This repo was bootstrapped by [CLI Printing Press](https://github.com/mvanhorn/cli-printing-press) in 2026-05, but it has been hand-maintained since. The generator was retired on 2026-07-08: there is no regeneration path, and systemic fixes no longer route upstream first.

## Local Operating Contract

Start by asking the generated CLI for current runtime truth:

```bash
zotio doctor --json
zotio agent-context --pretty
```

Use runtime discovery instead of relying on a copied command list:

```bash
zotio which "<capability>" --json
zotio <command> --help
```

Add `--agent` to command invocations for JSON, compact output, non-interactive defaults, no color, and confirmation-safe scripting:

```bash
zotio <command> --agent
```

Before running an unfamiliar command that may mutate remote state, inspect its help and prefer a dry run:

```bash
zotio <command> --help
zotio <command> --dry-run --agent
```

Use `--yes --no-input` only after the target, arguments, and side effects are clear.

For install, auth, examples, and longer product guidance, read `README.md` and `SKILL.md`. This file intentionally stays small so repo-local agents get invariant local guidance without duplicating the generated docs.

For CI, the [zotio-action](https://github.com/OrgMentem/zotio-action) GitHub Action ([marketplace](https://github.com/marketplace/actions/zotio-bibliography-health-for-zotero)) packages install → sync → gate on `library health` exit codes; see `docs/guide/ci.md`.

Before cutting a release (tagging `v*`), read `dev/releasing.md` — release flow, version/breaking-change decisions, validation checklist, and footguns. When the release coordinates with papio (the acquisition-side sister project), also read `~/@dev/papio/.agents/skills/papio-release/SKILL.md`: papio enforces a minimum-zotio-version floor and its release order depends on whether that floor moved.

## Dependency Updates

Two invariants that a routine `go get -u` breaks silently. Both are gated, so `make ci` catches them before CI does.

- **`modernc.org/sqlite` and `modernc.org/libc` move together, in one commit.** sqlite ships a transpiled C runtime whose ABI is coupled to the one exact libc revision its own `go.mod` pins; a `require` line is a floor, not an equality, so MVS accepts a higher libc that tidies, verifies, builds, and then faults at run time (upstream reports SIGBUS in the WAL index under concurrent access — gitlab.com/cznic/sqlite#177). `make lockstep` asserts the equality, in the `tidy` job and before every release. The `sqlite-currency` job in `vuln.yml` reports a lagging embedded SQLite as a warning annotation and still passes: upstream patch releases are outside this repo's control, and a job that sits red for weeks is a job nobody reads. Check the SQLite release notes for security content when it warns.
- **Attribution is generated, not hand-written.** `THIRD_PARTY_LICENSES.txt` is produced by `make notices` from the modules the two released binaries link, unioned across all six release targets, and it is drift-gated in CI. Adding, removing, or bumping a dependency means regenerating it. Every channel ships it: release archives (hence also the Homebrew cask, Scoop, and WinGet), deb/rpm/apk under `/usr/share/doc/zotio/`, the MCPB bundles, and the container image.

No advisory database covers most of this module set, so `vuln.yml` runs both `govulncheck` (Go module paths) and OSV-Scanner (GHSA and other feeds). Neither can see a SQLite CVE; that is what the currency job is for.

## Zotero API Surface

Missable invariants before you touch endpoints, schema, or mutations. Full coverage
matrix, known gaps, and the **refresh procedure** live in `dev/zotero-api-coverage.md`
— re-run it when a new Zotero version ships (releases are now every 6–10 weeks).

- This CLI targets Zotero's **local API** (`http://localhost:23119/api`, library base ends `/users/0`), which mirrors Web API v3 plus local-only extras. Enable it in Zotero: Settings → Advanced → "Allow other applications…".
- **Local API is GET-only** (writes "coming" as of 2026-06). When the base URL is local, mutating commands **auto-route writes to the Web API** if an `api_key` is set (reads stay local; user ID resolved via `keys/current`, cached as `user_id`/`ZOTERO_USER_ID`); a one-time stderr notice names the target. With **no** key, writes hit the read-only guard (`classifyAPIError`/`isLocalWriteRejection`). `doctor` reports writability under `writes:`. Web API writes sync down to the desktop; nothing writes the local DB directly.
- **Stored attachment bytes are a separate routing problem from item writes.** A Web API stored upload always lands in **Zotero's own cloud storage** and bills the account's plan, but Zotero's file store is a client-side setting the API never reports (`sync.storage.protocol = webdav` → personal-library files go to WebDAV; group libraries are always Zotero cloud, and *mode* is separate from *enabled*). The connector is **not** an escape hatch: `POST /connector/saveAttachment` resolves `parentItemID` only through its own save session (`SaveSession.getItemByConnectorKey`), so an **existing** item key is unaddressable (`500` live, `400 SESSION_NOT_FOUND` otherwise, verified 2026-08-17 against Zotero 7). zotio therefore **refuses** such uploads when the desktop keeps files elsewhere (`internal/cli/file_storage_guard.go`, precondition `zotero_file_storage`, `--allow-zotero-cloud` to override; `doctor` reports `file_storage:`). This is **best-effort, not fail-closed**: `prefs.js` is flushed lazily and can lag the running app in either direction, and Zotero's `-P` switch means the default profile may not be the running one (several profiles → hazards are unioned across all of them and the result marked ambiguous; pin with `ZOTERO_PROFILE_DIR`). Scope comes from the **resolved write target**: `--group` wins, a **local** base is always personal (hybrid routing captures only `flags.group`, so a local `/groups/<id>` read base still writes to `/users/<id>`), and only a **remote** base is classified by its own trailing library segment. There are exactly **two** local routes, both going through one connector session so the bytes reach the desktop's own file store. For a **new** item: `import apply --attach-mode stored --via connector`, which creates item and file together. For an item that **already exists**: `attachments add <key> <file> --via connector`, which creates a temporary parent plus the file in one session, moves the attachment with a single `PATCH {"parentItem": …}` guarded by `If-Unmodified-Since-Version`, then trashes the temporary parent. Re-parenting relocates no bytes: every storage name (local directory, upload zip, and both WebDAV remote names) derives from the **attachment's** own key, never its parent's — verified against Zotero's server and client source, then measured live on 2026-08-22 (`dev/field-report-2026-08-22-papio-round2.md`). That route is **opt-in**: `--via auto` still refuses, because it creates and trashes an item in the operator's library. It reconciles by **content hash**, not filename, because Zotero renames a saved file after its parent item's title at save time. Read `internal/zoteroprefs` before touching this.
- **Schema/type endpoints are global** (`/api/itemTypes`, `/itemFields`, `/itemTypeFields`, `/itemTypeCreatorTypes`, `/creatorFields`), NOT under the `/users|groups/<id>` prefix. The generated `schema *` commands keep the prefix and **404** live; `schema drift` strips it (`stripLibraryPrefix`). Mirror that if you fix them.
- New endpoints are implemented as hand-written commands; the endpoint coverage matrix lives in `dev/zotero-api-coverage.md`.
- Web API v3 is stable/versioned; the **local API is the evolving surface** (e.g. `/fulltext`, Jan 2025). New Zotero releases mostly add fields/data, rarely endpoints — run `zotio schema drift` to catch type/field deltas after an upgrade. The per-version "Zotero N for Developers" pages are Mozilla-migration guides, not API references; beta changelogs are unpublished (use the GitHub commit log).

## MCP Surface

The MCP surface **is** the CLI tree: `zotio-mcp` runs Cobra commands in-process via the `command_search`/`command_run` facade (or per-command mirror, `ZOTIO_MCP_SURFACE=mirror`). New functionality = a CLI command; it is auto-exposed over MCP with the same behavior and write gates. **Never add spec-derived typed MCP tools** — that parallel surface was retired for drifting behind CLI fixes (ADR-0003). The only hand-written MCP tools are the framework trio `context`/`search`/`sql`, plus resources and prompts.

## Architecture Decisions

Non-trivial architecture/infrastructure decisions (as opposed to product sequencing, which lives in `dev/roadmap.md`) are recorded as ADRs under `dev/adr/`. Read the relevant ADR before reworking the subsystem it covers.

- `dev/adr/0001-mcp-command-surface.md` — why the MCP server defaults to a command-orchestration facade (`command_search`/`command_run`) with global flags stripped from the mirror, and how to switch surfaces via `ZOTIO_MCP_SURFACE`.
- `dev/adr/0002-local-read-parity-subsystem.md` — why Zotero-aware local read parity (`internal/store/query.go` + the `resolveLocal*` path) is a deliberate, per-resource subsystem grown on demand, NOT a generic query-planner layer; read before adding a new `--data-source local` scope.
- `dev/adr/0004-no-zotero-plugin-exception-tags.md` — why there is no Zotero plugin: papio exception state surfaces as two reconciled automatic tags (`papio:needs-action`, `papio:unavailable`) written through `items tags add --automatic`, personal library only; read before adding any papio-facing tag or plugin surface.
- `dev/adr/0005-single-writer-concurrency-contract.md` — the cross-platform multi-reader/single-writer contract for installation state and independent outputs; read before changing a write path.
- `dev/adr/0006-unbound-profile-evidence.md` — why the stored-upload storage guard reads Zotero `prefs.js` machine-wide and does NOT bind that evidence to the account a command targets (no user ID in prefs; `sync.server.username` is an email while `keys/current` returns a username; `zotero.sqlite` is lock-contended while Zotero runs); read before trying to make the guard account-aware.
