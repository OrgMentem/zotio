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

1. **`pending_writes` gains a `deleted` flag, and `StoreSchemaVersion` goes to
   8.** `deleted = 1` reclassifies the row from "these changes are applied but
   unconfirmed" to "this key is gone but unconfirmed"; `changes` stays `'[]'`,
   because a deletion is not expressible as a field change. Adding the column is
   mechanically additive (`INTEGER NOT NULL DEFAULT 0`, applied to existing
   databases by `ensureColumn` in `backfillColumns`, which is the whole 7-to-8
   migration — there is no accompanying data repair), but it is **not** a
   compatible addition, and the bump is load-bearing rather than bookkeeping.
   An older binary passes the version guard when the stamp is still 7, reads the
   table with a `SELECT` that omits the column, and therefore reads a deletion
   marker as a field-change marker carrying an empty change set. Its reconcile
   then replays nothing and stores the read-plane row the marker exists to
   suppress: the resurrection bug returns, silently, on any machine where an
   older zotio is still on `PATH` — a normal state during a staged rollout.
   Version 8 turns that into the loud refusal `store.go` and
   `readiness.go` already implement for a database newer than the binary.
   Reinterpreting an existing row is a shape change, not an addition.

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
   proves it landed — and only a pass that could have observed **every** object
   can establish that, because absence in any narrower pass means "not
   reported", not "gone". So `SweepMissing` retires every deletion marker of the
   swept resource whose key is absent from the pass's seen-set, in the same
   transaction as its reaps, and nothing else confirms.

6. **The sweep is gated on total observation, not on the `--full` flag, and
   total observation has two halves.** `full` names the user's intent; the sweep
   needs the property. The two came apart the moment `--full --since N` became
   expressible: `full` stays true while every request carries a since filter, so
   the plane omits every unchanged object. `syncResource` therefore requires
   both of:

   - **the requests observed every object** —
     `syncRequestObservesEveryObject` allowlists the two pagination params,
     which choose which *slice* of the whole set returns and which a completed
     pass walks in full, and treats every other param as narrowing,
     **including params that do not exist yet**. A future filter disables the
     sweep by default instead of silently invalidating it, and a genuinely
     non-narrowing param must be added next to that reasoning; and
   - **pagination reached the end** (`completedNaturally`) — truncation is not
     a request param, so the param check cannot see it: `--full --max-pages 1`
     over a multi-page resource issues perfectly unfiltered requests and still
     observes only a prefix. This half was already enforced, by the block that
     encloses the sweep and by the `full && !completedNaturally` error return
     that follows it, so `--full --max-pages 1` never reached the sweep even at
     HEAD. It is restated in the gate's own conjunction because a reviewer read
     the nesting as absent, and a rule spread 35 lines apart reads as two
     accidents.

7. **`--full --since N` runs without sweeping, and says so.** It is not refused.
   The sweep is the only part of `--full` that requires a total observation;
   cursor reset, stored-version invalidation and authoritative re-fetch of the
   selected objects are all coherent under a `since` filter, so refusing the
   whole request to withhold one component would deny a meaningful ask, break a
   combination the flags already accept, and follow no precedent — `--latest-only`
   plus `--since` already resolves by warning and degrading, not by erroring. But
   silence was the actual defect: a filtered pass masqueraded as a complete one.
   So a skipped sweep prints a warning naming the resource, why it was skipped,
   and the command that does reap (`zotio sync --full` without `--since`).

8. **A deletion marker retires on confirmation and on nothing else. There is no
   TTL backstop.** The marker is written only after `deleteWithVersionGuard`
   reports the delete applied, so the key is gone on the authoritative plane.
   Retiring the marker on a clock would let elapsed time overrule that confirmed
   fact, and it would do so in precisely the situation the marker exists for: a
   desktop that has not synced, whose read plane keeps listing an item the cloud
   says is gone. Restoring the row there puts a ghost into offline reads and
   `search`, and a warning does not undo a ghost. So the marker stays until a
   total-observation pass stops listing the key.

   The TTL survives as a **diagnostic only**. Past `pendingWriteTTL`,
   `checkPendingWriteAges` warns that a confirmed delete has not propagated,
   names the key, and says what to check (the Zotero desktop is not syncing). It
   changes no state. A field-change marker keeps the old rule and is retired at
   the TTL, because it holds a local guess that may never converge, and yielding
   to the read plane there contradicts nothing the write plane asserted.

   Both halves are age-driven, so neither can be tested per-row inside
   `reconcilePendingWrites`, which only ever inspects keys a fetched page
   contained. A marker guards precisely the row the read plane is not
   reporting, and a sync with nothing to report returns an **empty page** that
   breaks out of the paging loop before the reconciliation runs at all — the
   common case on a quiet installation. `syncResource` therefore calls
   `checkPendingWriteAges` unconditionally, once per resource, **before** it
   fetches anything: before, so an expired field marker does not hold the mirror
   against the very pass that retires it. It retires; it never confirms.

