# zotio field report #3 — 2026-08-08 — verification of the field-report fixes

Third report. Verifies the work in the working tree behind `zotio 0.16.1-dev+fieldreports`
(48 files, +988/−105, uncommitted) against every finding in
`dev/field-report-2026-08-08.md` and `dev/field-report-2026-08-08-library-hygiene.md`.

Same install: macOS 25.6.0, Zotero desktop running with local API enabled, personal library
`5847066`, 928 top-level items / 2621 records / 786 distinct tags.

**All tests were run through zotio itself** (no API workarounds), with direct `api.zotero.org`
reads used only as an independent oracle to check what zotio actually wrote. Every mutation was
reverted; the library finished byte-identical to its pre-test state — 2621 records, 786 distinct
tags, no probe tags, no reverted merges.

---

## Verdict

| # | Finding | Status |
| --- | --- | --- |
| **2/P0** | `tags rename` cannot write (`expected_version: 0`) | **Fixed** — but see New-1, which blocks it in practice |
| R1-2 | Read-after-write incoherence (`collections-of` → `[]`) | **Fixed** (write-through) |
| R1-3 | `doctor` reports 0 rows for a hydrated cache | **Fixed** |
| R1-6 | `import pdf` doesn't return created keys | **Fixed** |
| R1-9 | `--plain` silently returns JSON | **Fixed** |
| R1-11 | `no_op` carries no reason | **Fixed** |
| R1-13 | `items move` / `add-to-collection` no cross-reference | **Fixed** |
| R2-2 | No batch apply for the tag merge plan | **Fixed** (`tags audit fix`) |
| R2-4 | `items audit` counts children | **Fixed** |
| R2-9 | `library health` / `items audit` disagree on "the library" | **Fixed** — both now report 928 top-level |
| R1-8 | No connector preflight | **Fixed** — verified with the desktop shut down |
| R1-4/5 | `sync` reports success on a frozen store; "Cache: fresh" | **Not fixed** — cursor still web-space |
| R2-6a | `sync` help doesn't say which plane it pulls from | **Not fixed** |
| R1-7 | `import pdf --collection` | Not addressed (not claimed) |
| R1-10 | Envelope shape varies | Not addressed (not claimed) |
| R1-12 | `which` misses collection-filing phrasings | Not addressed (not claimed) |
| R2-3 | No `--prefer` casing policy | Not addressed (not claimed) |
| R2-5 | `creators audit` has no remediation | Not addressed (not claimed) |
| R2-7 | `import pdf` creates detectable duplicates | Not addressed (not claimed) |
| R2-8 | `items find` bare-array envelope | Not addressed (not claimed) |

Plus **9 new findings**, two of them serious.

---

## Confirmed fixed

### P0 — `tags rename` writes

```
$ zotio tags rename --from 'zotio-fieldtest' --to 'Zotio-Fieldtest' --dry-run --json
{"key":"4LIPWP5Y","kind":"tag_rename","expected_version":69}      # was 0
```

Applied, and the write plane agrees: `{"version":12750,"tags":["/unread","Zotio-Fieldtest"]}`.

### The stale-plan guard is excellent — this is the best result in the release

The store on this machine is still frozen at 2026-07-15 (see New-3), so `tags audit` still plans
the 53 groups I merged hours earlier — tags that no longer exist on **any** plane. Running that
knowingly-stale plan is the worst case the new write-plane re-read has to survive:

```
$ zotio tags audit fix --yes
{"attempted":71,"applied":0,"no_op":71,"conflicts":0,"failed":0,"codes":{"tag_absent":71}}
```

Independent verification before/after: 786 distinct tags → 786, **zero tags changed**. A three-week
stale plan produced exactly zero writes and reported precisely why, per item
(`"item no longer carries tag \"misinformation\" on the write plane"`). That is the right design.

One caveat, not a correctness issue: the batch is sequential and re-reads each item from the write
plane, so those 71 operations took **67 seconds** (~0.95 s/op). A first-time merge on a library with
heavy tag drift will be a multi-minute foreground command with no progress output. Worth either a
progress event or bounded concurrency before this gets used on a big library.

### The rest

- **`doctor`**: `items: 4308 rows`, `collections: 469 rows`, with `last delta` reported separately.
  Also now surfaces `pending_writes: 3 (local writes Zotero has not synced down yet)`.
- **`items audit`**: `Scope: 928 top-level items`; `missing-abstract` 3773 → **393**,
  `missing-tags` 4018 → **639**. `top_level_items` present in JSON. Every count now ≤ 928.
- **`--plain`**: real TSV from `items recent`, `search`, `collections list`, `items collections-of`,
  `collections stats`. `items audit --plain` keeps its table rather than mangling.
- **`items move` no-ops**: `{"code":"already_member","message":"already in target collection"}` and
  `{"code":"already_moved","message":"collection membership already matches requested move"}`.
