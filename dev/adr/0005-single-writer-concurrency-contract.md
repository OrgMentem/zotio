# ADR 0005 — Single-writer concurrency contract

- **Status:** Accepted (2026-08-01).
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

1. A writer acquires a non-blocking, cross-platform OS advisory lock **before it
   reads state**, and holds that lock through its complete load → mutate →
   atomic-publish/checkpoint-cleanup transaction. A busy lock fails immediately
   with retry guidance and precondition-failure exit code 9; writers never wait
   indefinitely.
2. Installation-state writers share `<config-dir>/.writer.lock`. An output artifact
   independent of installation state instead uses a lock keyed by its canonical
   output path. The lock applies to configuration and credentials, profiles,
   local-store sync/demo/tail cursor changes, workflow checkpoint plus journal,
   export snapshot sidecars, vault compare plus replace, and bundle publication.
3. Remote library mutations also take the writer lock: in addition to the remote
   mutation, they can resolve and persist `UserID` and append the local mutation
   journal. Zotero-side conflicts remain protected by
   `If-Unmodified-Since-Version`.
4. Readers remain concurrent. Local and MCP queries continue to use SQLite WAL and
   read-only handles; reads do not take the writer lock. The MCP in-process
   mirrored-command mutex remains defense in depth only.
5. The lock is an OS advisory lock implemented cross-platform with
   `github.com/gofrs/flock` v0.13.0. Process death releases the OS lock, so there
   is no stale-lock recovery protocol;
   the lock file itself may remain harmlessly. Atomic temp-plus-rename remains the
   crash-consistency mechanism, not concurrency control.

This contract serializes cooperating zotio processes on one host. It does **not**
claim distributed locking across machines, NFS, or iCloud-backed paths.

## Consequences

- A concurrent writer fails predictably before it can observe stale state, and is
  safe to retry after the active writer exits.
- Read-only CLI and MCP commands remain available while a writer is active; MCP
  query throughput is not serialized behind local publication.
- Every writer must choose its correct scope and acquire the lock outside `save()`;
  a lock only around saving is invalid because it leaves the load stale.
- A remaining lock file is not evidence of an active writer. Only acquisition of the
  advisory lock determines availability.
- Coordinating independently running zotio instances on separate hosts remains out
  of scope. Remote Zotero write conflicts retain their API version precondition.

## Alternatives considered

| Option | Why not |
|---|---|
| Per-resource compare-and-swap everywhere | Too complex and error-prone for a personal CLI's many heterogeneous local state paths. |
| Lock only in `save()` | Does not cover stale load → mutate work, so conflicting writers can still publish. |
| Unix-only `flock` | Breaches zotio's Windows support contract. |
| Lock all reads | Unnecessary under SQLite WAL/read-only handles and harmful to MCP query availability. |
| Rely on atomic temp-plus-rename | Prevents torn files after a crash, not lost updates between concurrent writers. |

## Validation

- Writer-lock tests must establish non-blocking exclusive acquisition, idempotent
  release, parent-directory/private-file setup, and release after process teardown.
- Each migrated writer must acquire before its first state read and retain the lock
  through publication or checkpoint cleanup; a concurrent invocation must return
  exit 9 with retry guidance.
- Read-only local and MCP query paths must remain lock-free and concurrent while a
  writer is held.

## References

- ADR-0001 — MCP command surface: this adds no MCP surface; its in-process
  mirrored-command mutex is defense in depth, not this contract.
- ADR-0002 — Local read parity subsystem: its bounded per-resource local-read model
  remains unchanged; this decision introduces no planner.
- ADR-0003 — Retire typed MCP endpoint tools: no MCP surface change.
