# ADR 0001 — MCP command surface: strip global flags + command-orchestration facade

- **Status:** Accepted — implemented in commit `4cb4fb9` (2026-06-29).
- **Scope:** `internal/mcp/` (the `zotio-mcp` server's tool surface).
- **Deciders:** enieuwy, with a GPT-5.5 Pro consult via Oracle (session `mcp-surface-trim`).

## Context

The MCP server registers, at connect time, three kinds of tools (see `internal/mcp/tools.go`):

1. **28 typed spec endpoints** (`collections_*`, `items_get/list`, `schema_*`, `tags_*`, …).
2. **3 framework tools** — `context`, `search`, `sql`.
3. **~68 cobratree-mirrored commands** — every user-facing Cobra command not classified
   hidden/endpoint/framework, mirrored one-tool-per-command by `cobratree.RegisterAll`.

Measured live (`tools/list` over HTTP): **99 tools, ~236 KB wire ≈ 59K tokens loaded at every
connect** — roughly a quarter of a 200K context window before any work. The cost decomposed as:

| Surface | Tools | ≈ Tokens | Share |
|---|---|---|---|
| Spec endpoints | 28 | 3.2K | 5% |
| Framework | 3 | 0.3K | <1% |
| **Cobratree commands** | **68** | **55.7K** | **94%** |

And within the cobratree mass, **44.6K (80%) was 22 inherited/global persistent flags re-declared on
every one of the 68 tools** (`agent`, `json`, `compact`, `continue-on-error`, `csv`, `data-source`,
`dry-run`, `human-friendly`, `idempotent`, `ignore-missing`, `max-changes`, `max-failures`,
`no-cache`, `no-color`, `no-input`, `plain`, `quiet`, `rate-limit`, `select`, `timeout`, `yes`,
`allow-destructive`). Genuine command-specific flags totalled only ~5K. Most of those 22 are
meaningless to an MCP caller: output formatting it never reads, confirmation the MCP layer handles,
and agent-mode the server already activates out-of-band.

The generator's only built-in anti-bloat lever (`mcp.orchestration: code`) collapses **only the typed
endpoints**; it still calls `cobratree.RegisterAll` unconditionally, so it would cut <5% here. Filed
upstream as [cli-printing-press#3374](https://github.com/mvanhorn/cli-printing-press/issues/3374). This
CLI is soft-decoupled from the generator, so the fix is local.

## Decision

Two coordinated changes, plus a runtime switch:

1. **F-plain — strip inherited/global flags from the command mirror.** A single shared enumerator
   `visitSafeMirrorFlags` (in `cobratree/walker.go`) emits **command-local (non-inherited) flags
   only**, and drives **both** schema exposure (`safeToolOptionsForFlags`) and the validation
   allowlist (`safeFlagNames`) so they can never diverge — preserving the `da7c6f88` arg-safety guard
   (`validateMirrorArguments`). **Exception (see Amendment 2026-06-29):** for **mutating** commands
   (`!mcp:read-only`) the enumerator also emits the six write-safety gate flags (`yes`, `dry-run`,
   `allow-destructive`, `max-changes`, `continue-on-error`, `max-failures`) from the inherited set, so
   applies remain reachable over MCP. They cost zero standing tokens on the default facade surface
   (delivered only in on-demand `command_search` detail).

2. **`--agent` injection in the exec core.** `runMirroredInProcess` (in `cobratree/shellout.go`)
   prepends `--agent` when the root defines it, so mirror tools always return structured,
   non-interactive output regardless of which flags the schema exposes. This is the out-of-band
   mechanism that makes dropping `--agent`/`--json`/formatting/confirmation flags safe.

3. **Command-orchestration facade, env-gated.** A new `cobratree.RegisterOrchestration` registers two
   tools — `command_search` (discovery: list/filter commands, or full per-command flag detail) and
   `command_run` (validated execution via the same guard + exec core). `internal/mcp/tools.go`
   selects the surface via **`PP_MCP_SURFACE`**: default **`facade`**; `mirror` falls back to the
   lean per-command mirror. `cobratree.RegisterAll` is **retained** (not deleted) as the fallback.

### Resulting surface (measured live)

| Mode | Tools | ≈ Tokens | vs. baseline |
|---|---|---|---|
| `facade` (default) | 33 | **~3.8K** | **−94%** |
| `mirror` (`PP_MCP_SURFACE=mirror`) | 99 | ~15.1K | −75% |
| pre-change (`git revert`) | 99 | ~59K | — |

## Consequences

- **Default connect cost drops ~94%** (59K → 3.8K). All 68 commands stay reachable on demand via
  `command_search` → `command_run`.
- **Rollback is a 3-position runtime switch:** `facade` (env unset) → `mirror` (`PP_MCP_SURFACE=mirror`,
  no rebuild) → pre-change (`git revert 4cb4fb9`). The facade is additive; `RegisterAll` is intact.
- **Security preserved.** `command_run` reuses `safeFlagNames` + `validateMirrorArguments`; forged
  global flags and raw flag tokens in positional args are rejected (covered by tests for all 22
  globals + the 4 hidden `config/deliver/group/profile`).
- **Tradeoff (accepted):** the 68 commands lose native, host-validated MCP `inputSchema` — their flag
  schema is delivered as `command_search` *text/JSON*, and a wrong flag fails at `command_run` time
  rather than at call construction. Catalog discovery is one-hop-amortized per session; per-command
  schema is fetched on demand. This is the standard search+execute cost; acceptable for a solo,
  token-sensitive surface. Switch to `mirror` for native one-hop schemas at ~15K.
- **Soft coupling:** anything pinned to the 68 individual tool names breaks under `facade` and returns
  under `mirror`/revert. Low risk for solo use.
- **Pre-existing (not introduced here):** commands that write to `os.Stdout` directly rather than
  `cmd.OutOrStdout()` (e.g. `capabilities`) aren't captured by the in-process handler — identical in
  both surfaces. Out of scope; tracked separately if it bites.

## Alternatives considered

| Option | ≈ Tokens | Why not |
|---|---|---|
| Status quo | 59K | The problem. |
| Reprint with `mcp.orchestration: code` | ~56K | Collapses only the 28 endpoints (#3374); a merge cost for <5%. |
| **F-plain alone** (no facade) | ~15K | Good, native, one-hop — kept as the `mirror` fallback, not the default. |
| F-surgical (keep `dry-run`/`allow-destructive` on mutating cmds) | ~17K | Oracle's first pick. **Originally rejected** ("not worth the standing tokens once `--agent` injection covers non-interactive needs") — but that premise was wrong: `--agent` does **not** imply `--yes`, so stripping the gate flags made mutating commands preview-only. **Partially adopted** in Amendment 2026-06-29 (gate flags kept for mutating cmds; on the facade they cost ~0 standing tokens, not 17K). |
| F + description trim | ~12.6K | Descriptions already lean (~43 tok/tool); ~2K for real selection-signal loss. |
| Hide cold commands (`mcp:hidden`) | ~31K (−47%) | Subtractive — hidden commands become unreachable; conflicts with classify.go's "underused < broken contract". |
| **F-plain + facade (chosen)** | **~3.8K** | Best token win; all commands reachable; reuses owned hardened machinery; trivially reversible. |

## Validation

- Oracle (GPT-5.5 Pro) consult validated the diagnosis (inherited persistent flags mirrored as
  per-tool schema; `safeFlagNames`/`safeToolOptionsForFlags` the precise lever) and the
  allowlist-over-denylist + validation==exposure design.
- Live `tools/list` for both surfaces; end-to-end: `command_search` lists 68 real commands,
  per-command detail exposes only local flags (no globals), `command_run library stats` returns
  structured JSON, forged globals + raw flag tokens rejected.
- Package tests in `cobratree/flagstrip_test.go` (all 22 globals + 4 hidden) and
  `cobratree/orchestrate_test.go` (facade behavior). `go build/vet/test ./...` and
  `golangci-lint run ./...` green.

## Amendment (2026-06-29) — restore write-safety gate flags for mutating commands

- **Problem:** F-plain (commit `4cb4fb9`) stripped **all** inherited globals, including the six
  write-safety gate flags. The original Decision assumed `--agent` injection "covers non-interactive
  needs", but `--agent` explicitly does **not** auto-apply writes (`root.go`: "does NOT auto-apply
  writes — pass --yes to mutate") and the apply gate is `Yes && !DryRun` (`mutation.ResolveMode`).
  Net effect: every cobra-only mutation workflow (`items enrich`, `items duplicates resolve`,
  `tags audit fix`, `tags rename`, `reading-list add`, `library health --fix` — none of which have a
  typed-tool equivalent) could only **preview** over MCP, never apply. A functional regression vs. the
  pre-`4cb4fb9` surface.
- **Fix:** `visitSafeMirrorFlags` now also emits the six gate flags (`yes`, `dry-run`,
  `allow-destructive`, `max-changes`, `continue-on-error`, `max-failures`) for **mutating** commands
  (`!isMCPReadOnly`). The single-enumerator invariant is preserved, so schema exposure, the validation
  allowlist, and `command_search` detail all gain them together — `command_run` accepts `{"yes": true}`
  for a mutating command and rejects it for a read-only one. Standing-token impact on the default
  `facade` surface is ~0 (gate flags appear only in on-demand detail); the `mirror` surface gains the
  six flags on mutating tools only (≈ the F-surgical estimate, not the full 22-flag bloat).
- **Tests:** `flagstrip_test.go` (accept on mutating / reject on read-only / formatting+hidden still
  rejected) and `orchestrate_test.go` (end-to-end: `--yes` propagates through `command_run` to a
  mutating command and applies; rejected on a read-only command; exposed in `command_search` detail).
- Patch-catalog entry: `mcp-facade-apply-gate-flags`.

## References

- Commit `4cb4fb9`; `.printing-press-patches.json` entry `mcp-command-surface-f-plain-and-facade`.
- Upstream: [cli-printing-press#3374](https://github.com/mvanhorn/cli-printing-press/issues/3374)
  (generator mirrors inherited persistent flags onto every tool) and #3373 (MCP path-param encoding).
- Security invariant: `da7c6f88` (`validateMirrorArguments` / `unsafeMCPMirrorFlags` / `safeFlagNames`).

## Amendments

- **2026-07-08:** `PP_MCP_SURFACE` was renamed to `ZOTIO_MCP_SURFACE` as part of retiring the CLI Printing Press generator; the old name was dropped outright the same day (pre-adoption, no fallback kept).
- **2026-07-08:** The `.printing-press-patches.json` catalog referenced above was removed with the generator retirement; the cited entries live in git history.
