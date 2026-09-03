# ADR 0007 — Permanent deletes are tombstoned in `pending_writes`

- **Status:** Accepted (2026-09-03).
- **Scope:** `Store.ReapMirroredItem`, `Store.SweepMissing` and the `pending_writes` table in `internal/store/store.go`; `reconcilePendingWrites` in `internal/cli/pending_writes.go`; the `item_delete` write-through path in `internal/cli/write_through.go`.
- **Deciders:** enieuwy.

## Context

zotio's two planes are asymmetric by design. Reads go to Zotero's **local
desktop API**, which is GET-only and implements no `/deleted` feed. Writes are
auto-routed to **api.zotero.org**, and they reach the desktop only when Zotero
itself next syncs. Every locally-applied write therefore spends a window —
normally seconds to minutes — being invisible to the plane `sync` reads from.

For a field change that window is already handled. Write-through replays the
applied change onto the mirror, records a **pending-write marker** holding the
serialized `[]mutation.Change`, and `reconcilePendingWrites` re-applies those
changes on top of each row the read plane reports. When replaying becomes a
no-op the plane has caught up and the marker is dropped; `pendingWriteTTL`
(7 days) is the backstop for a write that never propagates.

A **permanent delete** has the same window and no field to replay, and nothing
covered it. `reapMirroredItem` removed the live row, the trash row and both FTS
documents, and — before this decision — deleted the marker as well. The marker
table had no way to express "this key no longer exists", so `reconcilePendingWrites`
could not suppress anything: any sync between the delete and Zotero syncing the
delete down re-inserted the purged item into `resources` from the read plane's
still-current listing. Offline reads, `search` and every mirror-derived count
then served an item that 404s on **both** planes, until a later full pass swept
it (finding `zotio-ec7c129c5d58bd24`).

Commit `8b22538` made the reap one transaction. That closed the torn
intermediate state — a reader observing the item gone from `items` while still
present in `items-trash` — and nothing else. The store's own comment said so.

## Decision

**A permanent delete writes a deletion marker into `pending_writes`, in the same
transaction as the reap, and the sync reconciliation drops any read-plane row
whose key carries one.**

1. **`pending_writes` gains a `deleted` flag.** `deleted = 1` reclassifies the
   row from "these changes are applied but unconfirmed" to "this key is gone but
   unconfirmed"; `changes` stays `'[]'`, because a deletion is not expressible as
   a field change. The column is additive (`INTEGER NOT NULL DEFAULT 0`,
   backfilled by `ensureColumn`), so no schema-version bump: an older binary
   ignores the flag and merely resurrects rows as it does today.

2. **The marker is written by the reap, not after it.** `ReapMirroredItem`
   interleaves `reapResourceLocked` and `markPendingDeletionLocked` for `items`
   and `items-trash` inside one transaction under `writeMu`. There is no instant
   at which the rows are gone and the suppression is not yet in force. Both
   canonical resources get a marker because both listings keep reporting the key:
   `/items` during the lag, and `/items/trash` when the item was trashed before
   it was purged.

3. **A deletion outranks a field change for the same key.** One envelope can
   carry both, and the order inside it is not the store's to choose, so
   `RecordPendingWrite`'s upsert skips a row with `deleted = 1`.

4. **`reconcilePendingWrites` drops the row.** A listed key with a live deletion
   marker is removed from the page before it is stored, per key — every other row
   the plane reported still lands. Rows reaped by `ReapResource` and
   `SweepMissing` get **no** marker: those reaps act on evidence that the read
   plane already stopped reporting the key, so a marker there would suppress
   nothing and only leak.

5. **Confirmation is the mirror image of the field case, and belongs to
   `SweepMissing`.** For a field write, the read plane *carrying* the change
   proves it landed. For a delete, the read plane *no longer listing* the key
   proves it landed — and only a **complete** pass can establish that, because in
   an incremental pass absence means unchanged. That is exactly `SweepMissing`'s
   existing precondition, so `SweepMissing` retires every deletion marker of the
   swept resource whose key is absent from the pass's seen-set, in the same
   transaction as its reaps. An incremental sync never retires one.

6. **TTL expiry means upstream wins, loudly.** A read plane still listing a
   purged key after `pendingWriteTTL` is not lagging: the delete never reached
   zotero.org, or Zotero sync is off, so the object genuinely still exists there.
   The marker is cleared and the row is stored again — the same rule an expired
   field marker follows — accompanied by a stderr warning naming the key, the
   elapsed TTL and the two things to check. The item does not come back silently,
   because a reappearing row is otherwise indistinguishable from a delete that
   failed. Once Zotero does sync the delete down, the next full pass reaps the
   row through `SweepMissing` as it always did.

### Interaction with ADR-0005

