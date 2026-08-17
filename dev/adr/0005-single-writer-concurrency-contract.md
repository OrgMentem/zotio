# ADR 0005 — Single-writer concurrency contract

- **Status:** Accepted (2026-08-01; revised 2026-08-17).
- **Scope:** Every zotio process that publishes installation state or an output artifact, including CLI and in-process MCP command execution.
- **Deciders:** enieuwy.

## Context

zotio has several durable state and publication paths: configuration and credentials,
profiles, the local-store sync/demo/tail cursor, workflow checkpoints and journals,
export snapshot sidecars, vault compare-and-replace output, and bundle publication.
Many of those paths follow a load → mutate → publish sequence. Atomic temporary-file
rename preserves crash consistency for one publication, but it does not prevent two
processes from loading the same stale state and each publishing a valid-looking,
conflicting result.

MCP compounds the risk because its commands execute in-process alongside framework
reads, while normal CLI invocations can run at the same time. An in-process
mirrored-command mutex already limits one MCP execution path, but it cannot govern
separate CLI processes and therefore is not the concurrency contract.

The local SQLite mirror is intentionally a concurrent-read subsystem: WAL and
read-only handles allow local and MCP queries to proceed without making ordinary
reads contend with mutations. Zotero's remote API has its own optimistic concurrency
precondition (`If-Unmodified-Since-Version`), but remote mutations can also resolve
and persist `UserID` and append zotio's local mutation journal.

## Decision

**zotio is multi-reader/single-writer per installation or independent output scope.**

Every writer is classified on four independent axes. Collapsing them is what produced
the incoherence this revision removes: `export snapshot --output F` coordinated on
`F`, a plain `export --output F` took no lock at all, and `--deliver=file:F` was
exempt from both — three writers to one named file, one namespace, three policies.

### 1. Derivation dependency

Does the correctness of the next publication depend on **coordinated mutable state
this installation owns**, which another writer can invalidate before commit? This is
deliberately not "does it read state": every export reads the library.

- **Yes** — configuration and credentials, profiles, the local-store sync/demo/tail
  cursor, workflow checkpoint plus journal, export snapshot checkpoint sidecars
  (`--resume` continues from them), vault compare-and-replace, bundle publication.
  These writers acquire **before their first state read** and hold through
  publication or checkpoint cleanup. A lock taken only around `save()` is invalid:
  the load it publishes is already stale.
- **No** — a replacement projection. A plain export, or a delivery rename, derives
  its bytes from the source library alone; nothing it published earlier constrains
  what it publishes now. It still locks, but for collision detection on the output,
  never for staleness protection.

Remote library mutations belong to the first class: besides the remote write they can
resolve and persist `UserID` and append the local mutation journal. Zotero-side
conflicts remain protected by `If-Unmodified-Since-Version`.

### 2. Coordination scope and key

- **Installation scope** — the host user's `~/.zotio/.writer.lock`, because profiles
  are persisted in `~/.zotio/profiles.json` even when configuration or data paths are
  overridden. `--config` and `ZOTERO_CONFIG` select only configuration; they never
  create an independent writer scope. `ZOTIO_DATA_DIR` moves credentials and local
  state but does not isolate the shared profile file, so it shares this lock too. A
  distinct user home is an independent installation scope.
- **Output scope** — the transient sibling `<canonical target>.lock`. Every writer to
  a user-named path derives this key through one helper (`outputWriterLockPath`), so
  the same named data target always yields the same lock identity, including through
  symlinked ancestors. Case-insensitive filesystems remain best-effort.

A command computes its whole lock **set**, not a single scope: `sync --deliver=file`
writes installation state and a named artifact. Sets are deduplicated, acquired
installation-scope-first, released LIFO. A transaction reuses any scope already held
anywhere in its command context — nested in-process invocations inherit that context
— and **never releases a lock it did not acquire**.

### 3. Collision policy belongs to the scope, not the writer

A policy is a property of the coordination scope, so every writer targeting a scope
participates in it. What may differ is the response to a busy scope:

- **Primary writers** (installation writers, `export snapshot`, `collections bundle`,
  `vault` writes, and every export publishing to `--output`) fail fast: precondition
  exit 9 with retry guidance. They never wait.
- **The delivery sink is secondary.** `--deliver=file` is a routing side effect of a
  command whose real work already succeeded, and it runs after that command's
  transaction has ended. A busy target makes it skip with a stderr warning; the
  command's exit code is unchanged. It holds the output lock only across its rename,
  because it has no load phase on the target.
- Stdout and webhook sinks name no path and are outside the namespace.

Two deliveries to one target therefore serialize their renames, and the last complete
artifact wins. That is acceptable: each is complete, and neither can be observed torn.

### 4. Publication mechanism

