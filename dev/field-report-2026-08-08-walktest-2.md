# zotio field report #5 — 2026-08-08 — walk-test 2

Second walk-test, against deployed `0.16.1-dev+fieldreports` at commits `c9a980c` → `fe5a029` →
`265db78` (the build moved twice mid-pass; each finding below states what it was tested on).

Library `5847066`. **All mutations reverted — 2621 records / 786 tags / empty trash /
fingerprint `b5663882922c13c7`, identical to baseline.**

Ten findings confirmed fixed. **Ten new findings, four HIGH** — three of them in surface that had
never been exercised before this pass.

---

## Confirmed fixed

| finding | evidence |
|---|---|
| **W-1 both causes** | A/B rerun: preview selects 1 **immediately**, apply lands (`applied: 1`) whether or not a preview ran. Hardest case also passes: brand-new item, tag absent from the read plane (`local ?tag= 0`), three previews all select 1, zero-wait apply succeeds. |
| **W-1 cause 2, ordinary reads** | `items list --tag X` right after `items tags add X` returns 0 while the read plane genuinely lags, then flips to 1 at t+20s **with no intervening write**. The empty is no longer cached. |
| **W-2** | Connector failure now reports `failed` **and** creates nothing — keyset diff confirms 0 items created. Consistent. |
| **W-3** | `items restore` on the standard envelope: `.result.items[0].key`, `journal.run_id`, and it appears in `journal list` as `items.restore applied=1`. |
| **W-5** | `select count(*) from resources where id='items-trash'` → 0. No malformed rows anywhere in the store. |
| **W-6** | `--from Depression --to Depression` → `no_op: 1, applied: 0`, `code: same_name`, zero writes. |
| **W-7** | `--from no-such-tag-xyz123` → `code: tag_not_found`. |
| **W-8** | `tags audit fix` on a real 2-item merge: 946 bytes total, no embedded item payload. |
| **W-10** | No-op `items move` → `"journal": {"run_id": null}`. |
| **R1-12 `which`** | "add an item to a collection" → `items add-to-collection` (48), `items move` (48). "rename a creator" → `creators rename` (9). |
| **R2-9 counts** | `library health` "933 top-level items (4313 mirrored rows)" matches `items audit` "Scope: 933 top-level items" and `top_level_items: 933`. |
| **R2-3 `--prefer`** | On a space-separated tag all four policies differ correctly: sentence `Wt2 alpha beta`, title `Wt2 Alpha Beta`, lower `wt2 alpha beta`, frequency `WT2 Alpha Beta`. |
| **R2-5 `creators rename`** | Works: `applied: 1`, preserves creator order and type, journaled, reverts cleanly. |
| **Envelope change** | `.results` is an array for `items get`, `items list`, `items find --doi`, `search`, `collections list`, `collections get`, `tags list`, `tags get`, `searches get`, `items children`, `items trash`, `items recent`. `jq .results[0].key` works uniformly across all twelve. |

---

## X-1 — [HIGH] `import pdf --on-duplicate` does nothing; the `skip` default creates duplicates

R2-7 is not closed. The DOI `10.1037/0021-9010.87.4.611` already had **two** copies in the library
(`4LIPWP5Y`, `PHMIJWH3`). Importing the same PDF once per mode:

| mode | reported | items created |
|---|---|---|
| *(default)* | `status: recognized`, `applied` | `RHGLCX3Y` + attachment |
| `skip` | `status: recognized`, `applied` | `VDXKSLMZ` + `A6FJ48VQ` |
| `attach` | `status: recognized`, `applied` | `NFCBCLNK` + `TFXNS4MI` |
| `create` | `status: recognized`, `applied` | `VNPGMU96` + `4A3L2P3N` |

**All four create a new item.** `skip` — which the help text names as the default — created a third
copy of an item the library already held twice, and reported plain success with no duplicate
mentioned anywhere in the payload. `attach` created a *fifth* standalone item rather than attaching
the PDF to either existing one; its DOI is the duplicate DOI, so detection is not merely mis-routing,
it is not running.

(My first measurement of `attach` showed "created: none" — that was a 3-second propagation window,
not a pass. The item and its attachment appeared shortly after. Worth a longer settle in your tests.)

This is the finding from report #2 that motivated the flag, so the flag needs a test that asserts
`skip` produces zero new keys for a DOI already present.

