# zotio field report — 2026-08-23 — papio — the capability registry has two routes and one `requires`

Source: papio, the downstream consumer that drives `zotio` as a subprocess.
Raised after consuming v0.20.0 and v0.20.1. Recorded rather than fixed, so the
argument survives with the finding.

Status: **OPEN, not blocking.** papio gates on nothing that this breaks today.

---

## The defect

`attachments add` gained a second write route in v0.20.0 (`--via connector`,
which reaches the desktop instead of the Web API uploader). The capability
registry still describes one:

```json
{"path":"attachments add","operation":"write","data_sources":["web"],
 "write_target":"web_api","requires":["web_api_key","zotero_file_storage"]}
```

`zotero_file_storage` asserts that the command needs Zotero's own cloud storage
to be the desktop's file store. For the connector route that is **false** —
avoiding that storage is the whole reason the route exists, and
`checkZoteroFileStoragePrecondition` already waives the precondition for it
(`internal/cli/preflight.go`).

So the registry, which exists to be the machine-readable source of truth agents
read *instead of* parsing `--help`, states a precondition the command does not
have. Nothing gates on it today. It is still a false fact in a contract.

## What was considered and rejected

Making `write_target` conditional. Two independent reviewers and papio all
landed on the same answer: **do not.**

papio's reasoning, as the consumer the field exists for, is the clearest
statement of it:

> papio does not use that field to pick a route. Papio picks the route from facts
> it owns — whether the Zotero item already exists, and what attachment_mode the
> operator configured. A conditional value would give papio nothing it needs and
> would cost every existing reader, because a scalar that sometimes means
> "default" and sometimes means "one of several" cannot be read safely without
> knowing which release wrote it.

`write_target` therefore stays a scalar naming the DEFAULT route. That decision
is recorded beside the entry in `internal/cli/capability.go`.

## The design papio asked for, and committed to consuming

Additive, needing no conditional vocabulary:

- keep `write_target` as the default route, unchanged for every existing reader;
- add a list of the routes a capability offers, each carrying **its own**
  `requires`.

Two things that buys, in papio's words:

1. It can gate on **capability instead of version**. Today it cannot detect the
   route at all: an older zotio ignores `--via` and answers normally, so there is
   no probe. Its doctor consequently tells an operator to "upgrade zotio" when
   they may already be current — "papio printing a maybe".
2. The `requires` for each route becomes true, rather than one union that is
   wrong for one of them.

papio: *"If you build that, I will consume it and drop the version guess."*

## Why it was not built with the finding

It is a schema addition to the contract agents consume: it touches the registry,
the MCP surface, and the generated reference docs. It arrived immediately after
two releases were cut in one hour. Landing an unreviewed registry schema at that
moment is the risk `.omp/rules/release-tag-requires-green-ci.md` and
`dev/releasing.md` exist to prevent, so it is queued deliberately rather than
rushed. papio confirmed it is not blocking and will say so if that changes.

## Adjacent, resolved elsewhere

**Attachment titles from a content-addressed path.** `attachments add` titles the
attachment from the filename it is given, so a caller passing a content-hash path
gets a 64-character SHA-256 as the title Zotero shows. Visible in one library:

| route | attachment title |
|---|---|
| `import apply` (five papers) | `Full Text PDF` |
| `attachments add` (`6XSHTW5M`) | `3a78243d047f72794bf5e8e31e481ad2…` |

The on-disk filename was already correct — Zotero's save-time rename from the
parent title produced a readable name. Only the title field carried the hash.

papio owned and fixed this (papio `3fe86c9`): the attach route now stages a copy
named from the work title, falling back to the identifier basename, sanitized to
one path component. Content-addressed names are papio's artifact-store invariant,
not zotio's, so that is the right place for it.

**Optional, for zotio:** title from the parent item or the resolved metadata
rather than the filename, so any caller passing a hash still lands a readable
title. Belt and braces — `--title` already exists for a caller that knows
better, and papio no longer depends on it. Not queued as a defect.

`6XSHTW5M` under `Y2X7SALM` still shows the old hash title, because papio's fix
only affects new plans and a cached plan deliberately keeps the argv it recorded
(re-deriving a mutation zotio may already have applied is the duplicate-paper
risk). Retitling it is a write to the operator's real library for a cosmetic
result, so neither side did it unattended. It is the operator's call.
