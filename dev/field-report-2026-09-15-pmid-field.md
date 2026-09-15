# Field report — Zotero has a native PMID field (2026-09-15)

Found while running the pre-tag live validation for 0.26.0, against the real
library. `make ci` was green and the unit tests passed; the binary did not
work.

## What the release believed

Commit `ce39aa2` fixed "`import pmid` did not record the PMID it imported" by
writing `PMID: <id>` into Extra, on the stated premise that *"Zotero has no
PMID field for `journalArticle`, so Extra is the only place it can live"*.
`items find --pmid` matched that token.

## What the library says

```
$ curl -s "http://localhost:23119/api/itemTypeFields?itemType=journalArticle" \
    | jq -r '.[].field' | grep -i pm
PMID
PMCID
```

`PMID` is a first-class Zotero field. The premise was wrong.

## The failure, measured

`zotio import pmid 33306943 --yes` created item `EQ4H8J8S`. The preview
showed `extra: "PMID: 33306943"`, the connector payload carried it, and the
created item came back with **`extra: ""`**. `items find --pmid 33306943`
returned nothing, through six sync attempts over two minutes.

Isolating the route, same PMID:

| route | `extra` after create | `PMID` after create |
| ----- | -------------------- | ------------------- |
| `--via connector` (default while Zotero runs) | `""` | `33306943` |
| `--via web` | `PMID: 33306943` | `""` |

Zotero's `ItemSaver` parses a recognized `PMID:` line out of Extra and
promotes it to the real field. Confirmed with two direct
`/connector/saveItems` probes: `extra: "PLAIN PROBE TEXT"` survived verbatim,
while `extra: "PMID: 99999999"` came back as `extra: null`, `PMID:
"99999999"`. So Extra is not dropped — that one token is *migrated*.

The Web API accepts the field directly (`PATCH {"PMID": "33306943"}` → 204,
value persisted), so nothing about the field is desktop-only.

## Why the lookup could never have worked

Two layers, both Extra-only. The Go matcher read `d.Extra`, and — the part a
matcher fix alone would not have reached — the SQL prefilter selected only
rows with a matching Extra substring, so a native-field row was discarded
before any comparison ran.

## Fixed

- `pubmedItemFromSummary` writes `item["PMID"]`, not an Extra token.
- `items find --pmid` matches the native field **and** the legacy Extra token,
  in both the prefilter and the matcher. Legacy items and other tools' items
  still resolve.

Verified live after the fix: the connector-created item (native field) and an
item deliberately left carrying only `extra: "PMID: 99999999"` both resolve by
`--pmid`, and a prefix matches neither.

## For the reviewer who reads this next

Four probe items were created and all four were permanently deleted. The
library was snapshotted before and after — item count, trash count, distinct
tag count, and a SHA-256 of the whole key→tags map — and the two snapshots are
identical: `0418a6bc…82e5f97`, 3228 items, 41 trashed, 705 tags.

`import arxiv` writes `arXiv: <id>` into Extra by the same reasoning. That one
is **correct**: there is no arXiv field, and `archiveID` is what the preprint
item type uses. It was checked, not assumed.