Nothing here weakens the single-writer contract. The marker lives in the SQLite
mirror, which ADR-0005 classifies as a concurrent-read subsystem: readers stay
lock-free under WAL and take no writer lock. The reap and the sweep are store
mutations serialized on `writeMu` inside one transaction each, and the CLI
commands that reach them (`items delete`, `sync`) already acquire the
installation-scope writer lock before their first state read, because a remote
library mutation is derivation-dependent. Suppression is a *sync-path* decision,
deliberately not enforced inside `UpsertKeyed`/`UpsertBatch`: local write-through
must stay authoritative over the mirror, and pushing the check into the store's
unconditional upsert would make every local write consult a marker table it does
not need.

## Consequences

- A sync inside the read-plane lag window can no longer resurrect a permanently
  deleted item, in either `items` or `items-trash`.
- `doctor`'s `pending_writes` count now includes outstanding deletions. That
  remains truthful — a pending delete is a local write Zotero has not synced down
  — and it makes the delete window visible instead of invisible.
- A deletion marker normally clears on the first **full** sync after Zotero
  syncs the delete down. Between the delete and that pass the marker persists,
  which is the whole point; an installation that only ever runs incremental syncs
  holds it until the TTL, having suppressed a row that was correct to suppress
  the whole time.
- The trash and restore paths are untouched. `mirrorTrashedItem` still keeps the
  live row (a trashed Zotero item still exists with `deleted=1`), and
  `RestoreMirroredItem` still reconstructs the live row from the cached trash
  payload. A restore cannot collide with a deletion marker: a purged key 404s on
  the write plane, so no restore write-through ever runs for one.
- `reconcilePendingWrites` may now return a **shorter** page than it received.
  Its caller (`upsertResourceBatch`) reports the stored count from that page, so
  a suppressed row is honestly counted as not stored.
- The atomicity guarantee is now two-sided: the rows and the marker commit or
  roll back together, so no observer sees the live row gone with no marker
  written.

## Alternatives considered

| Option | Why not |
|---|---|
| **Deletion marker in `pending_writes` (chosen)** | Reuses the lifecycle that already exists and is tested: one TTL, one clear-on-confirm rule, one doctor counter, one table the reap transaction already touches. The delete *is* an unconfirmed local write; the only thing missing was a way to say the write was a deletion. |
| **A separate tombstone table** | Duplicates a lifetime that already exists. It would need its own insert path, its own TTL, its own clear-on-confirm sweep, its own doctor surface and its own tests, all parallel to `pending_writes` and free to drift from it — and it would still have to be written inside the reap's transaction and read at exactly the point `reconcilePendingWrites` already reads markers. A second table buys separation zotio has no use for: a key cannot simultaneously carry a field write and a deletion, so the two never need to coexist per row. |
| **Accept and document the window** | The window is user-visible data corruption, not a cosmetic lag: offline reads, `search`, `library health` and every mirror-derived count serve an item that 404s on both planes, and the reporter hit exactly this (933 mirrored top-level items against 929 real ones). Documenting it also does not bound it — without a full pass the resurrected row survives indefinitely. |
| **Enforce suppression inside `UpsertKeyed`/`UpsertBatch`** | Would make local write-through non-authoritative over its own mirror and put a marker lookup on every mirror write, to protect one caller (`sync`) that can check once per page. |
| **Retire the marker on any pass that omits the key** | Absence in an incremental pass means unchanged, not gone, so this clears the marker on the first sync after the delete — precisely when the plane is still lagging — and the next page resurrects the item. |
| **Keep an expired marker forever (never resurrect)** | A marker that outlives every convergence guarantee becomes an invisible permanent filter: a key the read plane insists exists would stay unmirrorable with no way for the user to learn why. The field-marker rule — upstream wins after the TTL, with a warning — is the coherent one. |

## Validation

- Seeding a mirrored item, reaping it, then syncing against a read plane that
  **still lists that key** must leave `resources` without the row, for both
  `items` and `items-trash`, while every other row on the page is stored.
- Once a complete pass stops listing the key, the marker must be gone: no
  outstanding marker may leak into `doctor`'s count or suppress a later row.
- An incremental pass that omits the key must **keep** the marker.
- An expired deletion marker must yield to the read plane, clear itself, and warn
  on stderr naming the key.
- `RecordPendingWrite` must not overwrite a deletion marker, and must still
  record normally for keys that carry none.
- The reap must roll the row deletions back when the marker insert fails, proving
  the two are one transaction in both directions.
- Trash and restore must be unaffected: `items trash` still sees what
  `items delete` trashed, and a restore still reconstructs the live row from the
  cached trash payload.

## References

- ADR-0005 — Single-writer concurrency contract: the reap, the sweep and their
  commands stay inside it; readers remain lock-free.
- AGENTS.md — "Zotero API Surface": local API is GET-only, has no `/deleted`
  feed, and Web API writes reach the desktop only on the next Zotero sync.
- Finding `zotio-ec7c129c5d58bd24`; commit `8b22538` (atomic reap, which closed
  the torn state only).
