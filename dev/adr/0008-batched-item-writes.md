# ADR 0008 — Batched item writes are opt-in, and only the writes batch

**Status:** Accepted (2026-09-15)

## Context

Every item mutation costs one HTTP write. A tag sweep costs three requests per
item: a planning read, an apply-time re-read inside `applyItemTagAdd` /
`applyItemTagRemove`, and the guarded `PATCH`. Measured at roughly 0.95s per
item (`dev/field-report-2026-08-08-verification.md:71-74`), a 500-item hygiene
run takes about eight minutes and 1,500 requests. That is the single largest
cost in bulk library hygiene, which is a stated product capability.

Zotero's Web API accepts up to 50 objects in one write request and answers
with `successful` / `unchanged` / `failed` maps keyed by the object's index in
the submitted array, where each `failed` entry carries the object `key`, a
`code`, and a `message`.

This was previously declined on the grounds that **array writes cannot
preserve per-item conflict attribution**. That reason is wrong, and no record
of the decline existed, so the finding resurfaced on every review. The
response is index-keyed and index maps deterministically to the submitted
object; the `failed` entry also names the key outright. Multi-object update
additionally accepts a per-object `version`, so per-item preconditions
survive. Attribution is not the obstacle.

## Decision

Batched writes ship as an **opt-in** `--batch` flag, starting with
`items tags add` and `items tags remove`. The implementation reuses the
`collections create` shape: one `mutation.Op` per item, where the first op's
`Apply` issues the array request and caches the decoded response, and every
other op reads its own outcome from that cache.

Two contract differences are accepted and documented at the flag, not hidden:

1. **The precondition moves into the object body.** `write_precondition.go`
   deliberately prefers `If-Unmodified-Since-Version` over a body `version` to
   avoid two preconditions that can disagree. One header cannot express many
   versions, so a heterogeneous batch has no alternative. A lost precondition
   returns code 412 in `failed` and is reported as that item's `conflict`,
   exactly as the per-item path reports it.

2. **Fail-fast cannot hold.** Every object in a request reaches Zotero
   together, so an early rejection cannot un-send its batch-mates. Reporting
   them as `not_attempted` would be false and would invite a duplicating
   retry. `--batch` therefore forces `ContinueOnError`, and `--batch` combined
   with `--max-failures` is **refused** rather than silently ignored.

The batched path also reuses the planning read instead of re-reading at apply
time. This is what removes the second request per item. It widens the window
in which a concurrent edit can land; that edit loses the precondition and is
reported as a conflict, never as a silent overwrite.

**Reads are not batched.** Tag merging needs each item's current tags, so the
saving is in writes only: 200 items go from 600 requests to 204, not to 4.
Claiming a 50x speedup would be dishonest.

## Consequences

- Bulk tag hygiene gets roughly a 3x request reduction, opt-in, with per-item
  status, per-item conflicts, and Zotero's own per-object error messages
  intact.
- The default path is unchanged, so no existing caller loses fail-fast.
- `items enrich` is **not** batched. Its apply path resolves each field change
  against a live write-plane read and composes Extra provenance per item;
  batching it means restructuring that read, which is a separate change with
  its own risk. The helper (`internal/cli/batch_item_update.go`) is written
  against object arrays, not tags, so enrich can adopt it later.
- If a future caller needs fail-fast over large key sets, the answer is
  smaller chunks or the default path, not a partial batch abort.