## X-2 — [HIGH] The `creators audit` merge plan proposes merging different people

`creators audit` now emits 304 ready-to-run `creators rename` commands across three classes:

```
creator_variant_exact       15
creator_variant_initials    59
creator_variant_ambiguous  230
```

**225 of the 304 merge names whose given names differ.** The `ambiguous` class is matching on
surname alone:

```
zotio creators rename --from 'Zhengguang Liu' --to 'Chao Liu'
zotio creators rename --from 'Yangyang Liu'   --to 'Chao Liu'
zotio creators rename --from 'Huaigui Liu'    --to 'Chao Liu'
zotio creators rename --from 'Yang S. Liu'    --to 'Chao Liu'
zotio creators rename --from 'Qimin Liu'      --to 'Chao Liu'
zotio creators rename --from 'Li Liu'         --to 'Chao Liu'
zotio creators rename --from 'Jiaojian Wang'  --to 'Meifang Wang'
```

Six distinct researchers collapsed into one. The commands are nested under the class heading, but
they are formatted identically to the safe `exact` ones and are directly pasteable. `tags audit`
established the pattern that a printed plan is meant to be run — and now has `tags audit fix` to run
it in bulk. If a `creators audit fix` ever lands with this plan behind it, it will silently destroy
authorship metadata across the library.

Direction is also inconsistent, and sometimes discards information:

```
'Martin H. Teicher' -> 'M. H. Teicher'      # full name collapsed into initials
'Mark A. Griffin'   -> 'Mark Griffin'       # middle initial dropped
'Adam Rock'         -> 'Adam J. Rock'       # …but here the fuller form wins
```

Frequency picks the target, as with tags, which is wrong for names: the canonical form should be the
most complete one, not the most common.

Suggested: never emit rename commands for `ambiguous`; require `--include-ambiguous` to see them and
mark them as unsafe; prefer the most complete name as the target; and never propose a merge where
given names differ beyond an initial expansion.

## X-3 — [HIGH] W-4: the union misses the fresh trash and surfaces four phantoms

Tested on `265db78`. The exact case you asked me to hit — `items delete` then `items trash` on the
default source:

```
$ zotio items delete JCKBB4SN --yes          # web plane: deleted=1 confirmed
$ zotio items trash
IXEGGGXY  S4ZSV95V  R4MXKSNG  K6MX6H44        # JCKBB4SN ABSENT
note: 4 trashed item(s) came from the local mirror; the Zotero read API has not caught up with them yet
```

Two problems:

- **The freshly trashed item is in neither arm.** The read plane has not synced it (0 entries), and
  the mirror's `items-trash` rows were not updated by the write. It appears at t+15s once Zotero
  syncs down, so the union's read arm works — but the 0–15s window, which is the whole point of the
  fix, is still blind.
- **All four entries the mirror contributes are phantoms.** `IXEGGGXY`, `S4ZSV95V`, `R4MXKSNG`,
  `K6MX6H44` are **HTTP 404 on both planes** — they do not exist anywhere. They are June rows. The
  stderr note describes them as items "the Zotero read API has not caught up with", which is exactly
  backwards: the read API is right and the mirror is stale.

**Regression:** in walk-test 1, `items trash --data-source local` *did* show the just-trashed item
(write-through recorded it). It no longer does — `--data-source local` now returns only the four
phantoms. So the write-through into `items-trash` was lost somewhere between builds.

## X-4 — [HIGH] The mirror never reaps deleted items, and serves them to offline reads

Four items sit in the mirror that return **404 on both planes**:

```
3UINT4UH  'Making sense of recommendations'   synced 2026-06-29
GPMXW2IS  'ZOTIO WALKTEST SCRATCH'            synced 2026-08-08
USJWA7X6  'WALKTEST PLANE PROBE'              synced 2026-08-08
HHP74G2C  'ZOTIO WALKTEST SCRATCH'            synced 2026-08-08
```

Three are items I permanently deleted **through zotio** (`items delete --permanent`); the row stayed
behind. `3UINT4UH` predates this session, so ordinary user deletions leak the same way. Neither the
delete path nor `sync` reaps them.

Consequences:

