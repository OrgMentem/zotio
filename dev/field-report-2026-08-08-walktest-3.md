# zotio field report #6 — 2026-08-08 — walk-test 3

Third walk-test, against deployed `0.16.1-dev+fieldreports` at `c193041`.

Library `5847066`. **All mutations reverted — 2621 records / 786 tags / empty trash /
fingerprint `b5663882922c13c7`.** Mirror and write plane agree exactly (927 top-level each).

All ten walk-test-2 findings verified. **Nine of ten fixed.** One (X-9) is not, and this pass
root-caused it to a single field. Six new findings, all from surface nobody had exercised.

---

## Verified fixed

| finding | evidence |
|---|---|
| **X-1** `--on-duplicate` | With a 30s settle, all four modes behave: default and `skip` → `skipped_duplicate`, **0 items created**; `attach` → `attached_duplicate`, creating only attachment `HIV84S33` with `parentItem: PHMIJWH3`; `create` → `recognized`, 2 new records. `import scan` on the renamed file classifies `[duplicate] … -> PHMIJWH3`, so content-stream extraction works. |
| **X-2** creators plan | 304 → **73** runnable. `creator_variant_ambiguous`: **0 runnable**, hidden by default, rendered under `--include-ambiguous` as `REVIEW ONLY — UNSAFE same-surname candidate`. Runnable count stays 73 with the flag on. Direction now prefers completeness: `M. H. Teicher → Martin H. Teicher`, reversed from last pass. |
| **X-2** JSON surface | Attacked the path a script would use: `creators audit --include-ambiguous --json` → **0** unsafe findings carry a runnable rename; each unsafe alias is marked `"unsafe": true`. |
| **X-3 / X-4** mirror reap | `sync --full` reaped all four phantoms (`3UINT4UH`, `GPMXW2IS`, `USJWA7X6`, `HHP74G2C` → 0 rows). Mirror top-level **927** = web top-level **927**, zero drift in either direction. `library health` reports 927. |
| **X-5** journal + undo | `items new` journals `('item_create', '4XR56ZRJ')` — the real key. `journal undo` → `applied: 1`, and the item comes back with `deleted: 1`. Full round trip works. |
| **X-6** template | `schema new-item-template --item-type journalArticle` returns a 36-field template, no 404. |
| **X-7** envelopes | `items collections-of` and `journal list` now `.results` arrays; `items audit` documented report-shaped. |
| **X-8** `items update` | Standard envelope. |
| **X-10** `--prefer title` | Hyphen boundaries handled. |

---

## X-9 — [MED, NOT FIXED] `items new` still 500s on the default route — root cause is one field

The `uri: ""` fix is present and correct (`connector.go:87-90` substitutes `https://zotero.org/`),
but the failure is unchanged:

```
$ zotio items new --item-type journalArticle --field "title=WALKTEST3 UNDO PROBE" --yes
ok: false   status: failed   reason: "connector saveItems: HTTP 500: "
```

Reporting is still honest (0 items created — W-2 holds). I isolated the cause by replaying payloads
against `/connector/saveItems` with `X-Zotero-Connector-API-Version: 3`:

| payload | result |
|---|---|
| minimal item (`itemType` + `title`), with or without `id` | **201** |
| full schema template + `id` | **500** |
| full schema template, no `id` | **500** |

Then bisected all 35 template fields individually — exactly one triggers it:

```
fields that alone trigger 500: ['creators']
value: [{"creatorType": "author", "firstName": "", "lastName": ""}]
```

Confirmed by varying only that field:

```
creators omitted             -> 201
creators: []                 -> 201
creators: [{"", ""}]         -> 500      # the schema template's placeholder
creators: [{"", "Tester"}]   -> 201
creators: [{"A", "Tester"}]  -> 201
```

**`schema new-item-template` emits a placeholder creator with both names empty, `items new` sends the
template verbatim, and Zotero's ItemSaver 500s on it.** Any bare create with no `--field creators=…`
hits this; `--via web` is unaffected because the Web API tolerates it.

Fix: drop creator entries whose name fields are all empty before `SaveItems` (or omit `creators`
entirely when every entry is blank). One guard in `connector.SaveItems` covers every caller —
`items_create.go:121`, `create_route.go:199`, and the import paths.

