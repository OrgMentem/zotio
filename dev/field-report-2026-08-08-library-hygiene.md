# zotio field report #2 — 2026-08-08 — library hygiene

Companion to `dev/field-report-2026-08-08.md`. Same install: zotio 0.16.1, macOS 25.6.0,
Zotero desktop running with local API enabled, personal library `5847066` (928 top-level items,
2621 records including children, 840 distinct tags before this session).

Session goal: "improve the library with zotio — for instance, normalise tags." That is squarely the
`tags audit` → merge-plan → `tags rename` loop advertised in the CLI highlights. **It does not work.**
Zero of 54 renames applied. Everything below came out of trying to complete that one workflow.

Reproduced against the live install. `[INFERENCE]` marks anything not directly observed.

---

## Correction to field report #1

**Finding #1 of the first report — "mutating commands write a non-JSON banner to stdout" — is WRONG.
Please disregard it; I have struck it in that file.**

zotio's stream discipline is correct: `→ writing via Zotero Web API: …` goes to **stderr**, stdout is
pure JSON. My original repro used `2>&1 | jq`, which I merged myself. Verified:

```
$ zotio items move 2GFPDDBZ --to 55C8PBTZ --yes --json 2>/dev/null | head -c 40
{ "schema_version": 1, "ok": true, "oper
```

The rest of report #1 stands. Finding #4's cursor observation (`sync_state.library_version = 12689`
against a local plane whose item versions are ~24) is unaffected.

---

## P0 — `tags rename` cannot write at all

Every operation is planned with `expected_version: 0`, so no `If-Unmodified-Since-Version` header is
sent and the Zotero API refuses the write:

```
$ zotio tags rename --from depression --to Depression --yes
Error: mutation incomplete

  "result": { "summary": { "attempted": 1, "applied": 0, "conflicts": 1, "not_attempted": 4 },
    "items": [ { "key": "PMAGGXZ3", "status": "conflict",
        "reason": "Either If-Unmodified-Since-Version or object version property must be provided for key-based writes" } ] }
```

Scope of the breakage:

- **54 of 54** renames from zotio's own `tags audit` merge plan fail identically. Applied: 0.
- Not a first-item abort artifact — `--continue-on-error` attempts all of them and every single one
  conflicts: `{"attempted":7,"applied":0,"conflicts":7,"failed":0,"not_attempted":0}`.
- `--via connector` fails the same way.
- No flag supplies the version, so there is no user-side workaround. The feature is unreachable.

**The fix is likely small, and the reference implementation is already in-tree.** `items move` writes
to the same plane successfully because it resolves the item's live version at apply time — I saw it
report `expected_version: 12704` on a real apply, versus `0` at plan time. The `tags.rename` path
never does that resolution step. Port whatever `items move` does into the tag-rename writer, and add
a regression test asserting a rename applies against a web-write target.

