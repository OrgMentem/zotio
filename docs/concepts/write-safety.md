# Safe-by-default writes

Writes are preview-first, with no exceptions. Every command that changes your library or your vault previews by default, applies only under `--yes`, and counts what it plans against `--max-changes`. That covers the bulk fixers and importers (`items enrich`, `tags audit fix`, `items duplicates resolve`, `import apply`, `import doi`, `import file`, `import url`, and the generic `import <resource>`), every single-item create (`items new`, `import pmid`, `import arxiv`, `import isbn`), the single-resource CRUD commands (`items create/update/delete/restore`, `collections create/update/delete`), and every vault command (`vault push`, `vault pull`, `vault sync`, `vault resolve`); `workflow run` extends the same contract across a multi-step plan. Read the contract once and you never have to guess which command is dangerous.

<div class="zotio-diagram-wrap">
--8<-- "docs/assets/write-safety.svg"
</div>

## The contract

- **Preview is the default.** You get a plan/result envelope with zero changes. `--yes` applies; `--dry-run` always wins.
- **`--agent` does *not* auto-apply.** Agent mode sets JSON + non-interactive defaults, but a write still needs an explicit `--yes`.
- **No command is exempt.** Every write in this CLI needs `--yes`, and `--dry-run` overrides it everywhere — including inside a `workflow run` preview, which previews each step by injecting `--dry-run`. The vault commands keep their own per-note report rather than the generic envelope, but they obey the same gate.
- **Gates cap the blast radius.** `--max-changes` defaults to 500 (50 under `--agent`); irreversible ops (merge, permanent delete, empty-trash) refuse to run without `--allow-destructive`.
- **Read-your-writes.** A write applied through the mutation envelope is replayed into the local mirror immediately and the post-write item state comes back in the envelope, so a re-audit sees the fix with no follow-up `sync`. Replay is deliberately conservative: scalar field edits and tag/collection membership are replayed, while creates, trash, and structural edits are left for the next `sync` to reconcile authoritatively.
- **Journaled, and honest about undo.** Runs applied through the mutation envelope are recorded append-only (`journal list` / `journal show`), including the single-resource CRUD commands and partially-rejected batches (a 100-item create where one element is refused still journals the 99 that landed). Some gated writes deliberately sit outside the envelope and so are not journaled: the `vault` commands, which keep their own report, and `items create` when it routes through the desktop connector. `journal undo <run-id>` reverses only what it can invert losslessly — tag and collection membership toggles — and **loudly refuses** everything else (merges, deletions, field overwrites, creates) rather than guessing. A journal entry is therefore an audit record, not a promise of reversibility: `items delete` is journaled but not undoable, because the journal records what a run did, not a full pre-image of what it replaced. Recover a deleted item from Zotero's trash with `items restore`.
- **One writer at a time.** Applying a write takes the installation-wide advisory lock (`~/.zotio/.writer.lock`). `export snapshot` and `collections bundle` instead lock their canonical output path, so runs writing to different directories stay parallel, and a vault write holds both the installation lock and its vault-path lock. Writers do not queue: a second concurrent writer in the same scope exits **9** immediately and is safe to retry once the first finishes. Reads and previews are never blocked, so parallelize those and serialize applies. See [Architecture decisions › Single-writer concurrency](../contributing/architecture-decisions.md#single-writer-concurrency).
- **Partial success is loud.** Zotero answers a batched write with HTTP 200 even when it rejected some elements. Those rejections are reported with their source index and message, and the command exits **13** (degraded) — never 0.

## Across a whole workflow

The contract scales from one command to a plan. `zotio workflow run` previews every mutating step by default and applies the whole plan on a single `--yes`; every mutation in the run shares one journal run ID (`journal list --workflow <id>`), and an interrupted run resumes from a checkpoint without replaying a write whose outcome is uncertain. See [Workflows & triggers](../guide/workflows.md).

## Where writes land

Writes split by intent — new items prefer the keyless local desktop connector; everything else routes to the Zotero Web API and needs a key.

<div class="zotio-diagram-wrap">
--8<-- "docs/assets/architecture.svg"
</div>

The [capabilities reference](../reference/capabilities.md) lists the operation, write target, destructiveness, and requirements for every command. See [Authentication](../guide/authentication.md) for key setup.

## Example

```bash
zotio tags audit fix --agent            # preview: the merge plan, zero changes
zotio tags audit fix --agent --yes      # apply
zotio journal list                      # find the run id
zotio journal undo <run-id>             # reverse the tag renames
```
