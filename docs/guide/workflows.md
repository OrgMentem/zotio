# Workflows & triggers

A **workflow** is a declarative list of `zotio` steps run through one transactional envelope: one preview, one approval, one journal run — with inter-step data flow, conditionals, and resume. It is the automation counterpart to [safe-by-default writes](../concepts/write-safety.md): the same preview-first, one-`--yes` contract, extended from a single command to a whole plan.

Reach for it when a task is more than one command — *diagnose, then fix*, *export, then verify*, *enrich, then re-audit* — and you want it to preview as a unit, apply on a single approval, and be safe to re-run.

## The spec

A workflow is a JSON file: optional `vars`, an optional `continue_on_error`, and an ordered list of `steps`. Each step is a named `zotio` argument vector.

```json
{
  "vars": { "PROJECT": "thesis" },
  "steps": [
    { "name": "diagnose", "args": ["library", "health", "--json"] },
    { "name": "fix", "args": ["items", "enrich", "--keys-from", "-"], "stdin_from": "diagnose" }
  ]
}
```

## Preview by default, one `--yes` to apply

```bash
zotio workflow run workflow.json          # preview: mutating steps run --dry-run, read-only steps run for real
zotio workflow run workflow.json --yes    # apply the whole workflow on a single approval
```

- **Preview is the default.** Without `--yes`, every mutating step is forced to `--dry-run`; read-only steps run normally so the plan reflects real data. `--dry-run` always wins, even alongside `--yes`.
- **One approval owns the run.** A single `--yes` on `workflow run` applies every step. Specs that embed their own `--yes`/`--dry-run` in a step's args are rejected — the workflow owns approval, not the steps.
- **One journal run.** Every mutation applied under that approval shares one workflow run ID. Filter a run's entries with `zotio journal list --workflow <id>`; reverse the reversible ones with `zotio journal undo <run-id>` as with any other write.

## Variables

Declare `vars` and reference them as `${vars.NAME}` in any step argument; override per run with repeatable `--var`:

```bash
zotio workflow run workflow.json --var PROJECT=demo
```

An undeclared `--var` name is rejected (typo guard). A `${...}` placeholder is *data* — it can fill a flag's value (`--tag=${vars.T}`) but can never construct a flag name or redirect the step to a different command.

## Data flow between steps

A named step's output is addressable downstream:

- `${steps.NAME.output}` — the trimmed stdout of an earlier step, substituted into a later step's arguments.
- `"stdin_from": "NAME"` — pipe an earlier step's raw stdout into this step's stdin.

That is what makes *diagnose → fix* one workflow: `library health --json` emits a remediation plan on stdout, and `items enrich --keys-from -` consumes it via `stdin_from`. Only stdout flows into data; stderr never does. In preview mode the substituted values are the *preview* outputs.

## Conditionals

Run a step only on an earlier step's outcome with `when`:

```json
{ "name": "notify", "args": ["...", "..."], "when": { "step": "fix", "is": "failed" } }
```

`is` is one of `ok`, `failed`, or `skipped`. By default a failed step stops the run; set `"continue_on_error": true` to let the workflow proceed so `when` branches (e.g. a cleanup or notify step) can react.

## Interrupted runs resume

An applied run records a checkpoint sidecar (`<spec>.checkpoint.json`) as it goes. If it is interrupted, continue where it stopped:

```bash
zotio workflow run workflow.json --yes --resume
```

Resume is spec-hash- and variable-verified: it refuses if the spec or the resolved `--var` set changed since the checkpoint, and a step whose completion is uncertain is **not** silently replayed. Re-running `--yes` while a checkpoint exists is refused — resume it, or delete the sidecar to start over. A successful run removes the sidecar.

## Triggers

Run a workflow automatically when the library changes, by attaching it to the sync loop:

```bash
zotio watch --workflow workflow.json          # after every successful sync cycle
zotio tail  --workflow workflow.json          # once after a poll cycle that emitted change events
```

`watch --workflow` fires after each successful sync; `tail --workflow` fires only on cycles that saw events (quiet when nothing changed). Triggered runs follow the same rule: they **preview unless the `watch`/`tail` invocation itself carries `--yes`**. A trigger failure is logged but never stops the loop, and a failed *applied* trigger leaves its checkpoint — later applied triggers refuse until you resume or delete it:

```bash
zotio workflow run workflow.json --yes --resume
```

## From an agent — `workflow_submit`

Over MCP, an agent submits a workflow inline through the dedicated `workflow_submit` tool rather than the local-file `workflow run` runner (which stays CLI-only). Each submitted step names a mirrorable command and is validated against the **same per-command safe-flag allowlist as `command_run`**, then executed through the same transactional runner — previewing unless the submission sets `yes`. See the [MCP server guide](mcp-server.md) and the [MCP tools reference](../reference/mcp-tools.md).

## See also

- [Safe-by-default writes](../concepts/write-safety.md) — the mutation contract workflows extend.
- [Command reference › `zotio workflow`](../reference/commands.md) — every flag, generated from the binary.
