# Field report — Zotero multi-object update semantics (2026-09-15)

**Question.** `items tags --batch` submits `{key, version, tags}` and nothing
else. Every prior batched write in this repo is a *create*, where there are no
pre-existing fields to lose. If Zotero's multi-object **update** applies the
submitted object as a full replacement — the way `PUT` does for a single
object — then `--batch` would clear every unsubmitted field on up to 50 items
per request. Nothing in the tree settled this, and the test fake models merge
semantics by construction, so `make ci` could never answer it.

**Measured against api.zotero.org, personal library, 2026-09-15.**

Created a scratch `journalArticle` carrying a title, a publication title, a
date, one creator, and one tag. Then sent exactly the payload shape the
batched path sends:

```
POST /users/<id>/items
[{"key":"<KEY>","version":<V>,"tags":[{"tag":"probe-batch-added"}]}]
```

Result, read back immediately:

| field              | before                        | after                      |
| ------------------ | ----------------------------- | -------------------------- |
| `title`            | `zotio batch semantics probe` | unchanged                  |
| `publicationTitle` | `Probe Journal`               | unchanged                  |
| `date`             | `2026`                        | unchanged                  |
| `creators`         | 1 creator                     | unchanged                  |
| `tags`             | `[probe-original]`            | `[probe-batch-added]`      |

**Verdict: merge, not replace.** Omitted fields are preserved; the submitted
field is replaced wholesale. Sending `{key, version, tags}` is safe, and the
`tags` array must therefore be the item's complete intended tag set — which is
what `nextTagsForAdd` / `nextTagsForRemove` build from the planning read.

**Second measurement, incidental.** The first attempt used the version
returned by the create, which the library had already moved past. Zotero
answered:

```
{"successful":{},"unchanged":{},"failed":{"0":{"code":412,
  "message":"Item has been modified since specified version ..."}}}
```

That is the per-object precondition working, attributed by index, with the
server's own message — the behaviour ADR-0008 claims and
`TestItemsTagsBatchAttributesRejectionToItsOwnItem` pins. It is also why the
enforcing test fake rejects a stale submitted version with 412.

The scratch item was deleted (`DELETE` under `If-Unmodified-Since-Version`,
204, then 404 on re-read). No other item was touched.

## Live sweep — `items tags --batch` against the real library

Same day, same library, using the command rather than a hand-built request.
59 `journalArticle` items, every one already carrying between 1 and 18 tags,
selected so the run crosses the 50-object ceiling and must chunk.

A 60th candidate was dropped: `2DVR9X6Y` exists in the desktop mirror but is
404 on api.zotero.org — a leftover `ZOTIO REPARENT PROBE` item from the
2026-08-31 attachment work that never reached the cloud. Unrelated to this
change, but worth knowing the mirror can hold an item the Web API does not.

| run                        | items | wall  | per item |
| -------------------------- | ----- | ----- | -------- |
| preview (reads only)       | 59    | 28.8s | 0.49s    |
| `add --batch --yes`        | 59    | 33.3s | 0.56s    |
| `add --batch --yes` again  | 59    | 26.8s | 0.45s    |
| `remove --batch --yes`     | 30    | 15.3s | 0.51s    |
| `remove --yes` (per item)  | 29    | 37.2s | 1.28s    |

**Speedup: 2.5x wall clock, 2.8x requests** (31 vs 87 for a 30-item run).
That is the honest figure, and it matches ADR-0008's "roughly 3x". The floor
is the reads: they are per item in both paths, and the whole write phase for
59 items was about 6 seconds.

The second `add` run is the no-op case — every item already had the tag — and
it cost only its planning reads, confirming the corrected claim that a no-op
item never cost three requests.

**Chunking is proven by the run succeeding at all.** Zotero rejects a request
carrying more than 50 objects; 59 items applied.

**Merge held at scale.** All 59 items re-read from the cloud after the write:
zero changes to `title`, `date`, `publicationTitle`, or creator count; every
tag set was exactly its prior set plus the new tag, including the item that
went from 18 tags to 19. After the removals, all 59 were byte-identical to
the pre-sweep snapshot.

## Live conflict inside a batch

The defect this sweep most needed to disprove: a 412 inside `--batch` used to
exit 13 where the same command without `--batch` exits 1.

Staged by racing the command — an external `PATCH` landed on the first key
two seconds in, after its planning read and before the write:

```
exit code: 1
summary: {"attempted":8,"applied":7,"conflicts":1,"failed":0,"not_attempted":0}
28399S2Q -> conflict | {"code":412,"message":"Item has been modified since
  specified version (expected 15275, found 15334)"}
```

Exit 1, matching the per-item path measured in
`dev/field-report-2026-08-30-conflict-contract-live.md`. The conflict landed
on the item that was actually edited, carrying Zotero's own message and both
version numbers. The other seven applied — no fail-fast, and nothing reported
`not_attempted`. The concurrent edit survived intact and the stale write did
not land, which is the guarantee the body-carried precondition exists for.

Every probe tag was removed afterwards and the cohort verified against the
pre-sweep snapshot: no residue.
