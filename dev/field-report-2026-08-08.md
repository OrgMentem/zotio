# zotio field report — 2026-08-08

Environment: zotio 0.16.1, macOS 25.6.0, Zotero desktop running with local API enabled.
Library: personal user `5847066`. `base_url = http://localhost:23119/api/users/0` (reads), writes auto-routed to `api.zotero.org`.
Session task: import 2 PDFs from `~/Downloads`, then file each into a collection. Task succeeded; everything below is friction hit on the way.

Every finding was reproduced against this live install. Nothing here is speculative unless tagged `[INFERENCE]`.

Likely subsystems, for whoever picks this up: findings 2–5 all land on the local-read-parity
path (`internal/store/query.go`, `resolveLocal*`) — read `dev/adr/0002-local-read-parity-subsystem.md`
first, since that ADR deliberately scopes parity per-resource and these are parity/coherence gaps
rather than an argument for a generic query planner. Finding 1 is an output-layer fix, not a parity one.

---

## P1 — Silent wrong answers / broken agent contract

### 1. ~~Mutating commands write a non-JSON line to stdout~~ — RETRACTED, NOT A BUG

**This finding was wrong. Do not action it.** See `dev/field-report-2026-08-08-library-hygiene.md`.

zotio's stream discipline is correct: the `→ writing via Zotero Web API: …` banner goes to **stderr**,
and stdout is pure JSON. My original repro piped `2>&1` into `jq` — I merged the streams myself.

```
$ zotio items move 2GFPDDBZ --to 55C8PBTZ --yes --json 2>/dev/null | head -c 40
{ "schema_version": 1, "ok": true, "oper
```

Retained only so the numbering below stays stable against anything already referencing it.

### 2. Read-after-write is incoherent across the two planes

Reads resolve against the local desktop API / SQLite store; writes go to the web API. Nothing invalidates the store after a write, so the next read contradicts the write that just succeeded:

```
$ zotio items move 2GFPDDBZ --to 55C8PBTZ --yes     # applied
$ zotio items collections-of 2GFPDDBZ
[]
$ curl -H "Zotero-API-Key: $K" .../items/2GFPDDBZ | jq .data.collections
["55C8PBTZ"]
```

Store confirms the stale copy:

```
$ sqlite3 data.db "select json_extract(data,'$.data.collections') from resources where id='2GFPDDBZ'"
[]
```

`[]` is a wrong answer, not a degraded one — there's no warning that the answer came from a store predating the write.

Fix options, cheapest first: (a) write-through — on successful mutation, upsert the returned record into `resources`; (b) invalidate affected rows and let the next read fall through to live; (c) at minimum, have mutating commands print the post-state so the agent doesn't need a follow-up read.

### 3. `doctor` reports "0 rows" for resources that hold thousands of rows

```
$ zotio doctor
  OK Cache: fresh
      - collections: 0 rows, 0s
      - items: 0 rows, 0s
```

Actual store:

```
$ sqlite3 data.db "select resource_type, count(*) from resources group by resource_type"
collections|469
items|4306
tags|841
```

The reported number is `sync_state.total_count`, which holds the **last run's fetched delta**, not rows in the store. Reads as a catastrophically empty cache when the cache is fine. This is what sent me down a 20-minute wrong path debugging a non-existent sync failure.

Fix: count rows in `resources` grouped by `resource_type`. If the delta count is worth showing, label it `last delta:`.

### 4. `sync` reports `success` while fetching nothing, and stores a cursor from the wrong version space

```
$ zotio sync
{"event":"sync_complete","resource":"items","total":0,"duration_ms":47}
{"event":"sync_summary","total_records":166,"resources":8,"success":8,"warned":0,"errored":0}
```

`success: 8, errored: 0` for a library with 4306 stored items and 2 items created minutes earlier.

Underlying hazard — `sync_state.library_version = 12689` is a **web API** version number, but the sync target is the **local** API, whose version space is unrelated:

```
local  2GFPDDBZ version=24
web    2GFPDDBZ version=12704
curl 'localhost:23119/api/users/0/items?since=12689&limit=5' | jq length   → 0
curl 'localhost:23119/api/users/0/items?since=0&limit=5'     | jq length   → 5
```

Also: the local API returns **no** `Last-Modified-Version` header on `/items` (the web API returns `12705`), so whatever populates that cursor isn't reading it from the local plane.

`[INFERENCE]` Rows did get their `synced_at` refreshed today, so sync currently appears to do a full refresh and the bad cursor isn't breaking it *yet*. But a 12689-vs-24 cursor against a `?since=` endpoint is a latent silent-no-op sync. Worth a targeted test: sync, mutate, sync, assert the delta lands.

