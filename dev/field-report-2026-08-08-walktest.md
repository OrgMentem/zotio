# zotio field report #4 — 2026-08-08 — walk-test of the deployed build

Adversarial walk-test of the **deployed** `zotio 0.16.1-dev+fieldreports` (the installed binary,
not the working tree), covering the fixes for reports #1–#3 and hunting for new breakage.

Library `5847066`: 928 top-level items / 2621 records / 786 distinct tags.
Everything below was driven through zotio; `api.zotero.org` and `localhost:23119` were read directly
only as independent oracles. **All mutations were reverted — the library is byte-identical to the
pre-test baseline** (totals at the end).

Note on drift: this was run against the binary as deployed at test time. Findings marked
`[re-check]` touch code six subagents were editing concurrently, so confirm them against the next build.

---

## Correction to field report #3, New-1 — my root cause was WRONG

**Report #3 attributed the silent `tags rename` no-op to version-less write-through mirror rows.
That is not the cause, and any fix built on it will not close the bug.** I confirmed version-less
rows correlate with the failure, then broke the correlation: rows carrying a version (99, 100, 101,
102) still produced `selected: 0`, and the true discriminator turned out to be elsewhere. Details in
W-1. I apologise for the misdirection — the correlation was real, the causation was not.

---

## W-1 — [HIGH] `tags rename` silently renames nothing: two stacked causes

`tags rename` selects with a **live** `GET /items?tag=<name>` against the **read** plane
(`localhost:23119`), while the tag was written to the **write** plane (`api.zotero.org`). Two
separate failures follow, and both surface identically as `selected: 0, applied: 0, ok: true, exit 0`.

### Evidence table (the (a)–(d) you asked for)

Two probes, each tagged via `zotio items tags add`, neither tag ever queried before it existed:

- **E** = `PB6FSP5G`, a long-standing item the desktop has held for months, tag `wt-clean-exist`
- **N** = `USJWA7X6`, created seconds earlier by zotio on the write plane, tag `wt-clean-new`

| when | probe | (a) `items get` auto / local | (b) local `?tag=` | (c) web `?tag=` | (d) selected (cached) | selected (`--no-cache`) |
|---|---|---|---|---|---|---|
| t0 | **E** (old item) | title ✓ / title ✓ | **0** | **1** | 0 | 0 |
| t0 | **N** (new item) | title ✓ / title ✓ | **0** | **1** | 0 | 0 |
| t+15s | **E** | — | **1** | 1 | **0** | **1** |
| t+15s | **N** | — | **1** | 1 | **0** | **1** |

### Cause 1 — plane split. Your hypothesis: CONFIRMED, but narrower and broader than stated

At t0, (b) is empty and (c) is 1 — exactly your prediction, so **write commands must select from the
plane they write to**. Two corrections to the framing:

- **It is not about items the desktop has not pulled.** `PB6FSP5G` has been in the desktop for
  months and failed identically. The invisible object is the freshly written **tag**, not the item.
  So the correct statement is: *`tags rename` cannot rename any tag the read plane has not yet
  learned about, on any item.*
- **It is transient.** Zotero's auto-sync closed the gap in **under 15 seconds** here. On its own
  this would be a brief, self-healing window.

`items get` sees both items on every data source throughout (write-through works), so the item is
never invisible — only the tag query is.

### Cause 2 — the response cache. Not in your hypothesis, and this is the durable one

At t+15s the read plane has the tag (b = 1) and `--no-cache` correctly selects 1 — but the **default
cached path still returns 0**, and stays there. On the previous build I watched it hold 0 for 60s+
while `--no-cache` returned 1 the whole time.

The invalidation is keyed to writes on the item, and the ordering defeats it:

1. `items tags add` writes the tag — invalidates nothing, because nothing is cached yet.
2. Something reads `/items?tag=X` during the propagation window → **the empty result is cached**.
3. Propagation completes; the cache is never revisited.
4. Every later rename serves the cached empty. `zotio sync` does **not** clear it. Only a *further*
   write to the same item does (verified: adding an unrelated tag flipped cached selection 0 → 1).

### Why this matters: preview-then-apply poisons itself

The documented, preview-first workflow is the trigger. Same command, same item, same 30s wait — the
only difference is whether a preview ran during the propagation window:

```
A: add tag → preview (selected: 0) → wait 30s → apply
   → {"selected": 0, "applied": 0, "ok": true, "exit": 0}     tag unchanged: 'wt-preview-flow'

B: add tag →         (no preview)  → wait 30s → apply
   → {"selected": 1, "applied": 1, "ok": true, "exit": 0}     tag renamed:   'WT-Noprev-Flow'
```

**Previewing causes the apply to silently fail.** A careful user is punished for being careful, and
the tool reports success either way.

### Suggested fix

Select from the write plane (your conclusion — right for cause 1), and either exempt selection
queries from the response cache or invalidate `/items?tag=*` on any tag write (cause 2). Fixing only
cause 1 leaves the preview trap intact if the write-plane query is itself cached.

---

## W-2 — [HIGH] `items new` on the connector route reports failure but creates the item

```
$ zotio items new --item-type journalArticle --field "title=ZOTIO WALKTEST SCRATCH" --yes
{"ok": false, "result": {"summary": {"attempted":1, "applied":0, "failed":1},
 "items":[{"status":"failed","reason":"connector saveItems: HTTP 500: "}]}}
```

`GPMXW2IS` was created on the server at exactly that moment. The command reported a hard failure
and created the item anyway — a false-negative write. I only caught it because my end-of-run
fingerprint check showed 2622 records instead of 2621.

