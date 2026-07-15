# Safe-by-default writes

Every write command — `items enrich`, `tags audit fix`, `items duplicates resolve`, `items create/update/move/delete`, `import apply`, `vault push` — flows through one mutation envelope with identical, predictable semantics. You never have to remember which command is dangerous; they all behave the same way, and `workflow run` extends that same contract across a multi-step plan.

<div class="zotio-diagram-wrap">
--8<-- "docs/assets/write-safety.svg"
</div>

## The contract

- **Preview is the default.** You get a plan/result envelope with zero changes. `--yes` applies; `--dry-run` always wins.
- **`--agent` does *not* auto-apply.** Agent mode sets JSON + non-interactive defaults, but a write still needs an explicit `--yes`.
- **Gates cap the blast radius.** `--max-changes` defaults to 500 (50 under `--agent`); irreversible ops (merge, permanent delete, empty-trash) refuse to run without `--allow-destructive`.
- **Read-your-writes.** An applied write is replayed into the local mirror immediately, and the post-write item state comes back in the envelope — a re-audit sees the fix with no follow-up `sync`.
- **Journaled + reversible.** Every applied run is recorded append-only (`journal list` / `journal show`). `journal undo <run-id>` reverses the reversible ops (tag renames, collection membership) and **loudly refuses** the rest (merges, deletions, field overwrites) rather than guessing.

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
