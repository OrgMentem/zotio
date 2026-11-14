# zotio Agents Guide

This repo was bootstrapped by [CLI Printing Press](https://github.com/mvanhorn/cli-printing-press) in 2026-05, but it has been hand-maintained since. The generator was retired on 2026-07-08: there is no regeneration path, and systemic fixes no longer route upstream first.

## Local Operating Contract

Ask the CLI for current runtime state:

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

For install, auth, examples, and product guidance, read `README.md` and `SKILL.md`.

For CI, the [zotio-action](https://github.com/OrgMentem/zotio-action) GitHub Action ([marketplace](https://github.com/marketplace/actions/zotio-bibliography-health-for-zotero)) packages install → sync → gate on `library health` exit codes; see `docs/guide/ci.md`.

Before cutting a release (tagging `v*`), read `dev/releasing.md` — release flow, version/breaking-change decisions, validation checklist, and footguns. When the release coordinates with papio (the acquisition-side sister project), also read `~/@dev/papio/.agents/skills/papio-release/SKILL.md`: papio enforces a minimum-zotio-version floor and its release order depends on whether that floor moved.

## Task-specific contracts

Read these references before changing the named surface. Paths in code spans remain repository-root-relative.
Correct instruction conflicts within the task. Report unrelated conflicts without expanding the task.

- Dependency changes: read [Dependency contracts](dev/dependency-contracts.md) before updating modules or attribution.
  Keep `modernc.org/sqlite` and `modernc.org/libc` in lockstep and regenerate `THIRD_PARTY_LICENSES.txt`.
- Endpoints, schema, imports, attachments, or mutations: read [API routing contracts](dev/api-routing-contracts.md).
  Local reads do not imply local writes. Web writes can reach Zotero cloud; never write the local DB directly.
  Stored uploads can bill the account. Preserve the storage guard and explicit connector opt-in.
- CLI commands or MCP changes: read [MCP contracts](dev/mcp-contracts.md).
  The CLI tree defines MCP behavior and write gates; never add a parallel typed endpoint surface.

## Architecture Decisions

Non-trivial architecture/infrastructure decisions are ADRs under `dev/adr/`; product sequencing lives in `dev/roadmap.md`. Read the relevant ADR before reworking its subsystem.

- `dev/adr/0001-mcp-command-surface.md` — why the MCP server defaults to a command-orchestration facade (`command_search`/`command_run`) with global flags stripped from the mirror, and how to switch surfaces via `ZOTIO_MCP_SURFACE`.
- `dev/adr/0002-local-read-parity-subsystem.md` — why Zotero-aware local read parity (`internal/store/query.go` + the `resolveLocal*` path) is a deliberate, per-resource subsystem grown on demand, NOT a generic query-planner layer; read before adding a new `--data-source local` scope.
- `dev/adr/0004-no-zotero-plugin-exception-tags.md` — why there is no Zotero plugin: papio exception state surfaces as two reconciled automatic tags (`papio:needs-action`, `papio:unavailable`) written through `items tags add --automatic`, personal library only; read before adding any papio-facing tag or plugin surface.
- `dev/adr/0005-single-writer-concurrency-contract.md` — the cross-platform multi-reader/single-writer contract for installation state and independent outputs; read before changing a write path.
- `dev/adr/0006-unbound-profile-evidence.md` — why the stored-upload storage guard reads Zotero `prefs.js` machine-wide and does NOT bind that evidence to the account a command targets (no user ID in prefs; `sync.server.username` is an email while `keys/current` returns a username; `zotero.sqlite` is lock-contended while Zotero runs); read before trying to make the guard account-aware.
- `dev/adr/0007-permanent-delete-tombstones.md` — why a permanent delete leaves a deletion marker in `pending_writes` rather than a separate tombstone table: the delete lands on the Web API while sync reads the local desktop plane, which has no `/deleted` feed, so a sync inside that lag window would otherwise re-insert the item; read before changing the reap, `reconcilePendingWrites`, or `SweepMissing`.
- `dev/adr/0008-batched-item-writes.md` — why batched multi-object writes are opt-in per command (`--batch`), why per-item conflict attribution survives batching but fail-fast does not, and why planning reads are always batched (one list request per 50 keys) while writes batch only on `--batch`; read before batching another command or changing `internal/cli/batch_item_update.go`.
