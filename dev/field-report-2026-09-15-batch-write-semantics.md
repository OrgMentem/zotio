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