Blast radius: an agent that retries on `failed` mints a duplicate every attempt, and the retry loop
looks like it is making no progress. `--via web` works correctly and returns the key; only the
connector route (which is the `auto` default when a local base URL is configured) is affected.

Fix: on a connector error, re-read before reporting `failed`, or report an indeterminate status that
tells the caller not to retry blindly.

---

## W-3 — [MED] `items restore` is not journaled and uses a non-standard envelope

```
$ zotio items restore HHP74G2C --yes --json
{"action":"restore","key":"HHP74G2C","resource":"items","status":204,"success":true}
```

No `schema_version`, `ok`, `plan`, `result`, or `journal` — and no `run_id`, so the restore is
invisible to `journal list`. `items delete` **is** journaled, so the journal records a trash with no
record of its reversal: replaying or auditing it gives the wrong library state. `.result.items[0]`,
which works for `move`/`rename`/`tags add`/`tags remove`/`delete`/`import pdf`, throws here.

---

## W-4 — [MED] `items trash` shows nothing while an item is trashed

With `HHP74G2C` genuinely trashed (`deleted: 1` on the write plane):

```
web plane /items/trash        → HHP74G2C
local plane /items/trash      → (empty)
zotio items trash             → (empty)      # auto
zotio items trash --data-source live   → (empty)
zotio items trash --data-source local  → HHP74G2C   ✓
```

So immediately after `zotio items delete`, `zotio items trash` cannot show you what you just
trashed, and `--data-source local` — the mirror, normally the *less* current source — is the only
one that is right, because write-through put it there. The default is the wrong choice here.

---

## W-5 — [MED] Corrupt row in the mirror: a resource name stored as a row id

```
sqlite> select id, data from resources where resource_type='items-trash' and id='items-trash';
items-trash|[]
```

An empty API response array was persisted as a row keyed by the resource name. It surfaces in
`items trash --data-source local` output as an entry with no `data` object, so
`jq '.results[].data'` throws `cannot index [] with "data"`. Same family as the `/items/<key>/tags`
path-parse bug (report #3 New-4): a response is being written where a record belongs.

---

## W-6 — [LOW] Renaming a tag to itself performs real writes

```
$ zotio tags rename --from 'Ctrl-Probe' --to 'Ctrl-Probe' --yes
"selected": 2, "applied": 2
```

Two PATCHes that change nothing: item versions bump, the journal gains a meaningless run, and other
clients see spurious modifications. Should be `no_op` with a `same_name` code.

---

## W-7 — [LOW] `selected: 0` cannot be distinguished from "no such tag"

```
$ zotio tags rename --from 'no-such-tag-xyz123' --to 'X' --yes
"selected": 0, "applied": 0, ok: true, exit 0
```

Byte-identical to W-1's silent failure. Whatever happens with W-1, a rename that matches nothing
while the tag demonstrably exists on the write plane must say so — a `code` on the empty result
(`tag_not_found` vs `no_items_on_read_plane`) would make both diagnosable.

---

## W-8 — [LOW] Mutation results embed the entire item payload

`tags audit fix` returns, per applied op, the full item JSON (all fields, creators, collections,
links) inside `result.items[].item`. For a 53-group merge that is megabytes of output for
information the caller already has. A key plus the changed field would do.

---

## W-9 — [LOW] `items new` reports the item **type** as the op key

```
{"op_id":"items.new","key":"journalArticle","status":"applied","reason":{"key":"HHP74G2C","via":"web"}}
```

Every other command puts the item key in `key`; here it holds `journalArticle` and the real key is
buried in `reason.key`.

---

## W-10 — [LOW] `journal` is `null` on no-op mutations

`items move` on an already-filed item returns `"journal": null`, so `jq '.journal.run_id'` throws
rather than yielding null. Defensible (nothing was written), but it means agents cannot use one
extraction path across a command's outcomes. Emit `{"run_id": null}` or omit consistently.

---

## Confirmed fixed in this build

- **New-2**: `items delete` trashes (`deleted: 1`), `items restore` reverses it, and `--permanent`
  is gated — `Error: destructive_opt_in_required` without `--allow-destructive`.
- **New-4**: `items tags list` returns data on `auto`, `live` and `local`.
- **New-5**: `journal.run_id` populated on `items new`, `items delete`, `items tags add/remove`,
  `items move` (real change), `tags rename`, `tags audit fix`. Live example:
  `20260808T043155Z-3132854d`.
- **New-9**: `sync --help` now names the plane and direction ("Mirror data from the Zotero read API…
  One direction only"). **Closed.**
- **Mirror restoration**: `tags audit` reports 786 tags / **0 duplicate groups**, matching the live
  library exactly. The 53 phantom groups are gone.
- **`doctor`**: real row counts, `last delta`, and `pending_writes` with an explanation.
- **`tags audit fix`** applies end-to-end (`applied: 1`, run_id present) and dedupes correctly when
  renaming into a tag the item already carries.
- **Validation**: empty `--from` is rejected (`required flag "from" not set`).

## Restoration

| metric | before | after |
|---|---|---|
| records | 2621 | 2621 |
| distinct tags | 786 | 786 |
| trash | empty | empty |
| tag-map fingerprint | `b5663882922c13c7` | `b5663882922c13c7` ✓ |
| key delta | — | none |
| tag deltas | — | none |

Mutations made and reverted: 2 scratch items created and permanently deleted (one of them W-2's
false-negative), 9 scratch tags added and removed, 1 trash/restore round trip ×3, 2 collection moves
reversed, 1 controlled tag merge reverted.