- **`import pdf`**: returns `item_key`, `attachment_key`, `doi`. The anti-false-match guard is real —
  two *older* items share the imported PDF's exact title and it still returned the new key
  (`24GL6HHR`), not either of them.
- **`import pdf` preflight** (R1-8): with Zotero desktop shut down, the plan phase now refuses
  instead of previewing an impossible import, and exits non-zero:
  ```
  $ zotio import pdf /tmp/pre.pdf --dry-run   # Zotero quit
  Error: import pdf requires desktop_connector: Zotero desktop connector is not reachable:
         connector ping: … dial tcp 127.0.0.1:23119: connect: connection refused
  exit=9
  ```
  Read commands correctly keep working from the mirror while the desktop is down (`items get`,
  `tags audit` both exit 0). Nit: the message could name the no-desktop alternative (`import doi`).
- **Read-after-write**: `items collections-of 4LIPWP5Y --plain` → `RVETQVZ4`. Previously `[]`.
- **Help cross-references** exist in both directions.
- **Journal**: renames are journaled (`tags.rename applied=1 ok`), and `journal undo` correctly
  reversed a tag add.

---

## New findings

### New-1 — [HIGH] Write-through creates mirror rows with no version, and `tags rename` then silently does nothing

The single most important issue in this build. It defeats the P0 fix on exactly the items a user
just touched.

```
$ zotio items tags add ZFTIBMSY --tag 'probe-b' --yes
{"attempted":1,"applied":1,...}                       # write succeeds

$ zotio tags rename --from 'probe-b' --to 'Probe-B' --yes
{"attempted":0,"applied":0,"no_op":0,"conflicts":0,"failed":0}   # selects nothing, exit 0, silent
```

Both planes can see the tag — this is not propagation lag:

```
local  /items?tag=probe-b → 1
web    /items?tag=probe-b → 1
```

The discriminator is the mirror row's version:

```
4LIPWP5Y   items   ver=87     tags=[…,"probe-a"]     → rename selects 1  ✅
ZFTIBMSY   items   ver=NULL   tags=[{"tag":"probe-b"}] → rename selects 0  ❌
```

`4LIPWP5Y` already existed in the mirror from the July sync, so write-through **updated** the row and
its version survived. `ZFTIBMSY` had no `items` row, so write-through **created** one — with no
`version` field. Selection skips version-less rows.

Two failure modes from the same cause:

- `tags rename` — **silent**. `selected: 0`, `ok: true`, exit 0, no warning. Identical output to
  "that tag doesn't exist", which is the dangerous part.
- `tags audit fix` — **aborts the whole batch** on one such row:
  `Error: planning tag audit fix for "Zotio-Fieldtest": item 4LIPWP5Y missing version`.
  One version-less item takes down all 53 groups.

`--data-source auto|local|live` all behave identically, so selection ignores the flag and always
reads the mirror. And because the sync cursor is frozen (New-3), a version-less row can never be
repaired by syncing — the item is permanently invisible to tag renames.

Suggested fix: have write-through carry the version from the write response into the mirror row
(the rename path already does this — its rows get versions; the `items tags add`/`remove` path does
not). Then make selection treat a missing version as "re-read from the write plane", never as
"skip". At minimum, `selected: 0` when the tag demonstrably exists must not be silent.

### New-2 — [HIGH] `items delete` permanently destroys data while documenting "moves to trash"

```
$ zotio items delete --help
Delete an item (moves to trash)

$ zotio items delete 24GL6HHR --yes        # no --allow-destructive required
$ curl .../items/24GL6HHR   → 404
$ curl .../items/trash      → not present
$ curl .../items/49U6ZJK3   → 404          # child attachment destroyed too
```

The item is gone from the server, is *not* in the trash, and the PDF attachment went with it.
`items restore` ("Restore a trashed item") cannot recover it. The global `--allow-destructive` flag
advertises that it gates "irreversible operations (merge, permanent delete, empty-trash)" — this is
a permanent delete and it is not gated.

I hit this while cleaning up my own test import, so nothing of yours was lost, but a user following
the help text will lose items and their PDFs believing they are recoverable from the trash.

Fix: either implement it as a trash operation (`PATCH {"deleted": 1}`, which is what the help
describes and what `items restore` expects), or keep the hard delete, correct the help, and put it
behind `--allow-destructive`. The first is almost certainly what was intended.

### New-3 — [MED] The sync cursor is still in the wrong version space, so the mirror stays frozen

Unchanged from report #1 finding 4, and now the root cause of New-1's unrepairability:

```
sqlite> select resource_type, library_version from sync_state;
items|12689          # web-API version space
collections|12689

$ curl 'localhost:23119/api/users/0/items?limit=1' | jq '.[0].version'
71                   # local plane version space
```

