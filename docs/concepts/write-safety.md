# Safe-by-default writes

Writes are preview-first. Every command that changes your Zotero library or your vault previews by default, applies only under `--yes`, and counts what it plans against `--max-changes`. That covers the bulk fixers and importers (`items enrich`, `tags audit fix`, `items duplicates resolve`, `import apply`, `import doi`, `import file`, `import url`, and the generic `import <resource>`), every single-item create (`items new`, `import pmid`, `import arxiv`, `import isbn`), the single-resource CRUD commands (`items create/update/delete/restore`, `collections create/update/delete`), and every vault command (`vault push`, `vault pull`, `vault sync`, `vault resolve`); `workflow run` extends the same contract across a multi-step plan. Read the contract once and you never have to guess which command is dangerous.

<div class="zotio-diagram-wrap">
--8<-- "docs/assets/write-safety.svg"
</div>

## The contract

- **Preview is the default.** You get a plan/result envelope with zero changes. `--yes` applies; `--dry-run` always wins. A preview runs the same reads as an apply, so it reports real no-ops and real versions, and it evaluates the same gates (`--max-changes`, `--allow-destructive`, input validation). The `→ writing via Zotero Web API` notice and any config save happen only on apply.
- **`--dry-run` suppresses command writes.** It suppresses Zotero writes, command output files and their `.lock` siblings, config saves, journal entries, and mirror write-through. Read commands run normally.
- **`--deliver` is separate output routing.** `--deliver file:<path>` or `webhook:<url>` sends the command's output to a sink. `--dry-run` does not suppress delivery: a file target still gets its lock and output.
  `--output` and `--deliver file:` must name different files; the same target is a usage error (exit 2).
- **`--agent` does *not* auto-apply.** Agent mode sets JSON + non-interactive defaults, but a write still needs an explicit `--yes`.
- **Every Zotero write is gated.** Every command that writes to Zotero or to the vault needs `--yes`, and `--dry-run` overrides it. A `workflow run` preview injects `--dry-run` into each step that supports it. Steps that cannot preview, such as `sync`, are skipped with the reason `not previewable; runs only on apply`. Commands that destroy local state (`auth logout`, `profile delete`) also preview by default and apply with `--yes`. Commands that create local state (`auth set-token`, `profile save`) apply directly. The vault commands keep their own per-note report rather than the generic envelope, but they obey the same gate.
- **Gates cap the blast radius.** `--max-changes` defaults to 500 (50 under `--agent`). Irreversible operations require `--allow-destructive`: merge, permanent delete, collection delete, `creators rename`, `creators audit fix`, and `vault resolve --keep-vault|--keep-remote`. `vault resolve --recreate` does not require it. On apply, a workflow step that declares `--allow-destructive` also requires that flag on `workflow run`; otherwise the invocation exits 9 before any step runs. Preview remains available without invocation-level consent.
  `--max-changes` below `-1` and negative `--max-failures` are usage errors (exit 2).
