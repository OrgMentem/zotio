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

Before cutting a release (tagging `v*`), read `notes/releasing.md` — release flow, version/breaking-change decisions, validation checklist, and footguns. When the release coordinates with papio (the acquisition-side sister project), also read `~/@dev/papio/.agents/skills/papio-release/SKILL.md`: papio enforces a minimum-zotio-version floor and its release order depends on whether that floor moved.

## Zotero API Surface

Missable invariants before you touch endpoints, schema, or mutations. Full coverage
matrix, known gaps, and the **refresh procedure** live in `notes/zotero-api-coverage.md`
— re-run it when a new Zotero version ships (releases are now every 6–10 weeks).

- This CLI targets Zotero's **local API** (`http://localhost:23119/api`, base in `spec.yaml` ends `/users/0`), which mirrors Web API v3 plus local-only extras. Enable it in Zotero: Settings → Advanced → "Allow other applications…".
- **Local API is GET-only** (writes "coming" as of 2026-06). When the base URL is local, mutating commands **auto-route writes to the Web API** if an `api_key` is set (reads stay local; user ID resolved via `keys/current`, cached as `user_id`/`ZOTERO_USER_ID`); a one-time stderr notice names the target. With **no** key, writes hit the read-only guard (`classifyAPIError`/`isLocalWriteRejection`). `doctor` reports writability under `writes:`. Web API writes sync down to the desktop; nothing writes the local DB directly.
- **Schema/type endpoints are global** (`/api/itemTypes`, `/itemFields`, `/itemTypeFields`, `/itemTypeCreatorTypes`, `/creatorFields`), NOT under the `/users|groups/<id>` prefix. The generated `schema *` commands keep the prefix and **404** live; `schema drift` strips it (`stripLibraryPrefix`). Mirror that if you fix them.
- `spec.yaml` is retained as API-coverage reference data only. New endpoints are implemented as hand-written commands.
- Web API v3 is stable/versioned; the **local API is the evolving surface** (e.g. `/fulltext`, Jan 2025). New Zotero releases mostly add fields/data, rarely endpoints — run `zotio schema drift` to catch type/field deltas after an upgrade. The per-version "Zotero N for Developers" pages are Mozilla-migration guides, not API references; beta changelogs are unpublished (use the GitHub commit log).

## MCP Surface

The MCP surface **is** the CLI tree: `zotio-mcp` runs Cobra commands in-process via the `command_search`/`command_run` facade (or per-command mirror, `ZOTIO_MCP_SURFACE=mirror`). New functionality = a CLI command; it is auto-exposed over MCP with the same behavior and write gates. **Never add spec-derived typed MCP tools** — that parallel surface was retired for drifting behind CLI fixes (ADR-0003). The only hand-written MCP tools are the framework trio `context`/`search`/`sql`, plus resources and prompts.

## Architecture Decisions

Non-trivial architecture/infrastructure decisions (as opposed to product sequencing, which lives in `notes/roadmap.md`) are recorded as ADRs under `notes/adr/`. Read the relevant ADR before reworking the subsystem it covers.

- `notes/adr/0001-mcp-command-surface.md` — why the MCP server defaults to a command-orchestration facade (`command_search`/`command_run`) with global flags stripped from the mirror, and how to switch surfaces via `ZOTIO_MCP_SURFACE`.
- `notes/adr/0002-local-read-parity-subsystem.md` — why Zotero-aware local read parity (`internal/store/query.go` + the `resolveLocal*` path) is a deliberate, per-resource subsystem grown on demand, NOT a generic query-planner layer; read before adding a new `--data-source local` scope.
