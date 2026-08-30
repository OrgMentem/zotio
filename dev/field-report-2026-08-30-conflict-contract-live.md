# zotio field report — 2026-08-30 — the mutation conflict contract, live

Companion to `dev/field-report-2026-08-30-release-gate-real-library.md`, which listed this as the
first item under `## Not closed by this run`:

> **`mutate` conflict versus exit 13.** Not verified live. Forcing a genuine version conflict needs
> a stale-version flag, and `items tags add` exposes none. Defended by unit tests only.

**It is now closed.** This report records the measurement.

Source: this repo at `7df62d9` (`feat(client): make a real version conflict reachable for release
rehearsal`). Binary: `go build ./cmd/zotio`. Target: the operator's real personal library
(`users/5847066`), WebDAV-backed, Zotero 7 desktop running, reads resolving against the desktop and
writes routing to `api.zotero.org`.

Every claim below is labelled **OBSERVED** — measured against the live library, with an independent
paged oracle (`api.zotero.org` by `urllib`, never another zotio command).

---

## Why a live conflict was previously unreachable

Not an oversight. The write engine resolves each object's version at apply time, immediately before
the write, which is the 0.16.1 fix that made `tags rename` able to write at all. A conflict
therefore requires either a concurrent writer inside a sub-second window, or a deliberately stale
precondition — and no command exposes a stale-version flag.

So the contract shipped in 0.22.0's `### Changed — breaking` section was defended by unit tests
alone, in a repo whose release gate 3 exists precisely because a green suite hid every P0 of the
0.17.0 cycle by mocking the read/write plane split.

## The affordance

`ZOTIO_TEST_STALE_VERSION` forces the outgoing `If-Unmodified-Since-Version` to a fixed value. It is
applied in `doRequestOnBase` — the single point every write's precondition passes through — and
**only when the request already carries one**, so it can never add a precondition to a write that
deliberately omits one. An env var rather than a flag, matching `ZOTIO_VERIFY`: it belongs to the
rehearsal harness, not the command surface.

---

## Finding 1 — a forced-stale write produces Zotero's own 412 — OBSERVED

Baseline, oracle-paged: **3224 items, library version 15119**,
`key_tags_sha256 96ea73e8…eebae484` — byte-identical to this morning's release-gate snapshot, so
nothing had drifted between the two runs.

Control first, without the override, to prove the write path itself works on this item:

```
$ zotio items tags add --tag zotio-stale-conflict-probe UU98D9TU --yes --json
→ writing via Zotero Web API: https://api.zotero.org/users/5847066
  "expected_version": 15121   … applied
```

Reverted, and the oracle confirms `tags: ['/unread'] version: 15123`.

Then the same write with the precondition forced stale:

```
$ ZOTIO_TEST_STALE_VERSION=1 zotio items tags add --tag zotio-stale-conflict-probe UU98D9TU --yes --json
"summary": { "attempted": 1, "applied": 0, "conflicts": 1, "failed": 0, "not_attempted": 0 }
"items": [ { "key": "UU98D9TU", "status": "conflict",
             "reason": "Item has been modified since specified version (expected 1, found 15123)" } ]
"journal": { "run_id": null }
Error: mutation incomplete
exit=1
```

OBSERVED: the reason string is **Zotero's own**, not zotio's — the server rejected the write. This
is a genuine 412 round trip, not a simulated one.

## Finding 2 — the 0.22.0 breaking claim holds — OBSERVED

The changelog claim under `### Changed — breaking` is:

> **A mutation that hits a conflict now returns the conflict, not exit 13.** A failure to record the
> mutation journal used to overwrite the engine's own error, so a real conflict without
> `--continue-on-error` surfaced as the generic "results incomplete" exit code.

OBSERVED, and worth stating precisely, because the claim is narrower than it first reads. The
"specific conflict error" is the **engine's own** error — `mutation incomplete` from
`internal/mutation/mutation.go:304`, exit **1**. The thing it must not be is `degradedErr`, which is
exit **13** (`internal/cli/helpers.go:241`).

Measured: exit **1**, `Error: mutation incomplete`, and `journal.run_id: null`. A CI gate or MCP
caller branching on 13 to mean "partial run" correctly does not see 13 here.

NOT ESTABLISHED, and deliberately out of scope: the reverse half of that fix — a journal-recording
failure degrading a run the engine already considered *successful* — needs an unwritable journal
path, which this run did not construct.

## Finding 3 — `items tags add` returns the engine's generic error, by design — OBSERVED

The conflict's Zotero reason string reaches the operator inside the JSON envelope
(`items[].reason`), while the process error is the generic `mutation incomplete`. Two sibling
commands deliberately do more: `collections_move.go:53` and `collections_update.go:94` both carry a
comment about surfacing "the `classifyAPIError` result, not the engine's generic 'mutation
incomplete'".

So the specificity is per-command, not global. For a scripted consumer this is adequate — the
envelope carries the status and the server's reason, and the exit code is not 13 — but a caller
reading only stderr on `items tags add` learns less than one calling `collections move`. Recorded as
an observation, not a defect; nothing in the shipped contract promises the richer error here.

---

## Revert detector — PASSED

Four writes landed over the run (two control add/remove pairs); the forced-stale attempt landed
none, which is the whole point.

| field | before | after |
| --- | --- | --- |
| items | 3224 | 3224 |
| `key_tags_sha256` | `96ea73e8…eebae484` | `96ea73e8…eebae484` |
| library version | 15119 | 15123 |

The probe tag is present on **nothing**. The item→tags map is byte-identical. Only
`library_version` advanced, exactly as the four reverted writes require.

---

## What this closes

* `zotio-75579ea6d3e3afed` — the conflict contract is no longer unit-test-only.
* The first item under `## Not closed by this run` in
  `dev/field-report-2026-08-30-release-gate-real-library.md`.

Still open, unchanged by this run:

* The connector re-parent route. A completed re-parent has never been measured on a current tree.
* `vault push`. No vault configured on this machine.
* `journal undo` cannot reverse an `import apply` create (`zotio-e62e952b429c45ca`).