In-place append plus checkpoint (`export snapshot --resume`), atomic single-file
replacement (plain exports, delivery), or multi-file transaction (bundle, vault).
Atomic temp-plus-rename remains the crash-consistency mechanism, **not** concurrency
control: it prevents torn and destroyed artifacts, never lost updates.

### Mechanism and hygiene

- The lock is an OS advisory lock implemented cross-platform with
  `github.com/gofrs/flock` v0.13.0. Process death releases it, so there is no
  stale-lock recovery protocol.
- **zotio never removes a lock file.** Unlinking one can drop an inode a concurrent
  acquirer has already locked, letting a third writer lock a fresh inode under the
  same name and split the namespace. Acquisition creates the file with mode `0600`
  and chmods an existing one, but never truncates, writes, or deletes it — the
  sibling lives in the user's output directory, so its bytes are not zotio's to
  destroy. `export snapshot` removed its lock file before this revision; it no
  longer does.
- Readers remain concurrent. Local and MCP queries use SQLite WAL and read-only
  handles and take no writer lock. The MCP in-process mirrored-command mutex is
  defense in depth only.
- Lock eligibility follows a command's actual mutation condition, not capability
  metadata alone: confirmation-gated mutations lock only in apply mode, commands that
  write unless `--dry-run` lock for their live mode, and flag-sensitive commands lock
  only when the write-enabling flag is present.

This contract serializes cooperating zotio processes on one host. It does **not**
claim distributed locking across machines, NFS, or iCloud-backed paths.

## Consequences

- A concurrent writer fails predictably before it can observe stale state or overwrite
  a live artifact, and is safe to retry after the active writer exits.
- Every command that names an output path now contends on it, so two exports to one
  file no longer race: the second exits 9 instead of publishing over the first.
- Read-only CLI and MCP commands remain available while a writer is active; MCP query
  throughput is not serialized behind local publication.
- A `<target>.lock` sibling is left behind next to every artifact zotio publishes. It
  is not evidence of an active writer — only acquisition determines availability —
  and pre-existing content at that path is preserved.
- `--deliver=file` can be skipped by a busy target while the command itself succeeds.
  The warning is the only signal, matching delivery's existing warn-only contract.
- Coordinating zotio instances on separate hosts remains out of scope. Remote Zotero
  write conflicts retain their API version precondition.

## Alternatives considered

| Option | Why not |
|---|---|
| Per-resource compare-and-swap everywhere | Too complex and error-prone for a personal CLI's many heterogeneous local state paths. |
| Lock only in `save()` | Does not cover stale load → mutate work, so conflicting writers can still publish. |
| Unix-only `flock` | Breaches zotio's Windows support contract. |
| Lock all reads | Unnecessary under SQLite WAL/read-only handles and harmful to MCP query availability. |
| Rely on atomic temp-plus-rename | Prevents torn files after a crash, not lost updates between concurrent writers. |
| Exempt plain exports because they are "just projections" | Confuses derivation dependency with collision policy: an unlocked replacement writer still destroys a concurrent writer's artifact. |
| Remove the lock file after a successful run | Unlinking it can drop an inode a live acquirer already holds, splitting one namespace into two "locked" writers. |
| Make `--deliver=file` fail the command on a busy target | Delivery is a routing side effect of work that already succeeded; failing it would report a false command failure. |

## Validation

- Writer-lock tests must establish non-blocking exclusive acquisition, idempotent
  release, parent-directory/private-file setup, and release after process teardown.
- Each writer in the derivation-dependent class must acquire before its first state
  read and retain the lock through publication or checkpoint cleanup; a concurrent
  invocation must return exit 9 with retry guidance.
- One named target must resolve to one lock identity across commands, proved in both
  directions: a plain export refused while `export snapshot` holds the target, and a
  snapshot refused while the plain export holds it.
- Every export that names an output path must refuse a busy target **before its first
  source request**, so a collision costs no API traffic.
- A nested transaction on a path its command already holds must reuse the lock and
  leave it held; no code path may release a lock it did not acquire, and the
  installation wrapper must not release an output-scope lock owned by the same
  command.
- Pre-existing content at `<target>.lock` must survive a successful run.
- A file delivery must skip with a warning when the target lock is busy, leave the
  target untouched, and succeed once the lock is released.
- Read-only local and MCP query paths must remain lock-free and concurrent while a
  writer is held.

## References

- ADR-0001 — MCP command surface: this adds no MCP surface; its in-process
  mirrored-command mutex is defense in depth, not this contract.
- ADR-0002 — Local read parity subsystem: its bounded per-resource local-read model
  remains unchanged; this decision introduces no planner.
- ADR-0003 — Retire typed MCP endpoint tools: no MCP surface change.