9. **Growth is one row per permanent delete, retired on the next
   total-observation pass.** For a user whose desktop syncs, that is a handful
   of rows for minutes. For a user whose desktop never syncs again, the rows
   persist indefinitely, and that is the correct trade: each one suppresses a
   row that genuinely no longer exists, so the cost of keeping it is a
   `pending_writes` row and the cost of dropping it is a ghost in every offline
   read, search result and mirror-derived count. `doctor` already reports
   `pending_writes`, which is where a count of long-unconfirmed deletions would
   naturally belong; that is not built here.

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

### Bound: a delete on an install that has never synced

`applyMirrorWriteThrough` opens the mirror through `openExistingStoreForWrite`,
which returns nil when the database **file** does not exist, and returns early.
So a permanent delete on a machine that has never synced writes no marker, and
the first sync can insert the stale read-plane row.

This is accepted, not fixed. Creating the database from the delete path is the
wrong trade: a command that removes something must not have "and now you have a
local mirror" as a side effect, and it would put store creation on a path with
no other reason to touch the store.

The honest bound, checked against the code rather than assumed:

- **At delete time nothing is harmed.** There is no mirror, so there is no
  offline read to poison and nothing mirrored to resurrect.
- **The first sync may store the row**, because the read plane still lists the
  item until Zotero syncs the delete down.
- **The row then survives until the next `zotio sync --full` without `--since`,
  which the user may never run.** Not "the first sync": a default `zotio sync`
  is not a full pass (`full` comes only from the `--full` flag), and
  `SweepMissing` is the only thing that can reap a row for an object that is
  gone, because the local desktop API implements no `/deleted` feed. This is
  true at HEAD too and is not a consequence of the observation gate.
- **Once landed, the row carries no marker**, so nothing suppresses it on later
  passes. The state is latent, not self-healing.
- **What the user sees** is an item they permanently deleted appearing in the
  first synced listing — which is what they would report as a bug.

This is one instance of a general property of the mirror, not a quirk of the
delete path: a default sync never sweeps, so any object that disappears upstream
outlives its object locally until a full pass. See the comment on
`TestSweepMissingReapsAbsentRows`, which already states that a full pass is the
only way zotio learns an object is gone.

`items delete` therefore emits a one-line stderr notice when the mirror does not
exist, naming what happened, what the user will see, and the remedy. That notice
lives in `write_through.go`, which this slice does not own.

## Consequences

- A sync inside the read-plane lag window can no longer resurrect a permanently
  deleted item, in either `items` or `items-trash`.
- `doctor`'s `pending_writes` count now includes outstanding deletions. That
  remains truthful — a pending delete is a local write Zotero has not synced down
  — and it makes the delete window visible instead of invisible.
- **The delivered lifetime, stated once.** A deletion marker lives until the
  first pass that observed every object of its resource stops listing the key.
  Nothing else retires it — there is no TTL backstop, and any earlier wording
  promising one is superseded. Past `pendingWriteTTL` the marker is reported,
  not removed. A field-change marker keeps the older lifetime: confirmed by a
  page that carries the write, or retired at the TTL.
- An installation whose desktop never syncs keeps its deletion markers
  indefinitely, and keeps suppressing rows for objects that genuinely do not
  exist. It is warned on every sync of the affected resource. That noise is the
  price of not serving a ghost.
- A filtered `--full` pass (`--full --since N`) reaps nothing and confirms
  nothing, and warns that it skipped the reap. That is a behaviour change for
  that combination: before, it reaped almost the whole mirror.
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
- **Mixed-version installations are refused, not degraded.** A database written
  by this binary is at version 8, so an older zotio on the same machine fails to
  open it — read-write and read-only alike — with the existing "newer than
  supported version; upgrade the CLI binary" error, instead of resurrecting
  purged items behind the operator's back. A 7-to-8 upgrade in the other
  direction is transparent: `backfillColumns` adds the column and the open
  stamps 8.

## Alternatives considered

