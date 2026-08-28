# zotio field report — 2026-08-28 — live connector verification

Source: this repo at `d8b3adc` ("fix(import): bind connector results to written PDF"), built to
`/tmp/papio-launch-demo/bin/zotio-current` and run through the isolated wrapper
`/tmp/papio-launch-demo/bin/zotio-demo-profile`. macOS 25.6.0, Zotero 7 desktop launched with
`-profile /tmp/papio-launch-demo/zotero-profile -no-remote`, data directory
`/tmp/papio-launch-demo/zotero-data`, `sync.autoSync` false, zero sync credentials in `prefs.js`,
sole owner of `127.0.0.1:23119` (`lsof` confirmed pid 97304).

This report is the opposite of `dev/field-report-2026-08-22-papio.md`, which states plainly that
it never contacts a live Zotero. **Every claim below labelled OBSERVED was measured against a
running Zotero desktop.** The connector import contract had until now been defended only by tests
against a fake server on port 23119, which skip themselves whenever real Zotero holds that port.

Claim labels: **OBSERVED** — measured live in this run, with the command or query given.
**VERIFIED** — a file and line range read in this repo. **NOT ESTABLISHED** — searched for and
not found, stated so the next investigator does not repeat the search.

---

## Why this run was needed

`internal/cli/import_apply.go` binds a connector write to its result by adding a random fragment
to the attachment's provenance URL, then requiring that exact fragment plus the manifest PDF's
MD5 on a recent parent/child pair. Four questions could not be answered by the test suite,
because the fake connector returns whatever the test tells it to return:

1. Does real Zotero persist the URL fragment?
2. Does real Zotero publish the stored file's MD5 on the child, and how soon?
3. How long does the created item take to become visible to a read?
4. Does a pre-existing item with the same title stay excluded?

---

## Finding 1 — The URL fragment survives a real Zotero write — OBSERVED, CONTRACT HOLDS

The library already held `IX8F23N5` "Demo connector key proof" from a pre-fix run on 2026-08-28
at 03:05Z, whose child `9V6S3F2A` carries **the same MD5 as the manifest PDF**
(`fccee1df227874118556ea945d5d6748`, confirmed with `md5sum` against
`papio/internal/pdf/testdata/candidatecorpus/sentinels/title_wrap.pdf`). So the live library
presented the hardest possible case: same title, same PDF bytes, differing only by the marker.

Three consecutive applies of the same manifest each returned a distinct permanent pair, and each
child carried its own fragment:

| run | parent | attachment | attachment `url` |
| --- | --- | --- | --- |
| pre-fix | `IX8F23N5` | `9V6S3F2A` | `https://doi.org/10.0000/papio-demo-key-proof` |
| 1 | `5Q5CXKAS` | `9UMW7JIU` | `…/papio-demo-key-proof#zotio-write-0d33056c125e03fbcce509665bb454ad` |
| 2 | `NXPYCJVW` | `4S8I5SJJ` | `…/papio-demo-key-proof#zotio-write-5c41042988a6b191ea0d1725757ac3d1` |
| 3 | `8WVYLY4R` | `4B6HF7FN` | `…/papio-demo-key-proof#zotio-write-75e21af58ff374771cba409c50bab3d3` |

Answers, in order:

1. **OBSERVED: yes.** Zotero stored each fragment verbatim in the child's `url` field, read back
   through `GET /api/users/0/items/<parent>/children`. This confirms live what the reviewer had
   only established from `zotero/zotero` source, where `createURLAttachment` calls
   `setField('url', options.url)`.
2. **OBSERVED: yes, immediately.** Every child reported `md5` equal to the manifest PDF on the
   first read-back.
3. **OBSERVED: sub-second.** Run 1 completed in 0.27 s wall time end to end; runs 2 and 3
   together took 0.43 s. The recovery wait bound — `storedConnectorRecoveryTimeout`
   (`internal/cli/import_apply.go:227`), aliased from the 90 s
   `connectorReparentVisibilityTimeout` (`internal/cli/attachments_reparent.go:79`) — was never
   approached, so that bound is generous rather than tight against Zotero 7 on this hardware.