Fix: namespace the cursor per plane (`plane + library`), and refuse to apply a cursor recorded against a different base URL. Separately: `sync` reporting `success` with `total: 0` for a non-empty upstream deserves at least a WARN.

### 5. "Cache: fresh" measures the last *attempt*, not the last data change

`stale_after: 6h`, and the age resets on any sync run regardless of whether records changed. Before I ran anything, doctor showed `19m0s` and `OK`; `items-trash` and `searches` rows in the store are actually from **2026-06-29**, `tags` from **2026-07-11**. Freshness should track the newest `synced_at` of stored rows for that resource, not the last poll.

---

## P2 — Agent ergonomics

### 6. `import pdf` doesn't return the keys it just created

Result payload gives `session`, `status: recognized`, `title`, `item_type` — but no keys. To file the imports I had to `zotio search "<title>"`, take the attachment key, follow `links.up` to find the parent. Three extra round trips for data the command already had.

Add per result: `item_key`, `parent_key`, `attachment_key`, `doi`. This is the single highest-value change here for agent workflows.

### 7. `import pdf` has no `--collection`

Import always lands in My Library root, so every ingest is a two-step `import pdf` → `items move`. Note the global `--connector-target` help text already advertises *"overrides `--collection` target mapping"* — referencing a flag that doesn't exist on this command. Either wire up `--collection <key|name>` or fix the help text.

### 8. No preflight — `--dry-run` happily plans an import that cannot execute

With Zotero desktop **not running**, `zotio import pdf ... --dry-run` returned a full, clean two-operation plan. `import pdf` requires the connector; the preview should fail loudly. I only caught it because I checked `pgrep` by habit; I then had to `open -a Zotero` and poll `/connector/ping` myself.

Fix: probe `/connector/ping` during the plan phase, and fail with an actionable message — "Zotero desktop not running; start it, or use `import doi <DOI>` which needs no desktop." The `capabilities` preconditions registry appears to be the natural home for this.

### 9. `--plain` is silently ignored — returns JSON

```
$ zotio items recent --limit 2 --plain
{ "meta": { "source": "live" }, "results": [ ...
```

Same for `zotio search <q> --plain` and `zotio collections list --plain`. Either honour it for array responses or reject the flag; silently returning a different format is the worst option.

### 10. Response envelope shape varies across read commands

`items get` → `.results` is an **object**. `items list` / `search` / `collections list` → `.results` is an **array**. Both are `.results`, so jq written against one silently breaks on the other (`cannot use null as iterable`). Normalize, or document the envelope per command in `agent-context`.

### 11. `no_op` results carry no reason

`items move` on an already-filed item returns `{"status": "no_op"}` with `"changes": null` and nothing else. Indistinguishable from "item not found" or "collection not found" without a second query. `import pdf` results include a rich `reason` object — do the same here (`reason: "already_member"` etc.).

---

## P3 — Discovery

### 12. `zotio which` misses the mark on core phrasings

```
$ zotio which "add an item to a collection"
  8  attachments add
  5  schema drift
  5  items summarize

$ zotio which "file a paper into a collection"
  8  annotations export
  6  items similar
  5  collections gaps
```

The right answers — `items move` and `items add-to-collection` — never appear, for two of the most obvious phrasings of a core operation. Meanwhile `collections --help` lists no way to add an item, which sends you to `which`, which sends you to `attachments add`. I found `items move` only by reading the full `items --help` command list.

Likely cause `[INFERENCE]`: the scored registry only indexes the curated "highlights" entries, so plain CRUD verbs are unreachable. Fix: index every command's name + short description, and add "add/file/put item in collection" aliases to `items move` / `items add-to-collection`.

### 13. Two commands do the same thing with no cross-reference

`items move --to` and `items add-to-collection` overlap. Neither help text mentions the other, nor explains when to prefer which. Pick one as canonical and make the other's help point at it.

---

## What worked well (don't regress it)

- `import pdf` recognition was flawless: both files got correct journalArticle parents with DOI, journal, date and creators, and Zotero-standard renamed stored copies. Byte sizes matched the originals exactly.
- Preview-first defaults with `--dry-run` / `--yes` are exactly right for agent use.
- `collections items` + `collections list` made the "where does this paper belong" judgement genuinely easy — being able to read the actual contents of candidate collections is what made the filing decision defensible rather than a guess.
- `no_op` on a repeated `items move` (rather than a duplicate or an error) is the correct idempotent behaviour.
