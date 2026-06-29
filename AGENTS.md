# Zotero Printed CLI Agent Guide

This directory is a generated `zotero-pp-cli` printed CLI. It was produced by [CLI Printing Press](https://github.com/mvanhorn/cli-printing-press), so treat systemic fixes as upstream Printing Press fixes first. Keep local edits narrow and document why a generated-tree patch belongs here.

## Local Operating Contract

Start by asking the generated CLI for current runtime truth:

```bash
zotero-pp-cli doctor --json
zotero-pp-cli agent-context --pretty
```

Use runtime discovery instead of relying on a copied command list:

```bash
zotero-pp-cli which "<capability>" --json
zotero-pp-cli <command> --help
```

Add `--agent` to command invocations for JSON, compact output, non-interactive defaults, no color, and confirmation-safe scripting:

```bash
zotero-pp-cli <command> --agent
```

Before running an unfamiliar command that may mutate remote state, inspect its help and prefer a dry run:

```bash
zotero-pp-cli <command> --help
zotero-pp-cli <command> --dry-run --agent
```

Use `--yes --no-input` only after the target, arguments, and side effects are clear.

For install, auth, examples, and longer product guidance, read `README.md` and `SKILL.md`. This file intentionally stays small so repo-local agents get invariant local guidance without duplicating the generated docs.

## Zotero API Surface

Missable invariants before you touch endpoints, schema, or mutations. Full coverage
matrix, known gaps, and the **refresh procedure** live in `docs/zotero-api-coverage.md`
— re-run it when a new Zotero version ships (releases are now every 6–10 weeks).

- This CLI targets Zotero's **local API** (`http://localhost:23119/api`, base in `spec.yaml` ends `/users/0`), which mirrors Web API v3 plus local-only extras. Enable it in Zotero: Settings → Advanced → "Allow other applications…".
- **Local API is GET-only** (writes "coming" as of 2026-06). When the base URL is local, mutating commands **auto-route writes to the Web API** if an `api_key` is set (reads stay local; user ID resolved via `keys/current`, cached as `user_id`/`ZOTERO_USER_ID`); a one-time stderr notice names the target. With **no** key, writes hit the read-only guard (`classifyAPIError`/`isLocalWriteRejection`). `doctor` reports writability under `writes:`. Web API writes sync down to the desktop; nothing writes the local DB directly.
- **Schema/type endpoints are global** (`/api/itemTypes`, `/itemFields`, `/itemTypeFields`, `/itemTypeCreatorTypes`, `/creatorFields`), NOT under the `/users|groups/<id>` prefix. The generated `schema *` commands keep the prefix and **404** live; `schema drift` strips it (`stripLibraryPrefix`). Mirror that if you fix them.
- `spec.yaml` came from a community OpenAPI spec, not a live probe — coverage = that spec + hand-written commands. New endpoints never appear on their own; add them to `spec.yaml` (regen) or as a `// PATCH:` command.
- Web API v3 is stable/versioned; the **local API is the evolving surface** (e.g. `/fulltext`, Jan 2025). New Zotero releases mostly add fields/data, rarely endpoints — run `zotero-pp-cli schema drift` to catch type/field deltas after an upgrade. The per-version "Zotero N for Developers" pages are Mozilla-migration guides, not API references; beta changelogs are unpublished (use the GitHub commit log).

## Local Customizations

If you modify this CLI beyond what the generator produced, record each customization so it isn't lost on the next regen and is visible to the next reader.

1. **Mark every changed site** in source with a comment summarizing the deviation:

    ```
    // PATCH: <one-line summary>
    ```

    Include an upstream reference inline when there is one (e.g. `// PATCH(upstream cli-printing-press#<issue>): ...`). `grep -rn 'PATCH' .` from this directory then surfaces every customization.

2. **Catalog the change** in a `.printing-press-patches.json` at this CLI's root (parallel to `.printing-press.json`). Minimum shape:

    ```json
    {
      "schema_version": 1,
      "applied_at": "YYYY-MM-DD",
      "base_run_id": "<copy from .printing-press.json>",
      "base_printing_press_version": "<copy from .printing-press.json>",
      "patches": [
        {
          "id": "short-identifier",
          "summary": "What changed (one sentence).",
          "reason": "Why this customization was needed (one or two sentences).",
          "files": ["internal/cli/foo.go"],
          "validated_outcome": "Optional: non-obvious test result that confirms the fix.",
          "upstream_issue": "Optional: https://github.com/mvanhorn/cli-printing-press/issues/<n>"
        }
      ]
    }
    ```

This file is an **index of customizations**, not a second copy of the diff. Diffs live in `git`; code lives in the source files; the inline `// PATCH:` comment carries the local semantics. Keep `summary` and `reason` short -- if you find yourself writing tables of field renames or code transformations, that detail belongs in the source comment or commit message, not here.

## Architecture Decisions

Non-trivial architecture/infrastructure decisions (as opposed to product sequencing, which lives in `docs/roadmap.md`) are recorded as ADRs under `docs/adr/`. Read the relevant ADR before reworking the subsystem it covers.

- `docs/adr/0001-mcp-command-surface.md` — why the MCP server defaults to a command-orchestration facade (`command_search`/`command_run`) with global flags stripped from the mirror, and how to switch surfaces via `PP_MCP_SURFACE`.