4. **OBSERVED: yes, under growing ambiguity.** Run 1 faced one same-title same-MD5 decoy, run 2
   faced two, run 3 faced three. No run returned another run's keys, and the pre-fix child
   `9V6S3F2A` was never selected. The fragment was the sole discriminator, and it was sufficient.

The mutation journal recorded all three runs with their permanent keys and markers
(`/tmp/papio-launch-demo/zotio/data/journal/journal.jsonl`, run ids
`20260828T062309Z-679c00bb`, `20260828T062341Z-c7e03a73`, `20260828T062341Z-e774932b`).

**The design holds against real Zotero.** Under the pre-fix title-and-MD5 rule this library state
was unresolvable: four candidates, identical titles, identical bytes.

---

## Finding 2 — `journal undo` cannot reverse an import-created item — PRE-EXISTING GAP

**Status: real gap, product decision required. Not a regression.**

`zotio journal undo 20260828T062309Z-679c00bb` refused, exited non-zero, and reported:

```
skip import_create 5Q5CXKAS (op import.apply:001:create): change on field "item" is not reversible
```

VERIFIED cause: `internal/mutation/reverse.go:70` trashes an op only when
`op.Kind == "item_create"`, while `internal/cli/import_apply.go:382` records
`Kind: "import_create"`. The two strings never meet, so an item created by `import apply` has no
inverse and falls through to the `reversibleFields` check, which covers tags and collections only.

VERIFIED pre-existing: `git show 85e82f5^:internal/cli/import_apply.go` already records
`Kind: "import_create"` at line 216, and `git diff 85e82f5^ HEAD -- internal/mutation/reverse.go`
shows the `item_create` arm untouched by this session's commits.

The test gap that hid it: `internal/mutation/reverse_test.go`'s `TestInverseOpsTrashesCreatedItem`
builds an op with `Kind: "item_create"` — a kind `import apply` never emits. The test defends a
contract no shipped command exercises. This is why a live run found it and a green suite did not.

The refusal itself is fail-closed and honest: nothing is silently skipped, and the command exits
non-zero. So this is a capability gap, not a data-safety defect.

**Undecided, and deliberately not implemented here:** whether `journal undo` should trash items
created by `import apply`. Extending the `item_create` arm to `import_create` is a two-line change
and makes undo destructive for a command that is currently irreversible. That is a product call,
not a cleanup.

---

## Finding 3 — The rehearsal reset cannot use `zotio items delete` — OBSERVED, PLAN CORRECTION

`zotio items delete 5Q5CXKAS --yes` failed:

```
Error: PATCH /items/5Q5CXKAS returned HTTP 428: Zotero-Server-ID not provided
```

OBSERVED cause: the isolated demo profile deliberately holds no Zotero API key, and Zotero's local
API refuses the write. The connector route can create but cannot delete, so in a
credential-free isolated profile **creates are possible and deletions are not**.

Consequence for the deferred demo work: the "reset the cart after each run" step cannot be a zotio
command in this profile. The reset must restore a pristine copy of
`/tmp/papio-launch-demo/zotero-data`, taken while Zotero is stopped, or be performed by hand in
the Zotero user interface.

This run therefore leaves the demo library holding four "Demo connector key proof" parents. Any
recording must start from a restored pristine data directory, not from the current state.

---

## What remains open

* The failure paths — an unresolved `saveItems`, a failed attachment, an ambiguous read-back — were
  not reproduced live. They stay covered by the fake-server tests only. Forcing them against real
  Zotero needs a fault-injection proxy in front of port 23119. NOT ESTABLISHED: no such harness
  exists in this repo.
* Finding 2 needs a decision before `journal undo` can be described as covering imports.
* Finding 3 needs the pristine-snapshot step written into the papio demo storyboard.
