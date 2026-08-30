# zotio field report — 2026-08-30 — the connector re-parent route, completed live

The third report of the day, and the one that closes the oldest standing residual. Companions:

* `dev/field-report-2026-08-30-connector-reverify-at-head.md` — Finding B: the re-parent `PATCH` is
  NOT VERIFIABLE in a credential-free profile (`428 Zotero-Server-ID not provided`).
* `dev/field-report-2026-08-30-release-gate-real-library.md` — the same route, blocked a second and
  independent way: WebDAV-backed library, so stored Web API uploads are refused by design.
* `dev/field-report-2026-08-22-papio-round2.md` — the only prior completed re-parent, on a much
  older tree.

Source: this repo at `436b2a5` plus the fix this report produced. Target: the operator's real
personal library (`users/5847066`), **WebDAV-backed** (`webdav.nieuwy.com`), Zotero 7 desktop
running, reads resolving against the desktop, writes routing to `api.zotero.org`.

Every claim is labelled **OBSERVED** — measured on that library, with an independent paged oracle
(`api.zotero.org` by `curl`/`urllib`, never another zotio command).

---

## What changed in the instrument

`dev/reparent-probe.sh` was written before the feature existed, to prove the ROUTE: it drove the
choreography by hand and issued the re-parent with raw `curl`. That answered "does Zotero permit
this". It became the wrong test once the route shipped as a command, because it exercised Zotero
rather than zotio.

The probe now drives `zotio attachments add <key> <file> --via connector` end to end and checks the
result against the oracle. `PROBE_YES=1` runs it unattended; the checkpoints that need a human
looking at Zotero or at the WebDAV server announce themselves as SKIPPED rather than being silently
assumed.

---

## Finding 1 — the route completes on a real WebDAV library — OBSERVED

Run 1, `two_column_gap.pdf` onto a freshly created receiver:

```
"summary": { "attempted": 1, "applied": 1, "conflicts": 0, "failed": 0 }
"reason": { "attachment_key": "4ZP6CCL6", "temp_parent_key": "8GKM9G44",
            "temp_parent_trashed": true, "via": "connector" }
```

Oracle, independently:

| check | observed | wanted |
| --- | --- | --- |
| attachment `parentItem` | `5MZU44CF` | the receiver |
| attachment `deleted` | 0 | 0 |
| registered `md5` | `b8bd7acc…e79b7` | the file's own md5 |
| temp parent `deleted` | 1 | 1 |
| temp parent children | 0 | 0 |

**The storage-naming invariant holds live.** `~/Zotero/storage/4ZP6CCL6/` exists and holds the PDF;
there is **no** directory for the temporary parent `8GKM9G44` or for the receiver `5MZU44CF`. That is
the property the whole route depends on, asserted from source in `AGENTS.md` and now measured on a
current tree: every storage name derives from the ATTACHMENT's key, so re-parenting relocates no
bytes.

This is the first completed connector re-parent measured on any current tree.

## Finding 2 — the route raced the desktop's own file registration — OBSERVED, NOW FIXED

Run 3 failed:

```
attachment PQPX97BG is stored under temporary parent 56UXGZFI but could not be moved to R8PWY86S:
PATCH /items/PQPX97BG returned HTTP 412: Item has been modified since specified version
(expected 15136, found 15137)
```

Sampling the attachment every 5 s showed why. Version settled at **15137** and stopped, with `md5`
and `mtime` now populated:

```
t+0s .. t+25s   version=15137  parent=56UXGZFI  md5=d43cbecc…  mtime=1788092566974
```

OBSERVED cause: the connector creates the attachment, then the **desktop** registers the stored file
by writing `md5` and `mtime`. That bump invalidates the precondition the route resolved moments
earlier. The "concurrent writer" is the desktop finishing work on the very file this run created —
benign, expected, and outside the route's control.

Proof that a retry resolves it, by hand against the oracle:

```
fresh version: 15137
PATCH with the fresh version -> HTTP 204
parent now: R8PWY86S
```

**Fix.** `reparentAttachment` retries a 412 up to `connectorReparentConflictRetries` (2) times,
re-reading the version each time. It is checked, not blind, and the decision table is the point:

