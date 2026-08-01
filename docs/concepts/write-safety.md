# Safe-by-default writes

Writes are preview-first. The audit fixers and importers that change many items at once — `items enrich`, `tags audit fix`, `items duplicates resolve`, `import apply`, `import doi` — flow through one mutation envelope with identical, predictable semantics, and `workflow run` extends that same contract across a multi-step plan. The single-resource CRUD commands (`items create/update/delete/restore`, `collections create/update/delete`) share the preview and `--yes` gate but write directly against the API. The exceptions are listed below; read them once and you never have to guess which command is dangerous.

<div class="zotio-diagram-wrap">
--8<-- "docs/assets/write-safety.svg"
</div>

## The contract

- **Preview is the default.** You get a plan/result envelope with zero changes. `--yes` applies; `--dry-run` always wins.
- **`--agent` does *not* auto-apply.** Agent mode sets JSON + non-interactive defaults, but a write still needs an explicit `--yes`.
- **Two importers and `vault push` are the exceptions today.** `import file` and `import url` apply unless you pass `--dry-run`, and `vault push` gates on `--dry-run` rather than `--yes`. Treat `--dry-run` as the preview switch for those three until they move onto the shared gate.
- **Gates cap the blast radius.** `--max-changes` defaults to 500 (50 under `--agent`); irreversible ops (merge, permanent delete, empty-trash) refuse to run without `--allow-destructive`.
- **Read-your-writes.** A write applied through the mutation envelope — `items enrich`, `tags audit fix`, `items duplicates resolve`, `import apply`, `import doi`, `workflow run` — is replayed into the local mirror immediately, and the post-write item state comes back in the envelope, so a re-audit sees the fix with no follow-up `sync`.
- **Journaled + reversible.** Every run applied through that envelope is recorded append-only (`journal list` / `journal show`). `journal undo <run-id>` reverses the reversible ops (tag renames, collection membership) and **loudly refuses** the rest (merges, deletions, field overwrites) rather than guessing. The single-resource CRUD commands (`items create/update/delete/restore`, `collections create/update/delete`) still preview and require `--yes`, but they issue their write directly against the API, so they are neither journaled nor mirrored — `sync` afterwards, and undo them by hand.
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
