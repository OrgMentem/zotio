# Workflows & triggers

A **workflow** chains several `zotio` steps into one reviewed run: one preview, one approval, and a shared run ID for journaled writes — with data flowing between steps, conditionals, and resume. It is the automation counterpart to [safe-by-default writes](../concepts/write-safety.md): the same preview-first, one-`--yes` contract, stretched from a single command to a whole plan. Steps run in order with no rollback, so a later failure leaves earlier successful writes applied.

Without a workflow: run five commands, approve five times, and hope you didn't skip one. With: one preview, one `--yes`, and journaled mutations filed under one workflow run ID. Vault steps use their own reports instead of journal entries.

## What you'd use it for

**Fill missing DOIs in one reviewed pass.** Check the library, then enrich the missing-DOI queue in one scope — as a single operation you approve once:

```json
{ "steps": [
  { "name": "diagnose", "args": ["library", "health", "--json"] },
  { "name": "fix", "args": ["items", "enrich", "--missing-doi", "--scope", "collection:ABCD1234"] }
]}
```
```bash
zotio workflow run fix-dois.json          # see exactly what it would change
zotio workflow run fix-dois.json --yes    # apply it all on one approval
```

The diagnose step reports the gaps; the fix step selects its own missing-DOI queue inside the scope rather than consuming the health output. `items enrich` requires at least one category flag; select the matching flags for other gaps (`--missing-abstract`, `--missing-pdf`, and the rest). The preview shows the whole plan; one `--yes` runs it.

**Prove a bibliography is submission-ready.** Chain the checks that gate a paper so one command says pass or fail. Record the baseline first with `zotio export snapshot --output corpus.jsonl`, which writes the `corpus.jsonl.manifest.json` sidecar the verify step reads:

```json
{ "steps": [
  { "args": ["items", "bibcheck", "thesis.tex"] },
  { "args": ["export", "snapshot", "verify", "corpus.jsonl.manifest.json", "--fail-on-drift"] }
]}
```

**Keep the library tidy automatically.** Attach a hygiene workflow to the sync loop — it runs when the library changes and stays quiet when it doesn't:

```bash
zotio watch --workflow nightly-hygiene.json --workflow-on-change
```

!!! tip "Just one command?"
    Don't wrap it — run it directly. Workflows earn their keep when steps *feed each other* or must *apply together*.

## Preview, then apply

```bash
zotio workflow run workflow.json          # preview: mutating steps run --dry-run, read-only steps run for real
zotio workflow run workflow.json --yes    # apply the whole workflow on a single approval
```

Without `--yes`, every mutating step is forced to `--dry-run` while read-only steps run normally, so the plan reflects real data. A single `--yes` applies every step, and every mutation in the run shares one journal run ID. There is no rollback: if a later step fails, earlier successful writes stay applied.

```bash
zotio journal list --workflow <id>    # everything that run changed
zotio journal undo <run-id>           # reverse the reversible parts, as with any write
```

`journal undo` reverses only the reversible operations and reports the rest as refused, so review the run's journal entries before assuming everything came back.

!!! note "Rules the runner enforces"
    - `--dry-run` always wins — even alongside `--yes`, the run previews.
    - The workflow owns approval: steps that embed their own `--yes`/`--dry-run` are rejected.
    - A `${...}` placeholder is *data* — it fills a flag's value but can't build a flag name or redirect a step to another command.

## How it works

A workflow is a JSON file: an optional `vars` map, an optional `continue_on_error`, and an ordered list of `steps`. Each step is a named `zotio` argument vector, optionally with `stdin_from` (plus `stdin_select`), `stdin_trigger`, and `when`.

### Variables

Declare `vars` and reference them as `${vars.NAME}` in any step argument; override per run with repeatable `--var`:

```bash
zotio workflow run workflow.json --var PROJECT=demo
```

An undeclared `--var` name is rejected (a typo guard).

### Data flow between steps

A named step's output is addressable downstream:

- `${steps.NAME.output}` — the trimmed stdout of an earlier step, substituted into a later step's arguments.
- `${steps.NAME.json:PATH}` — one string, number, or boolean selected from an earlier step's JSON output, substituted into a later step's arguments.
- `"stdin_from": "NAME"` — pipe an earlier step's raw stdout into this step's stdin.
- `"stdin_from": "NAME", "stdin_select": "PATH"` — pipe the values selected from that step's JSON output instead, one per line. This is the form `--keys-from -` reads.

That is how one step's findings become the next step's input. Only stdout flows into data; stderr never does. In preview mode the substituted values are the *preview* outputs.

#### Selecting JSON fields

A selector `PATH` walks the step's JSON output. Separate fields with dots. After a field, brackets work on an array:

| Selector part | Selects |
| --- | --- |
| `result.key` | the `key` field of the `result` object |
| `findings[0]` | element 0 of the `findings` array |
| `findings[]` | every element of the array |
| `findings[kind=missing_doi]` | the elements whose `kind` field equals `missing_doi` |
| `[0].key` | the `key` of element 0 when the output itself is an array |

Field names use letters, digits, `_`, and `-`. A selector has at most 256 characters and 32 parts. This workflow enriches only the items that `library health` reports as missing a DOI:

```json
{ "steps": [
  { "name": "diagnose", "args": ["library", "health", "--json"] },
  { "name": "fix", "args": ["items", "enrich", "--missing-doi", "--keys-from", "-"],
    "stdin_from": "diagnose", "stdin_select": "findings[kind=missing_doi].item_key" }
]}
```

The runner checks every selection and fails closed:

- The step fails if the output is not exactly one JSON value, a field is missing, an index is out of range, or a value has the wrong type.
- An argument takes exactly one string, number, or boolean. A selector that can return a list (`[]` or `[field=value]`) is rejected when the spec loads.
- `stdin_select` writes each selected string, number, or boolean on its own line, and a selected array contributes its elements. Objects, `null`, and values with line breaks fail the step.
- A selection with no values skips the step (reason `stdin_select selected no values`). The step never gets an empty key list.
- A selected value fills an argument value only. The runner checks the command and the flag names after substitution, so a value such as `--yes` or a subcommand name is refused.

The checkpoint stores each step's raw output, so `--resume` selects the same values again.

### Conditionals

Run a step only on an earlier step's outcome with `when`:

```json
{ "name": "notify", "args": ["...", "..."], "when": { "step": "fix", "is": "failed" } }
```

`is` is one of `ok`, `failed`, or `skipped`. By default a failed step stops the run; set `"continue_on_error": true` so the workflow proceeds and `when` branches (a cleanup or notify step) can react.

## Interrupted runs resume

An applied run records a checkpoint sidecar (`<spec>.checkpoint.json`) as it goes. If it's interrupted, continue where it stopped:

```bash
zotio workflow run workflow.json --yes --resume
```

Resume is spec-hash- and variable-verified: it refuses if the spec or the resolved `--var` set changed since the checkpoint, and a step whose completion is uncertain is **not** silently replayed. A run started by a trigger also keeps its change batch in the checkpoint, and resume uses that batch. Re-running `--yes` while a checkpoint exists is refused — resume it, or delete the sidecar to start over. A successful run removes the sidecar.

## Triggers

Run a workflow automatically when the library changes by attaching it to the sync loop:

```bash
zotio watch --workflow workflow.json          # after every successful sync cycle
zotio watch --workflow workflow.json --workflow-on-change   # only after a cycle that changed the mirror
zotio tail  --workflow workflow.json          # once after a poll cycle that emitted change events
```

`watch --workflow` fires after each successful sync; `tail --workflow` fires only on cycles that saw events (quiet when nothing changed). Triggered runs follow the same contract: they **preview unless the `watch`/`tail` invocation itself carries `--yes`**. A trigger failure is logged but never stops the loop, and a failed *applied* trigger leaves its checkpoint — later applied triggers refuse until you resume or delete it (`zotio workflow run workflow.json --yes --resume`).

Add `--workflow-on-change` (it requires `--workflow`) to run the workflow only after a cycle that changed the local mirror. A change is a row that sync stored, rewrote with different content, or reaped because the object is gone upstream (an erased collection or a purged trash row). Tags and schema rows that come back unchanged are not changes. An unchanged cycle logs `workflow skipped: library unchanged`. A cycle whose sync failed, or in which any resource failed, never runs the workflow; its changes wait for the next complete cycle.

### Changed keys from `tail`

A workflow that `tail` starts can read the change batch of that poll cycle. The batch is this JSON document:

```json
{ "source": "tail", "resource": "items",
  "events": [ { "event": "upsert", "resource": "items", "key": "AAAA1111" },
              { "event": "delete", "resource": "items", "key": "DDDD4444" } ],
  "upsert_keys": ["AAAA1111"],
  "delete_keys": ["DDDD4444"] }
```

Read it with the same selectors: `${trigger.json:PATH}` in an argument, or `"stdin_trigger": "PATH"` for stdin. `"stdin_trigger": "upsert_keys"` feeds `--keys-from -` with only the changed records:

```json
{ "steps": [
  { "name": "tag-new", "args": ["items", "tags", "add", "--tag", "to-read", "--keys-from", "-"],
    "stdin_trigger": "upsert_keys" }
]}
```

```bash
zotio tail items --workflow tag-new.json          # preview each change batch
zotio tail items --workflow tag-new.json --yes    # apply each change batch
```

- `upsert_keys` lists each upserted key once. Deleted keys are only in `delete_keys`, also when the same key was upserted earlier in the batch.
- A batch without upserts skips the `upsert_keys` step (reason `stdin_trigger selected no values`). The step never runs with an empty key list, so it cannot select the whole library.
- The batch is data. It never approves a write: the run previews unless `tail` itself has `--yes`.
- The first poll (no stored cursor) lists the current records, not changes. `tail` skips a workflow that reads trigger values on that poll and logs `workflow skipped`.
- A cycle with more than 500 events also skips that workflow and logs the count. A partial key list would leave changed records out, so the runner does not cut the batch. The cursor still advances; run the workflow over an explicit scope to cover those records.
- `stdin_trigger` cannot be combined with `stdin_from`.
- A workflow that reads no trigger values runs exactly as before, on every cycle with events.
- A spec that reads trigger values cannot run without a batch: `zotio workflow run` refuses it with exit code 9. `watch --workflow` passes no batch, so such a spec fails there too. Only `--yes --resume` of an interrupted triggered run can continue it, with the stored batch.

## From an agent — `workflow_submit`

Over MCP, an agent submits a workflow inline through the dedicated `workflow_submit` tool rather than the local-file `workflow run` runner (which stays CLI-only). Each submitted step names a mirrorable command and is validated against the **same per-command safe-flag allowlist as `command_run`**, then executed through the same preview-first runner — previewing unless the submission sets `yes`. Steps accept `stdin_from` with `stdin_select`, and the runner validates the selector exactly as for a local spec. See the [MCP server guide](mcp-server.md) and the [MCP tools reference](../reference/mcp-tools.md).

## See also

- [Safe-by-default writes](../concepts/write-safety.md) — the mutation contract workflows extend.
- [Command reference › `zotio workflow`](../reference/commands.md) — every flag, generated from the binary.