`?since=12689` against the local plane returns nothing, forever. `sync` reports
`{"resource":"items","total":0}` with `success: 8, errored: 0`, and `doctor` still says
`Cache: fresh`. Concretely: `tags audit` still offers to merge 53 tags that no longer exist on any
plane — the plan is three weeks stale and nothing in the tool says so.

Write-through papers over this for zotio's own writes, which is a genuine improvement, but any
change made in Zotero desktop or by another client is invisible to zotio indefinitely.

### New-4 — [MED] `items tags list` returns 404 for every item

```
$ zotio items tags list 4LIPWP5Y
Error: GET /items/4LIPWP5Y/tags returned HTTP 404: No endpoint found
```

`/items/<key>/tags` exists on the web API (`200`) but not on the local desktop API (`404`), and this
read routes to the local plane. Reproducible on every item; `--data-source auto|live` all fail.

`--data-source local` fails differently, revealing a path-parsing bug:

```
Error: resource "items" with ID "tags" not found in local store
```

It has split `/items/<key>/tags` into resource `items`, id `tags`.

Fix: fall back to the web plane for endpoints the local plane doesn't implement (or read tags out of
the item payload, which both planes carry), and fix the local-store path parse.

### New-5 — [MED] Mutation payloads report `"journal": null` even when the run was journaled

Every successful mutation returns `"journal": null`, yet `journal list` shows the run:

```
20260808T023729Z-b58256fd  2026-08-08 02:37  user  tags.rename  applied=1  ok
```

So an agent cannot undo its own write from the write's own response — it has to call `journal list`
and guess which run was its own by timestamp. Populate `journal.run_id` in the response.

### New-6 — [LOW] `items delete --json` uses a fifth envelope shape

```
{"status":"noop","reason":"already_deleted"}
```

No `schema_version`, `ok`, `plan`, `result`, or `journal`, unlike every other mutation. `.result.items[0]`
— the pattern that works for `move`, `rename`, `tags add`, `tags remove`, `import pdf` — throws here.

### New-7 — [LOW] Test-fixture keys were written into the real journal

```
$ zotio journal show 20260808T022925Z-592ade01
Run … · tags.rename · applied=2
  [applied] tag_rename K1
  [applied] tag_rename K2
```

`K1`/`K2` are unit-test fixtures, recorded in this machine's user journal at 10:29 — the build
timestamp. A test run is writing to the developer's real journal store. Point tests at a temp
journal dir.

### New-8 — [LOW] `--plain` on item-shaped responses emits ~35 columns with JSON blobs in cells

`items recent --plain` and `search --plain` now correctly emit TSV, but include every field present
on any record — `library`, `links`, `meta`, `relations` render as raw JSON objects inside cells, and
one row can exceed 2 KB. Consider a sensible default projection (key, title, date, itemType, DOI)
with `--select` for the rest.

### New-9 — [LOW] `sync --help` still doesn't say which plane it pulls from

> Sync data from the API into a local SQLite database.

"the API" is the *local desktop* API, not `api.zotero.org` — the distinction that makes New-3
confusing to diagnose, since a user reasonably reads this as "sync with Zotero" and concludes their
cloud library is mirrored. Name the plane, and state the direction.

---

## Regression checks that passed

- The 54-rename stale plan wrote nothing and left 786/786 tags intact.
- `items tags add` / `items tags remove` are correct on both planes; all probe tags removed cleanly.
- `pending_writes` rows are **updated in place**, not appended — after add→rename→remove the marker
  held only the final `remove`, and the mirror matched the web plane exactly.
- `journal undo` correctly reversed a tag add.
- `import pdf` key resolution did not mis-attribute to either of two older same-title items.
- Library end state identical to start: 2621 records, 786 distinct tags, no residue.

## Checked and found NOT to be bugs

Recorded so nobody re-investigates them:

- **`items children`** returns a normal `{meta, results:[…]}` envelope and works correctly. An earlier
  session logged a jq failure against it; that was `.[]?` applied to an object — my error.
- **`tags audit fix` is listed** in `tags audit --help` under `Available Commands`. An earlier grep of
  mine missed it because `Examples:` sits between the usage block and the command list.
- **392 keys appear as both a `collections` row and an `items` row** in `resources`. Not corruption —
  Zotero's collection and item keyspaces are independent, and the store keys on
  `(resource_type, id)`, so both rows coexist correctly.
- **`items get` returning an object while `items list` returns an array** is unchanged and still worth
  normalising (R1-10), but it is a consistency wart, not a defect.

## Suggested order

1. **New-2** — data loss, and the fix is small. Ship before anything else.
2. **New-1** — silently neuters the P0 fix on freshly-written items; `tags audit fix` aborts on one bad row.
3. **New-3** — the frozen cursor is the reason New-1 is permanent rather than transient.
4. **New-4**, **New-5** — both cheap, both block agent workflows.
5. **New-6/7/8/9**, then the still-open items from reports #1 and #2.