This is the headline feature in `--help` ("**tags audit** — Find and fix tag drift with ready-to-run
merge commands"). The commands are emitted correctly and then cannot run. It should be treated as
release-blocking; anyone who tries the advertised workflow hits it on the first command.

---

## P1 — Workflow gaps around the same loop

### 2. There is no batch apply — the "plan" is 54 shell lines to paste

`tags audit` prints one `zotio tags rename …` per group and stops. No `--apply`, no `--fix`, no plan
file. That means 54 separate process launches, each re-fetching library state, to complete one
conceptual operation. Compare `import scan` → `import resolve` → `import apply`, which has exactly the
reviewable-manifest shape this needs.

Suggest `tags audit --apply` (preview-first, `--yes` to commit) or an `import`-style manifest so the
whole merge is one journaled run — which also makes it one `journal undo`.

### 3. Merge targets are internally inconsistent, so "normalising" leaves the library un-normalised

The target is chosen by usage frequency, per group, in isolation. Actual output from one plan:

```
'Children'             → 'children'                 # lowercase wins
'Placebo'              → 'placebo'
'Cognitive Psychology' → 'Cognitive psychology'      # sentence case wins
'Developmental psychology' → 'Developmental Psychology'   # …title case wins two lines later
'Anxiety disorders'    → 'Anxiety Disorders'
```

Every individual choice is defensible; collectively they cement three conventions. A user asking to
"normalise tags" wants one convention, not 53 locally-optimal winners.

Suggest `--prefer frequency|sentence|title|lower` (default `frequency`, preserving today's behaviour).
Worth noting for whoever implements it: a blanket sentence-case policy would mangle the MeSH-derived
tags in this library (`Diagnostic and Statistical Manual of Mental Disorders`, `Magnetic Resonance
Imaging`, `Lysergic Acid Diethylamide`), where Title Case is correct. A `--prefer` policy should
probably skip tags carrying `type: 1` (automatic/imported), or at least flag them.

### 4. `items audit` denominators count children, making every number meaningless

```
$ zotio items audit
missing-abstract  3773
missing-tags      4018
missing-doi         56
```

Against **928 top-level items**. The audit is scoring attachments and notes for missing abstracts,
tags and DOIs — fields those types cannot have. `missing-abstract 3773` is mostly PDFs.

Fix: restrict to top-level items, or report `n / m top-level` so the denominator is visible. Right
now the only usable row is `missing-doi`, and only by luck.

### 5. `creators audit` finds problems it gives you no way to fix

It works well — 14 exact-variant groups, correctly grouped:

```
- Adam J. Rock (3 item(s); 5 total with aliases)
  - Adam J Rock (2 item(s): ALE7Y59C, MHX4YVEE)
- Robin L Carhart-Harris (3 item(s); 5 total with aliases)
  - Robin L. Carhart-Harris (2 item(s): CN6F79D8, YSACQE7S)
```

But `creators` has exactly one subcommand, `audit`. No `creators rename`, no merge plan, not even
copy-pasteable commands the way `tags audit` emits them. The audit is a dead end.

Suggest at minimum emitting a merge plan in the `tags audit` style; ideally `creators rename
--from "Adam J Rock" --to "Adam J. Rock"` sharing the tag-rename writer (which needs the P0 fix first).

---

## P2 — You cannot verify a zotio write with zotio

After a successful, server-confirmed tag merge, zotio still reported the pre-merge state:

```
$ zotio sync && zotio tags audit
total tags:  840
duplicate groups:  53          # server-side truth: 786 tags, 0 duplicate groups
```

Cause: writes land on `api.zotero.org`; reads resolve against the local desktop API and the SQLite
store; the desktop had not yet pulled from zotero.org. `zotio sync` **cannot** close this gap — it
syncs *from the local desktop API into the store*, not from zotero.org — so it happily propagates
stale data and reports `success: 8, errored: 0`.

Two consequences worth separating:

- **6a. Naming.** `sync` reads as "sync with Zotero" and is really "refresh local store from the
  desktop app". Rename, or state the direction in `--help` and in the `sync_summary` event.
- **6b. No refresh path after a write.** There is no zotio command that pulls the write plane's
  current truth into the read plane. This is field report #1's finding 2, escalated: it is not merely
  a stale read, it means **no zotio-only workflow can confirm its own mutations**. Anything that
  writes should either write through to the store or mark the affected resource dirty so the next
  read falls through to live.

---

## P3 — Smaller things

### 7. `import pdf` creates duplicates it already knows how to detect

The PDF import in this session created `4LIPWP5Y` (Dirks & Ferrin 2002, DOI 10.1037/0021-9010.87.4.611)
when `PHMIJWH3` — same DOI, same 185,944-byte PDF — had been in the library since 2026-07-15.
`library health` then flagged the pair as a High-severity duplicate. zotio created the mess and
immediately reported it.

`import scan` exists to make exactly this call (`duplicate` / `attach_candidate` / `new`), but
`import pdf` never consults it. Suggest running the scan classification inside `import pdf` and
defaulting to warn-and-skip, with `--on-duplicate skip|attach|create`. `attach` is the genuinely
useful case: the existing item had no better copy, so the right outcome was attaching the PDF to
`PHMIJWH3`, not minting a second item.

Side effect worth noting: Better BibTeX silently assigned the new item the citekey
`dirksTrustLeadershipMetaanalytic2002a`, so the duplicate is now also a citekey trap in a manuscript.

### 8. A third response envelope shape

Report #1 finding 10 noted `.results` is an object for `get` and an array for `list`/`search`.
`items find --doi` adds a third: a **bare top-level array**, no `meta`/`results` wrapper at all.
`jq '.results[]'` → `cannot index`; `jq '.[]'` works.

### 9. `library health` is genuinely good — but its scope line disagrees with its inputs

`Scope: library · 928 items` while the store it queries holds 4306 rows and the web plane returns
2621 records. Three different counts for "the library" across `health`, `items audit`, and the store.
Pick one definition (top-level items, presumably), use it everywhere, and say so in the scope line.

---

## What worked well

- `library health` is the best command in the tool. Ranked severity, a `Remediation plan` with real
  commands, and an explicit `Skipped (precondition unmet)` section that names *both* fixes for the
  unmet precondition. That skipped-check design is worth copying to every other audit.
- `tags audit` **detection** is accurate: 53 real groups, no false positives I could find, correct
  frequency-based grouping. Only the apply step and the target-selection policy need work.
- `creators audit` correctly distinguished punctuation variants (`Adam J Rock` / `Adam J. Rock`)
  without collapsing genuinely different people.
- `zotio which "normalise tags"` returned `tags audit` as the top hit — discovery works well here,
  in contrast to the collection-filing phrasings in report #1 finding 12.
- Failure was *safe*: the P0 aborts cleanly with `ok: false` and applies nothing. No partial writes,
  no corruption, nothing to undo. Given the bug, that is the right failure mode.

---

## Suggested order

1. **P0 `tags rename` version resolution** — release-blocking; port the `items move` behaviour.
2. **6b write-plane → read-plane refresh** — without it, no write workflow is verifiable.
3. **4 `items audit` denominators** — one-line scope fix, removes actively misleading output.
4. **2 batch apply for `tags audit`** — makes the advertised workflow one journaled, undoable run.
5. **7 `import pdf` duplicate check** — stops the tool from creating the problems it reports.
6. **3 `--prefer` casing policy**, **5 creator remediation**, **8/9 envelope + scope consistency**.