| state found after the 412 | action | why |
| --- | --- | --- |
| already under the new parent | success | the PATCH landed, its response was lost |
| still under OUR temporary parent, version advanced | retry with the fresh version | the only edit absorbed is to an object this run owns |
| under any other parent | return the original 412 | someone else owns it; do not fight |
| version unchanged | return the original 412 | replaying an identical precondition would spin |

Six unit tests cover that table, each negative-controlled: setting the retry budget to 0 fails
`RetriesTheDesktopsOwnVersionBump` and `TreatsAlreadyMovedAsSuccess`.

Run 4, after the fix, `hyphen_wrap.pdf`: `applied`, `temp_parent_trashed: true`, all oracle checks
passed, storage directory `6EN2VIP4` named for the attachment.

## Finding 3 — a failed run leaves nothing orphaned — OBSERVED

Run 2 failed differently, on a transient network fault, and it failed **before** the connector
session began:

```
reading target item 7GJDJV28: GET /items/7GJDJV28: context deadline exceeded
(Client.Timeout exceeded while awaiting headers)
```

OBSERVED: the receiver had **0 children** afterwards, and a title search for
`[zotio] temporary attachment parent` returned **nothing**. No temporary parent, no attachment, no
stored bytes.

Run 3's failure was the other shape — the file existed but could not be moved — and there the route
reported `temp_parent_trashed: false`, refusing to trash a parent while the attachment still hung
beneath it. Fail-closed in both directions, and the message names the temp key, the marker, and the
title for recovery.

## Finding 4 — trashing a parent does NOT trash its attachment — OBSERVED

The revert detector caught this, and nothing else would have. After three runs whose receivers were
all trashed, the library was **+3 items** against a baseline that was otherwise byte-identical:

```
before: n_items 3224
after : n_items 3227     <- 4ZP6CCL6, PQPX97BG, 6EN2VIP4
```

OBSERVED: Zotero's server does not cascade the `deleted` flag to children. Trashing only the parent
leaves each attachment LIVE, so it keeps appearing in `/items` while its parent sits in the trash.
This is the same no-cascade behavior the 2026-08-22 report established from the server source, seen
from the other side — there it was the *reason the route is safe*, here it is a *cleanup obligation*.

The probe now trashes the attachment explicitly, and first, matching the route's own ordering rule:
never let an attachment follow a parent into the trash.

---

## Revert detector — PASSED, after the Finding 4 cleanup

| field | baseline | final |
| --- | --- | --- |
| items | 3224 | 3224 |
| `key_tags_sha256` | `96ea73e870d92c0e…` | `96ea73e870d92c0e…` |
| library version | 15123 | 15152 |

Four probe runs, all objects trashed, reversible with `zotio items restore`. Only `library_version`
advanced. The trash also holds pre-existing litter from earlier sessions (`ZOTIO REPARENT SMOKE
TARGET`, `ZOTIO 0.20.0 RELEASE SMOKE`, and similar) — not created by this run, left as found.

Storage directories grew by 3 and will clear when the trash is emptied and Zotero synced.

---

## What this closes

* `zotio-26a2e7ab6bad82ab` — a completed connector re-parent has now been measured on a current
  tree, on the WebDAV library the route exists to serve.
* Finding B of `dev/field-report-2026-08-30-connector-reverify-at-head.md`.
* The second item under `## Not closed by this run` in
  `dev/field-report-2026-08-30-release-gate-real-library.md`.

## Still open

* **`vault push`.** No vault is configured on this machine. NOT ATTEMPTED.
* **`journal undo` cannot reverse an `import apply` create** (`zotio-e62e952b429c45ca`). A product
  decision, not a defect.
* **The WebDAV byte-level claim.** Runs verified the LOCAL storage directory naming. Confirming that
  exactly one `<attachment-key>.zip`/`.prop` pair exists on the WebDAV server, with no orphan and no
  rename, needs a human looking at that server; `PROBE_YES` reports it SKIPPED rather than assuming
  it. The naming derivation is settled from source in `dev/field-report-2026-08-22-papio-round2.md`.
* **The 412 retry has not been observed firing live.** The fix is defended by six unit tests and by
  the hand-verified `204`; run 4 did not race, so the retry path itself did not execute against
  Zotero. Provoking it deterministically needs a way to delay the desktop's file registration.