| Option | Why not |
|---|---|
| **Deletion marker in `pending_writes` (chosen)** | Reuses machinery that already exists and is tested: one marker table, one confirmation sweep, one doctor counter, one table the reap transaction already touches. The delete *is* an unconfirmed local write; the only thing missing was a way to say the write was a deletion. The clock is where the two kinds part company, and that difference is one column, not one table. |
| **A separate tombstone table** | Duplicates a lifetime that already exists. It would need its own insert path, its own TTL, its own clear-on-confirm sweep, its own doctor surface and its own tests, all parallel to `pending_writes` and free to drift from it — and it would still have to be written inside the reap's transaction and read at exactly the point `reconcilePendingWrites` already reads markers. A second table buys separation zotio has no use for: a key cannot simultaneously carry a field write and a deletion, so the two never need to coexist per row. |
| **Accept and document the window** | The window is user-visible data corruption, not a cosmetic lag: offline reads, `search`, `library health` and every mirror-derived count serve an item that 404s on both planes, and the reporter hit exactly this (933 mirrored top-level items against 929 real ones). Documenting it also does not bound it — without a full pass the resurrected row survives indefinitely. |
| **Enforce suppression inside `UpsertKeyed`/`UpsertBatch`** | Would make local write-through non-authoritative over its own mirror and put a marker lookup on every mirror write, to protect one caller (`sync`) that can check once per page. |
| **Retire the marker on any pass that omits the key** | Absence in an incremental pass means unchanged, not gone, so this clears the marker on the first sync after the delete — precisely when the plane is still lagging — and the next page resurrects the item. |
| **Retire a deletion marker at the TTL and restore the row** | This was the first design here, and it is wrong. The marker is written only after the write plane reported the delete applied, so the clock would overrule a confirmed fact — and it would fire in exactly the scenario the marker exists for, a desktop that has not synced, putting a purged item into offline reads and `search`. A warning does not undo a ghost. The clock stays as a diagnostic. |
| **Retire a deletion marker at the TTL and leave the row reaped** | Retires the only thing suppressing re-insertion while the read plane still lists the key, so the next page that reports it stores it. The marker's absence is not neutral; it is permission. |
| **Check the TTL per row, inside `reconcilePendingWrites`** | It only sees keys a fetched page contained, and a marker guards precisely the key the plane is not reporting. A quiet sync returns an empty page and breaks out before the reconciliation runs at all, so the field-marker TTL was never reached and an abandoned write pinned its row forever. It also reads as correct, which is why the age pass is separate and commented against being folded back in. |
| **Gate the sweep on the `--full` flag** | A flag names intent, not the property the sweep needs. `--full --since N` kept the flag true while every request narrowed the response, so the sweep reaped almost the whole mirror and confirmed every marker. |
| **Allowlist known narrowing params instead of default-deny** | Puts the next filter's author in charge of remembering to disable the sweep. Default-deny fails safe: an unrecognized param costs one skipped reap and a warning, while a missed one costs mirror rows and a reopened resurrection window. |
| **Refuse `--full --since N` as a contradiction** | Only the sweep needs a total observation; the rest of `--full` (cursor reset, stored-version invalidation, authoritative re-fetch of the selected objects) is coherent under a filter. Refusing would break a combination the flags accept today to withhold one component of it, and it has no precedent here: `--latest-only` plus `--since` warns and degrades. The defect was the silence, so the fix is the warning. |
| **Create the mirror from the delete path so a never-synced install gets a marker** | A command that removes something must not have "and now you have a local mirror" as a side effect, and it would put store creation on a path with no other reason to touch the store. The bound is documented instead, with a stderr notice at delete time. |

## Validation

- Seeding a mirrored item, reaping it, then syncing against a read plane that
  **still lists that key** must leave `resources` without the row, for both
  `items` and `items-trash`, while every other row on the page is stored.
- Once a complete pass stops listing the key, the marker must be gone: no
  outstanding marker may leak into `doctor`'s count or suppress a later row.
- An incremental pass that omits the key must **keep** the marker.
- A deletion marker past its TTL must SURVIVE a sync, the purged row must stay
  out of `resources`, and a warning must name the key and the unpropagated
  delete. This is the assertion that pins the clock as a diagnostic; an earlier
  revision asserted the opposite.
- An expired FIELD marker must be retired by a sync that never sees its key,
  including a sync that fetches an empty page.
- `RecordPendingWrite` must not overwrite a deletion marker, and must still
  record normally for keys that carry none.
- The reap must roll the row deletions back when the marker insert fails, proving
  the two are one transaction in both directions.
- Trash and restore must be unaffected: `items trash` still sees what
  `items delete` trashed, and a restore still reconstructs the live row from the
  cached trash payload.
- A database stamped one version ABOVE this binary must be refused on both open
  paths — read-write and read-only — naming the version mismatch. The adjacent
  version is the case that matters; a far-future stamp does not prove the guard
  is strict.
- A database an older binary created (version 7, `pending_writes` without the
  column) must still open, gain the column, keep its existing markers as
  field-change markers, and be able to record a deletion marker afterwards.
- A deletion marker inside its TTL must also survive a sync that never sees its
  key. Absence is not confirmation, and that is what stops the age pass from
  being simplified into the confirmation pass.
- A marker whose `written_at` is NULL must never be retired by the age pass: an
  unrecorded clock is not an expired one.
- `--full --since N` must reap no rows, retire no markers, and warn that it
  skipped the reap; `--full` alone must still reap and still retire a confirmed
  marker, so the gate is not "never sweep".
- `--full --max-pages 1` over a multi-page resource must reap no rows and retire
  no markers, and must report itself incomplete.

## References

- ADR-0005 — Single-writer concurrency contract: the reap, the sweep and their
  commands stay inside it; readers remain lock-free.
- AGENTS.md — "Zotero API Surface": local API is GET-only, has no `/deleted`
  feed, and Web API writes reach the desktop only on the next Zotero sync.
- Finding `zotio-ec7c129c5d58bd24`; commit `8b22538` (atomic reap, which closed
  the torn state only).
