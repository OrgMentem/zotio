# zotio field report #7 — 2026-08-08 — walk-test 4

Fourth walk-test, against deployed `0.16.1-dev+fieldreports` at `668abd2`.

Library `5847066`. **All mutations reverted — 2621 records / 786 tags / empty trash /
fingerprint `b5663882922c13c7`.** Mirror and write plane agree at 927 top-level each.

All walk-test-3 findings verified fixed. **Seven new findings**, six of them in the unexercised
surface, plus one correction to a number in the hand-off.

---

## Verified fixed

| finding | evidence |
|---|---|
| **X-9** | `items new` on the **default route** now succeeds: `ok: true`, `key: RYIY2QHR` (real key), journal records `RYIY2QHR`, `journal undo` → item returns with `deleted: 1`. Same on `--via web` (`85FTGWN3`). Both routes round-trip. |
| **N-1** | `annotations search`, `annotations timeline`, `capabilities`, `groups list`, `profile list`, `tags audit` all `.results` arrays. `annotations export` correctly remains a bare array (documented exemption). |
| **N-2** | text runnable **73**, JSON with command **73**, unsafe carrying a command **0**. Surfaces agree. |

---

## Correction: the 705-tag figure in the hand-off is a paging artifact, not a denominator

Your note said raw endpoints report 705 tags vs my 786, "different denominators". The top-level
945-vs-927 half is right — `items/top` counts standalone notes and attachments. The tag half is not:

```
GET /tags, fully paged        793 rows, 786 distinct
derived from item payloads    786
in items but not in /tags     0
in /tags but not in items     0

naive GET /tags (no paging)    25 rows
GET /tags?limit=100           100 rows
```

No denominator produces 705: top-level-only is 785, top-level-biblio-only is 785, everything is 786.
Fully paged, `/tags` agrees with my count exactly. Worth knowing because if 705 was used to confirm
your restoration, that check was measuring page size rather than library state.

The 793-vs-786 gap is real and benign: **7 tags exist as both automatic and manual**
(`Behavior`, `Depression`, `Leadership`, `Management`, `Research`, `Social networks`,
`Structural equation modeling`) — e.g. `Depression` is `type 1` on 10 items and `type 0` on 5.
`tags rename` handles both correctly (selects 15, 5, 3 respectively — the sums).

### N4-1 — [LOW] `tags list` and `tags audit` disagree on how many tags exist

`tags list` returns **793** rows, `tags audit` reports **786** total. Same tool, same library, two
answers, because `tags list` passes the raw per-`(tag, type)` feed straight through. Either dedupe by
name or surface `meta.type` in `--plain` so the duplicate rows are explicable.

---

## N4-2 — [MED-HIGH] `items delete` reports success for an item it did not delete

The inverse of the propagation lag we have been chasing all series: `items new` on the default route
creates in **Zotero desktop**, which syncs **up** to the write plane, measured here at **~17s**.
Write commands target the write plane, so there is a window where a just-created item is invisible
to them. Three commands disagree about what to do:

```
items tags add <new key>   → rc=3  Error: GET /items/<key> returned HTTP 404: Item does not exist
items move     <new key>   → rc=3  Error: GET /items/<key> returned HTTP 404: Item does not exist
items delete   <new key>   → rc=0  ok:true  no_op:1  code:"already_deleted"
                                    message:"item does not exist on the write plane"
```

`tags add` and `move` are honest. `items delete` reports **success**, and the reason code asserts the
item is *already deleted* when it was created seconds earlier and is not deleted at all.

Proof it is a false success — `SDLDFA9W` was created, immediately deleted (reported
`ok: true, already_deleted`), and then materialised on the write plane, untrashed:

```
SDLDFA9W: EXISTS  deleted=None  title='WT4 DELETE LAG'
```

An agent doing create → delete cleanup believes the item is gone; it is live in the library. This is
the same false-success class as W-2, in a command whose whole purpose is removal.

Fix: `already_deleted` must mean "the write plane says `deleted: 1`". A 404 on the write plane is a
different state — either error like `tags add`/`move` do, or return a distinct code
(`not_on_write_plane`). Do not report `ok: true`.

## N4-3 — [MED] `sync --full` does not reap `items-trash`, so the X-3 phantom is back

The mark-and-sweep covers `items` but not the trash mirror. One row survives:

```
mirror items-trash rows      ['WH3JEEWH']
GET /items/WH3JEEWH          HTTP 404
web /items/trash             []
local plane /items/trash     0 entries
zotio sync --full            → row still present
zotio items trash            → ['WH3JEEWH']
note: 1 trashed item(s) came from the local mirror; the Zotero read API has not caught up with them yet
```

`WH3JEEWH` is your own X-9 verification probe — trashed by `journal undo`, then removed from the
write plane out of band. Exactly the tombstone mechanism X-4 fixed for `items`, still live for
`items-trash`, producing exactly the X-3 symptom: `items trash` lists an item that exists nowhere,
under a note claiming the read API is merely behind.

