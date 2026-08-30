# zotio field report — 2026-08-30 — connector re-verification at release HEAD

Source: this repo at `2c1a2f7` ("docs(changelog): prepare 0.22.0"). The binary under test is the
**GoReleaser artifact**, not a `go build`: `dist/zotio_darwin_arm64_v8.0/zotio` from
`release --snapshot --clean --skip=sbom,sign`, installed into the demo environment with
`ZOTIO_BIN` so `manifest.json` records `released:` rather than `built:`. It reports
`zotio 0.21.0-SNAPSHOT-2c1a2f7` — the snapshot version is derived from the last tag because
v0.22.0 does not exist yet.

Environment: the durable root from `papio/scripts/launch-demo-env.sh`
(`~/.local/state/papio-launch-demo`), which closed Finding 3 of the 2026-08-28 report. macOS
25.6.0, Zotero 7 launched with `-profile …/zotero-profile -no-remote`, sole owner of
`127.0.0.1:23119`, zero sync credentials. Pristine baseline measured before any write: **0 items,
0 trash rows, 0 storage directories.**

Claim labels follow the 2026-08-28 report: **OBSERVED** — measured live in this run.
**NOT VERIFIABLE HERE** — blocked by a property of this environment, stated so the next
investigator does not repeat the attempt.

---

## Why this run was needed

`dev/field-report-2026-08-28-connector-live-verification.md:3` pins its provenance to `d8b3adc`.
`a24287e` landed **after** it and changed three connector write-path files:

| file | change |
| --- | --- |
| `internal/client/client.go` | base context moved to `atomic.Pointer`, new `Context()` accessor |
| `internal/cli/attachments_reparent.go` | stops cancelling the caller's write client; restores the borrowed context via that accessor |
| `internal/cli/create_route.go` | `confirmConnectorCreate`'s inter-probe sleep honours cancellation instead of `context.Background()` |

Those three interlock into one borrow-and-restore contract for the write client, rewritten after
the last live measurement, on the one path that creates items in a real library. A fake-server
suite cannot speak for it, which is the standing lesson of the 0.17.0 cycle.

---

## Finding A — `import apply --via connector` still holds at HEAD — OBSERVED, CONTRACT HOLDS

One manifest entry, the same sentinel PDF as the 2026-08-28 run
(`papio/internal/pdf/testdata/candidatecorpus/sentinels/title_wrap.pdf`, md5
`fccee1df227874118556ea945d5d6748`, re-confirmed with `md5sum`), same title and DOI.

```
zotio import apply manifest-verify-2c1a2f7.json --attach-mode stored --via connector --yes --json
```

Applied 1, conflicts 0, failed 0, in **0.254 s** wall time. The result carried a permanent pair
plus its evidence:

| field | value |
| --- | --- |
| `parent_key` | `M68XU3D2` |
| `attachment_key` | `9HJVKCIQ` |
| `attachment_marker` | `zotio-write-f4afc0df5f1cba295d3e90ebbbb5b3ff` |
| `committed` | `true` |
| journal `run_id` | `20260830T095650Z-cf7e33af` |

Read back through `GET /api/users/0/items/M68XU3D2/children`:

- **OBSERVED: the URL fragment persists verbatim.** The child's `url` is
  `https://doi.org/10.0000/papio-demo-key-proof#zotio-write-f4afc0df5f1cba295d3e90ebbbb5b3ff`.
- **OBSERVED: the MD5 is published immediately** and equals the manifest PDF exactly.
- **OBSERVED: sub-second**, consistent with the 0.27 s of the 2026-08-28 run. The 90 s
  `storedConnectorRecoveryTimeout` was nowhere near approached.

The three changes in `a24287e` did not regress this path.

---

## Finding B — the re-parent route's `PATCH` is NOT VERIFIABLE in a credential-free profile

```
zotio attachments add M68XU3D2 …/sentinels/ligature.pdf --via connector --yes --json
```

Failed, exit non-zero, `failed: 1`:

```
attachment S73XM2CR is stored under temporary parent 3D9K2V35 but could not be moved to
M68XU3D2: PATCH /items/S73XM2CR returned HTTP 428: Zotero-Server-ID not provided
```

OBSERVED cause: identical in class to Finding 3 of the 2026-08-28 report, which recorded the same
428 for `items delete`. This profile deliberately holds no Zotero API key, and the local API
refuses the write. **The connector can create; it cannot `PATCH`.** So the route's first half
(one session creating a temporary parent plus the file) runs, and its second half (re-parent, then
trash the temporary parent) cannot.

Two things the failure does establish:

1. **The failure is fail-closed and honest.** `temp_parent_trashed: false` — it did not trash the
   temporary parent while the attachment still hung beneath it, so nothing was orphaned. The
   temporary parent key, marker, and title are all reported for manual recovery.
2. **The storage-naming invariant holds live at HEAD.** The storage directory is `S73XM2CR`, the
   attachment's own key, while the attachment sat under parent `3D9K2V35` — re-confirming from a
   running Zotero what `AGENTS.md` states from source: every storage name derives from the
   attachment's key, never its parent's.

**Residual gap, stated plainly.** The only live evidence for a completed re-parent is the
2026-08-22 report, whose `PATCH … HTTP 204` was measured against a **credentialed** library at a
much older tree. Reproducing it needs an API key, which routes writes to a real Zotero cloud
account — out of scope for an isolated rehearsal. The borrowed-context fix in
`attachments_reparent.go` is therefore defended at HEAD by its unit tests (`a24287e` added 137
lines to `attachments_reparent_test.go`) and by Finding B's fail-closed behavior, **not** by a
completed live re-parent.

NOT ESTABLISHED: no harness in either repo can supply a Zotero-Server-ID to a credential-free
local API, so this gap cannot be closed without either a real account or a fault-injection proxy
in front of port 23119.

---

## Revert discipline

Per the release runbook's snapshot-revert-resnapshot detector:

| | items | trash | storage dirs |
| --- | --- | --- | --- |
| pristine baseline | 0 | 0 | 0 |
| after both runs | 4 | 0 | 2 |
| after `launch-demo-env.sh restore` | **0** | **0** | **0** |

Restored state is identical to the baseline. Zotero was stopped before the restore, as the script
requires.

---

## What remains open

* Finding B's residual gap: a completed connector re-parent has never been measured on this tree.
* Unchanged from 2026-08-28: the failure paths (unresolved `saveItems`, failed attachment,
  ambiguous read-back) are still covered only by fake-server tests.
* Unchanged from 2026-08-28: Finding 2, `journal undo` cannot reverse an `import apply` create
  (`import_create` vs `item_create`). Still a product decision, still not a data-safety defect.