Note `X-6` and `X-9` are the same template: fixing the template's placeholder would close both the
500 and the odd `creators: [{"",""}]` a user sees in the template output.

---

## New findings

### N-1 — [MED] The envelope contract stops at the items/collections/tags family

Six list-shaped commands still return **bare arrays** with no `{meta, results}` wrapper:

```
annotations search     bare array
annotations timeline   bare array
annotations export     bare array
capabilities           bare array
groups list            bare array
profile list           bare array
tags audit             bare array
```

`tags audit` is the notable one — it is the command this entire report series revolves around, and
`jq .results[]` fails on it while succeeding on `tags list`, `tags get`, and every items/collections
command. `groups list` and `profile list` are plainly the same shape as `collections list`, which is
wrapped.

These are distinct from the documented report-shaped exemptions (`items audit`, `journal show`,
`doctor`, `which`, `analytics`, `schema drift`, `capabilities drift`, `workflow status`,
`reading-list`, `items duplicates`, `items citekey-conflicts`, `creators audit`), all of which carry
purpose-built top-level keys and are reasonable exemptions. The seven above are lists.

### N-2 — [LOW] `creators audit --json` omits the 59 safe `initials` commands the text plan emits

```
text plan runnable: 73   (14 exact + 59 initials)
JSON findings with a runnable rename: 14   (exact only)
```

Per-class in JSON: `creator_variant_exact` 14/14 carry a command, `creator_variant_initials` **0/52**,
`creator_variant_ambiguous` 0/94. Gating `ambiguous` out of JSON is correct and deliberate; dropping
`initials` looks unintended, since those are exactly the safe expansions
(`E. Hollnagel → Erik Hollnagel`) the text plan happily offers. An agent consuming JSON sees 14 of
the 73 available remediations.

### N-3 — [LOW] `import scan` rejects a file path with a misleading error

```
$ zotio import scan /tmp/wt3/EBSCO-FullText-07_14_2026.pdf
Error: reading /tmp/wt3/…pdf: open /tmp/wt3/…pdf: not a directory
```

It takes a directory, which is reasonable, but "not a directory" reads as a filesystem fault on a
path that exists and is readable. `import pdf` accepts file paths, so passing one here is the natural
mistake. Suggest: "expects a directory; for a single file use `zotio import pdf <file>`".

### N-4 — [LOW] `library health` prints "preset quick" but the flag is `--for`

```
Scope: library · 927 top-level items (4306 mirrored rows) · source local · synced 20m ago · preset quick
$ zotio library health --preset full
Error: unknown flag: --preset
```

The output names the concept "preset", the flag is `--for`. (The `--for` error message itself is
excellent — it lists all four valid values.) Either rename the display to `--for quick` or accept
`--preset` as an alias.

### N-5 — [INFO] `library health --for all` is sound

Exercised the deepest untested path. Returns a well-formed report — `schema_version`, `scope`,
`preset`, `checks`, `findings`, `skipped`, `summary`, `remediation_plan` — with
`duplicate_candidates` 19, `missing_citation` 117, `missing_doi` 56, `missing_pdf` 112,
`missing_abstract` 393, `missing_tags` 638, and honestly declares `broken_attachment_file` and
`retracted_item` as skipped. No defect found.

### N-6 — [INFO] Read-only sweep of untouched commands: no crashes

`analytics`, `annotations search|timeline|export`, `capabilities`, `capabilities drift`,
`groups list`, `profile list`, `workflow status`, `schema drift`, `reading-list` all execute cleanly
and return well-formed JSON. `profile show` correctly requires an argument; `vault audit` correctly
refuses without a configured vault root; `export snapshot` correctly requires `--output`. Only the
envelope shapes in N-1 are wrong.

---

## Suggested order

1. **X-9** — one guard in `connector.SaveItems`; also fix the template placeholder for X-6.
2. **N-1** — wrap the seven list commands, `tags audit` first.
3. **N-2**, then N-3/N-4.

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

Mutations made and reverted: 46 scratch items created and permanently deleted (40 of them
connector-bisection probes), 1 `items new` → `journal undo` round trip, 1 duplicate-import matrix
across four modes, 1 attachment attached and removed.