- **Stored uploads have a storage gate.** Web API uploads always use Zotero cloud storage. The `zotero_file_storage` precondition refuses unsafe routes, including personal-library uploads when the desktop uses WebDAV. `doctor` reports `file_storage:`. Use `--allow-zotero-cloud` only to accept that cloud destination explicitly.
- **Vault reports distinguish preview from dry run.** `preview` is true in every preview mode, including omission of `--yes`. `dry_run` is true only with an explicit `--dry-run`.
- **Read-your-writes.** A write applied through the mutation envelope is replayed into the local mirror immediately and the post-write item state comes back in the envelope, so a re-audit sees the fix with no follow-up `sync`. Replay is deliberately conservative: scalar field edits, tag/collection membership, item creates, trash, restores, and permanent deletes are replayed when the mirror can express them, while unsupported structural edits are left for the next `sync` to reconcile authoritatively. A mirror update that itself fails is reported as a warning without failing the already-applied write.
- **Journaled, and honest about undo.** Runs applied through the mutation envelope are recorded append-only (`journal list` / `journal show`), including the single-resource CRUD commands and partially-rejected batches (a 100-item create where one element is refused still journals the 99 that landed). Some gated writes deliberately sit outside the envelope and so are not journaled: the `vault` commands, which keep their own report. `journal undo <run-id>` reverses only what it can invert losslessly — tag and collection membership toggles, creates (reversed by trashing the created item), `items delete` trash and `items restore` runs (reversed by the opposite toggle; permanent deletes stay refused), and `items enrich` writes of a DOI, abstract, citation fields, or subject and MeSH tags — and **loudly refuses** everything else (merges, deletions, other field overwrites) rather than guessing. An enrich run records its own inverse while it writes: the value each field held, the tags it added, and the exact `zotio:` provenance line it appended to Extra. Undo takes that line out of the live Extra and leaves every other line alone; it restores a field only while the field still holds the value enrich wrote; and it still works when the line is already gone. A run journaled before enrich recorded this keeps its old behaviour: its tags undo, its Extra line stays, and its field overwrites are refused. A journal entry is therefore an audit record, not a promise of reversibility: `items delete` is journaled but not undoable, because the journal records what a run did, not a full pre-image of what it replaced. Recover a deleted item from Zotero's trash with `items restore`.
  A run with an earlier recorded undo cannot be undone again; inspect that undo before recovering an interrupted run.
- **One writer at a time.** Applying a write takes the installation-wide advisory lock (`~/.zotio/.writer.lock`). `export snapshot` and `collections bundle` instead lock their canonical output path, so runs writing to different directories stay parallel, and a vault write holds both the installation lock and its vault-path lock. Writers do not queue: a second concurrent writer in the same scope exits **9** immediately and is safe to retry once the first finishes. Reads and previews are never blocked, so parallelize those and serialize applies. See [Architecture decisions › Single-writer concurrency](../contributing/architecture-decisions.md#single-writer-concurrency).
  `watch` takes the installation lock per cycle; `tail` takes it per poll and releases it between polls.
- **Partial success is loud.** Zotero answers a batched write with HTTP 200 even when it rejected some elements. Those rejections are reported with their source index and message, and the command exits **13** (degraded) — never 0.

## Across a whole workflow

The contract scales from one command to a plan. `zotio workflow run` previews every mutating step by default and applies the whole plan on a single `--yes`; each mutation has its own journal `run_id` and all share one `workflow_run_id` (`journal list --workflow <id>` lists them; undo each entry by its `run_id`), and an interrupted run resumes from a checkpoint without replaying a write whose outcome is uncertain. See [Workflows & triggers](../guide/workflows.md).

## Where writes land

Writes split by intent: new personal-library items prefer the keyless local desktop connector. `items new` is an exception: every route needs a Web API key to fetch its `/items/new` template. Edits and note write-back need a Web API key. `attachments add --via connector` saves bytes through the desktop but still needs a Web API key to move the attachment to an existing item.

A Web API apply that needs to write without a key fails with an exit-9 precondition. The message names `ZOTERO_API_KEY` and `zotio auth set-token --stdin`. Keyless previews can still read the local plane, and no-op applies can succeed without a key. Separate read prerequisites still apply, including the template requirement for `items new`.

<div class="zotio-diagram-wrap">
--8<-- "docs/assets/architecture.svg"
</div>

The [capabilities reference](../reference/capabilities.md) lists the operation, write target, destructiveness, and requirements for every command. See [Authentication](../guide/authentication.md) for key setup.

## Example

```bash
zotio tags audit fix --agent            # preview: the merge plan, zero changes
zotio tags audit fix --agent --yes      # apply
zotio journal list                      # find the run id
zotio journal undo <run-id>             # preview the undo: shows the reversal, changes nothing
zotio journal undo <run-id> --yes       # apply it: reverse the tag renames
```