- `zotio items list --data-source local` returns `3UINT4UH` (at index 253) and
  `zotio search "Making sense of recommendations" --data-source local` finds it. Offline reads serve
  items that no longer exist; following one up gives `items get 3UINT4UH → HTTP 404`.
- Every mirror-derived count drifts upward: the mirror holds 933 top-level bibliographic items where
  the write plane holds **929**. `library health` and `items audit` both report the inflated 933.

So R2-9's "one count definition" is internally consistent but counts four things that are gone.

## X-5 — [MED] W-9's journal half is still broken, and `items new` is not undoable

The rendered envelope reports the real key, but the journal does not:

```
$ zotio journal show 20260808T050825Z-b743d455 --json | jq '.ops[0] | {key, kind}'
{"key": "journalArticle", "kind": "item_create"}
```

and undo refuses outright:

```
$ zotio journal undo 20260808T050825Z-b743d455 --yes
warning: skip item_create journalArticle (op items.new): change on field "source" is not reversible
Error: journal.undo: 1 op(s) refused; nothing was reversible          # exit 13
```

Two separate things: the key is still the item **type**, and the run records a synthetic
`field: "source"` change rather than a create, so there is nothing for undo to reverse. Since
`items delete` is now a reversible trash, an `item_create` could plausibly be undone by trashing the
created key — but only once the journal records that key.

## X-6 — [MED] `schema new-item-template` 404s

```
$ zotio schema new-item-template --item-type journalArticle
Error: GET /items/new returned HTTP 404: No endpoint found
```

`/items/new` is **200 on the web plane** and **404 on the local plane**. Same class as report #3's
New-4 (`/items/<key>/tags`): a read whose endpoint only the write plane implements, with no
fallback. It is also one of the commands listed under the envelope change, so it is presumably
believed to be working.

## X-7 — [MED] Three commands violate the new envelope contract

`.results` is not always an array:

```
items collections-of  → bare array, no {meta,results} wrapper
journal list          → bare array, no {meta,results} wrapper
items audit           → no .results key at all (top-level report fields)
```

The first two are the exact shape the change set out to eliminate. `items audit` is report-shaped
rather than list-shaped, so it may be a deliberate exemption — if so the contract should say which
commands are exempt, because `jq .results[]` is now documented as universal.

## X-8 — [LOW] `items update --stdin` still uses the legacy envelope

```
$ zotio items update JCKBB4SN --stdin --yes --json
{"action":"patch","path":"/items/JCKBB4SN","resource":"items","status":204,"success":true}
```

No `ok`, `plan`, `result`, or `journal` — the same shape W-3 just removed from `items restore`. The
run **is** journaled (`items.update applied=1 ok`), so only the response is wrong, but a caller
cannot get the `run_id` from the response to undo its own write.

## X-9 — [LOW] `items new` fails on its default route

`items new` with no `--via` (auto → connector) fails **every** time with
`connector saveItems: HTTP 500`, with Zotero running and the connector answering `/connector/ping`
with 200. Only `--via web` succeeds. W-2 fixed the *reporting* of this; the underlying default path
for creating an item still does not work.

## X-10 — [LOW] `--prefer title` does not capitalise after hyphens

`wt2-Case` → `Wt2-case` under both `--prefer sentence` and `--prefer title`; conventional Title Case
gives `Wt2-Case`. Matters for real tags like `meta-analysis` and `Carhart-Harris`.

---

## Suggested order

1. **X-1** — `--on-duplicate skip` actively creates the duplicates the flag exists to prevent.
2. **X-2** — a plan that merges distinct researchers; gate `ambiguous` before anything can bulk-apply it.
3. **X-4** — deleted items served by offline reads and inflating every count.
4. **X-3** — the trash union needs the write-through arm back, and the phantom rows reaped (same root as X-4).
5. **X-5**, **X-6**, **X-7** — then X-8/9/10.

## Restoration

| metric | before | after |
|---|---|---|
| records | 2621 | 2621 |
| distinct tags | 786 | 786 |
| trash | empty | empty |
| fingerprint | `b5663882922c13c7` | `b5663882922c13c7` ✓ |
| key delta | — | none |
| tag deltas | — | none |

Reverted: 7 scratch items created and permanently deleted, 6 stray tags removed from 3 real items,
1 trash/restore round trip, 1 creator rename reverted, 2 controlled tag merges reverted.
