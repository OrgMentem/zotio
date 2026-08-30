# zotio field report — 2026-08-30 — release gate 3 against a real library

Companion to `dev/field-report-2026-08-30-connector-reverify-at-head.md`, which covered the
connector path in an isolated credential-free profile. That run could not touch the write plane.
This one does.

Source: this repo at `7436618`. Binary under test is the GoReleaser artifact
`dist/zotio_darwin_arm64_v8.0/zotio`, reporting `zotio 0.21.0-SNAPSHOT-2c1a2f7`.

**Target, stated before the first write.** Real personal library, userID `5847066` (`enieuwy`),
key scope `write: true`. Reads resolve against the Zotero 7 desktop on `127.0.0.1:23119`; writes
auto-route to `api.zotero.org`. `doctor` reports file storage as **WebDAV**
(`webdav.nieuwy.com`), so stored uploads through the Web API are refused by design.

Oracles are independent of zotio and each is used for the plane it owns: `api.zotero.org` for
writes, the desktop API for reads. Never another zotio command. All collections paged at 100.

---

## Baseline and revert — DETECTOR PASSED

| | items | trash | distinct tags | key→tags sha256 | library version |
| --- | --- | --- | --- | --- | --- |
| baseline | 3224 | 29 | 705 | `96ea73e8…bae484` | 15115 |
| after revert | 3224 | 29 | 705 | `96ea73e8…bae484` | **15119** |

Every content field is identical. `library_version` advanced by exactly the four writes performed
(search create, tag add, tag remove, search delete). A version that had **not** advanced would
have been the finding.

Four mutations were made and all four reverted:

1. Created saved search `T9URDXHS` with an impossible condition — deleted, HTTP 204.
2. Added tag `zotio-relgate-probe` to `UU98D9TU` — removed.

The desktop's saved-search list is back to its single baseline entry `8ZIVNQ24 Honours`.

---

## Finding 1 — both planes, both directions — OBSERVED

The runbook's ~15–20 s propagation figure is conservative on this hardware.

| step | plane | result |
| --- | --- | --- |
| tag write | `api.zotero.org` | version 2820 → **15117**, tag present, immediately |
| read-back at T+0 | desktop | version **2820**, tag **absent** |
| read-back at T+5s | desktop | version **2823**, tag present |

**The documented write-then-read-immediately window is real and was reproduced**: the cloud had
the tag before the desktop did. Convergence took under 5 s, not 15–20 s.

Cloud→desktop for a *created* object was faster still: saved search `T9URDXHS` was readable on the
desktop (HTTP 200, local version 2822) within the same second as its `POST`.

---

## Finding 2 — `searches run` distinguishes "ran, 0 hits" from "could not run" — OBSERVED

The changelog's breaking claim for 0.22.0, verified live on a purpose-built fixture:

| input | output | exit |
| --- | --- | --- |
| `T9URDXHS` (exists, matches nothing) | `[]` | **0** |
| `8ZIVNQ24` (exists, 391 hits) | array of 391 | 0 |
| `ZZZZZZZZ` (does not exist) | `HTTP 404: Not found` + hint | **3** |

The empty case is an empty array, not an "endpoint unavailable" envelope, and a genuinely missing
search still fails loudly. Contract holds.

There is no `searches create` in the CLI, so the fixture was created through the oracle API and
deleted afterwards.

---

## Finding 3 — `items trash --data-source live` matches the live plane exactly — OBSERVED

Desktop API `Total-Results: 36`. `items trash --data-source live` returned **36**.
`--data-source auto` also returned 36.

The two agree here because the local mirror was synced from the same desktop, so this run cannot
*discriminate* the union from its absence — it can only confirm that `live` is faithful to the
plane it names, which it is.

**Incidental observation, not a defect.** The desktop holds more than the cloud: 4902 items and 36
trash rows locally against 3224 and 29 on `api.zotero.org`. That is unsynced desktop state in the
operator's library, not a zotio behavior. Recorded so a future run does not read it as drift.

---

## Finding 4 — sync counts report rows written — OBSERVED

Two consecutive syncs. Unchanged resources completed with `total: 0` (`collections`,
`items-trash`) rather than reporting the rows they offered, which is the post-fix behavior: the
version-monotonic guard retains a newer row and writes nothing.

---

## Not closed by this run

* **`mutate` conflict versus exit 13.** Not verified live. Forcing a genuine version conflict needs
  a stale-version flag, and `items tags add` exposes none. Defended by unit tests only.
* **The connector re-parent route.** Still blocked, now for a second and independent reason: this
  library's files live on **WebDAV**, so zotio refuses stored Web API uploads by design, and the
  connector route would create and trash items in the operator's real library. NOT ATTEMPTED.
* **`vault push`.** No vault is configured on this machine. NOT ATTEMPTED.
* **`--fetch-pdf` rejection with stored mode.** Flag validation only; not exercised.

---

## Environment note

The operator's Zotero was closed when this run began and was started to bring the read plane up.
It was left running, because closing it could interrupt a WebDAV sync in progress.