Reachable through zotio alone: trash an item, then `items delete --permanent`. The permanent-delete
reap removes the `items` row and leaves the `items-trash` row.

## N4-4 — [MED] `analytics` is generator scaffolding: two of three flags are inert and the examples are from another product

```
$ zotio analytics --help
  # Count records by type
  zotio analytics --type messages
  # Group by a field
  zotio analytics --type messages --group-by author_id
  # Top 10 most frequent values
  zotio analytics --type messages --group-by channel_id --limit 10 --json
```

`messages`, `author_id`, `channel_id` are not Zotero concepts — this is untouched CLI Printing Press
boilerplate from another domain.

Behaviour matches the examples' provenance rather than the library:

- **`--group-by` is inert.** Byte-identical output for `year`, `itemType`, `creator`, `collection`,
  `nonsense`, and `!!!`. No validation, no effect.
- **`--limit` is inert.** Byte-identical for `0`, `-5`, `1`, `1000`, despite a documented default of 25.
- **`--type` matches the mirror's resource kind, not item type.** `--type items` → 4307;
  `--type journalArticle` → **`count: 0`**; `--type bogusType` → `count: 0`. A user asking how many
  journal articles they have gets zero, silently.

Default output is a row census of the mirror (`collections: 77, items: 4307, items-trash: 1,
schema: 40, …`) — a debug view presented as analytics. With `which` now walking the whole tree this
is reachable by natural-language routing.

Suggest either implementing it against library concepts or removing it; a command that answers
plausibly and wrongly is worse than a missing one.

## N4-5 — [MED] `workflow archive` requests four endpoints that do not exist

```
$ zotio workflow archive           # exit 13
Archive incomplete: 5177 items stored across 8 resources
archive incomplete: items-trash: fetching: GET /items-trash returned HTTP 404: No endpoint found
; schema: fetching: GET /schema returned HTTP 404: No endpoint found
; schema-creator-fields: fetching: GET /schema-creator-fields returned HTTP 404: No endpoint found
; schema-item-fields: fetching: GET /schema-item-fields returned HTTP 404: No endpoint found
```

It is using zotio's internal **resource names** as API paths. The real endpoints are `/items/trash`
and the global schema endpoints (`/itemFields`, `/creatorFields`, …) — which `AGENTS.md` already
warns are global and not under the library prefix, and which `schema drift` handles correctly via
`stripLibraryPrefix`. Four of twelve resources fail on every run.

## N4-6 — [LOW] `export --help` advertises flags its only subcommand rejects

```
export parent flags     --format  --limit  --no-cache  --output
export snapshot flags   --limit  --output  --page-size  --resume
$ zotio export snapshot --output s.csv --format csv
Error: unknown flag: --format
```

`export` has exactly one subcommand, so the parent's `--format` and `--no-cache` are unreachable.
Output is always JSONL regardless of the output filename's extension.

## N4-7 — [LOW] Every export leaves two stray files, and the manifest is misnamed

```
$ zotio export snapshot --output x.json --limit 2
x.json             4650 bytes   the export (JSONL)
x.json.lock           0 bytes   lock file, not removed on success
x.json.lock.json    783 bytes   the manifest
```

The 0-byte lock is never cleaned up. The manifest is a real, useful artifact
(`schema_version`, `generated_at`, `scope`, `format`, `count`, `content_sha256`, `items`) but it is
built from the **lock** path rather than the output path, so it lands at `<output>.lock.json`. For
the full-library export that is a 788 KB file named as if it were a lock. Suggest
`<output>.manifest.json` and removing the lock on success.

## N4-8 — [INFO] `vault` refuses cleanly

`vault audit`, `vault conflicts`, and `vault sync --dry-run` all exit 1 with
`no vault directory: pass --out <dir> or set [vault].root in config`. Correct, actionable, no crash.

---

## Suggested order

1. **N4-2** — false success on a removal command; agents will act on it.
2. **N4-3** — same tombstone class as X-4, one resource short of complete.
3. **N4-5**, **N4-4** — a partially-broken archive and a command that answers wrongly.
4. **N4-6/7**, then N4-1.

## Restoration

| metric | before | after |
|---|---|---|
| records | 2621 | 2621 |
| distinct tags | 786 | 786 |
| trash | empty | empty |
| fingerprint | `b5663882922c13c7` | `b5663882922c13c7` ✓ |
| key delta | — | none |
| tag deltas | — | none |
| mirror vs web top-level | — | 927 = 927 |

Mutations made and reverted: 8 scratch items created and permanently deleted, 2 `items new` →
`journal undo` round trips, 2 export snapshots written to temp dirs and removed. One artifact I could
**not** revert: the `items-trash` mirror row `WH3JEEWH` (N4-3) — it is a phantom with no
corresponding item on either plane, and no zotio command reaps it.
