# ADR 0004 — No Zotero plugin; papio exception state as reconciled automatic tags

- **Status:** Accepted (2026-07-23).
- **Scope:** The papio↔Zotero user-facing integration surface: zotio `items tags add --automatic`, and papio's exception-tag reconciler (papio `internal/zotio/tags.go`), which drives it.
- **Deciders:** enieuwy.

## Context

Competitive positioning review for papio (2026-07) proposed a Zotero plugin as the
top distribution move: render acquisition state inside Zotero, right-click →
"fetch PDF", ship through the Zotero plugin directory. The question was whether
that plugin should exist, and if so whether it belongs to papio (acquisition) or
zotio (library management).

Decomposing what a plugin would actually do exposed three jobs:

1. **Initiation** — "get PDFs for these items" from inside Zotero.
2. **Status** — see per-item acquisition state inside Zotero.
3. **Human-action resolution** — SSO logins, terms consent, identity review.

Job 3 is disqualified on its face: those actions physically happen in the
browser, where papio's extension already prompts at the point of action. Job 1
is a computed predicate ("has identifier, no attachment") served by
`papio acquire --from-zotio` and the backfill watch — nobody hand-selects 79
items. Job 2 decomposes further: success is self-indicating (the attachment
appears on the item — the product's whole point), and transient lifecycle
states (queued/fetching) have no reader inside Zotero and would be pushed
through Zotero's versioned, rate-limited sync as churn.

What remains is one legitimate in-Zotero question: **"is this item coming, or
is it mine now?"** — asked while planning reading or deciding what to chase at
the library.

Mechanics also matter: papio's daemon endpoint is an owner-only Unix socket /
named pipe. A Zotero plugin (privileged JS) cannot dial it; a direct client
would force papio to grow a localhost HTTP surface (token auth, CSRF exposure
to any local page). zotio, a native process papio already drives as a
subprocess, has none of that surface.

## Decision

**Do not build a Zotero plugin.** Serve the one legitimate in-Zotero question
with two durable exception tags, maintained by papio through zotio's CLI:

- `papio:needs-action` — acquisition parked on a human action
  (`awaiting_human`, `needs_review`).
- `papio:unavailable` — papio exhausted OA and institutional routes, as of the
  last attempt (`unavailable`).

Contract:

1. **Tags are reconciled state, not events.** papio recomputes the desired tag
   from each linked item's newest job state, then confirms that the item still
   lacks a PDF. A crash-safe ledger records pending, papio-owned, foreign, and
   missing-target states; pending intent is durable before the remote write,
   and per-item mutation envelopes are consumed even when zotio exits nonzero.
   Lost and partial writes therefore converge without claiming a user's
   pre-existing same-name tag. No lifecycle tags, ever.
2. **`papio:unavailable` decays.** Backfill re-checks unavailable items after a
   cool-down (`zotio.unavailable_recheck_days`, default 14) — green-OA copies
   appear months after publication, holdings change, adapters gain providers.
   A saved search on this tag is the user's ILL/manual worklist, so it must not
   go stale in either direction.
3. **Personal library only.** papio strips inherited `ZOTERO_GROUP` from every
   zotio subprocess and passes explicit empty `--group=` on tag writes. Zotero
   keys are library-local, so a v14 provenance table makes only keys observed
   by a post-upgrade personal missing-PDF scan eligible; pre-existing
   scope-unknown/group links are ignored. Group libraries are shared surfaces
   and papio's verdicts ("unavailable *to my institution*", "my SSO session
   needs attention") are personal.
4. **Automatic tag type and ownership-safe removal.** `items tags add
   --automatic` writes `{tag, type: 1}`; `items tags remove --automatic-only`
   removes only a matching type-1 tag. zotio never retypes or removes an
   existing same-name manual tag. Both operations expose type in their mutation
   plans/journals. No colors are assigned; that stays the user's call.
5. **Ownership split.** papio owns the state and the reconcile loop (it has the
   job store and already shells out to zotio); zotio owns every Zotero write
   and keeps its mutation envelope, version preconditions, rate limiting, and
   write gates in the path. zotio gains no papio coupling.
6. **Two tags, not three.** A `papio:skip` opt-out was considered and deferred
   until someone asks; the reconciler makes adding it later trivial, and every
   tag in v1 is one a user provably reads.

Version floor: the reconciler requires zotio >= 0.13.0 (first release with
`--automatic` and `--automatic-only`) and fails closed with an upgrade hint;
papio's global zotio floor is unchanged because the feature is opt-in
(`zotio.exception_tags`, default off).

## Consequences

- **No fourth install component.** The install chain stays daemon → browser
  extension → zotio; the tag surface adds zero components and works over
  Zotero's own sync (tag on the laptop; states maintained by the home daemon).
- **Zotero renders the UI.** Colored tags and saved searches provide the
  worklist ("papio: needs action") natively; a saved search on
  `papio:unavailable` doubles as the ILL/manual list, serving the demand the
  ILL-handoff feature idea imagined without building it.
- **Happy path writes nothing extra.** An item that acquires cleanly sees
  exactly one Zotero write — the attachment zotio was already making.
  While exception tags exist, reconciliation performs a bounded exact-key
  missing-PDF read so a manually attached PDF clears stale state; stable
  exception state performs no tag writes.
- **Failures are isolated per item.** Tag mutations are one item per zotio
  invocation. A deleted item is tombstoned without blocking valid items;
  partial/applied outcomes are persisted before aggregate errors return.
- **Disable converges.** After the daemon reloads
  `zotio.exception_tags = false`, the maintenance runner remains active while
  ledger state exists and removes papio-owned automatic tags. Users should
  stop the daemon and force one reconcile pass before uninstalling.
- **The plugin decision is reversible cheaply.** If beta users demonstrably hit
  tag-surface limits (need per-item progress detail, right-click ergonomics, or
  never see browser prompts), a plugin is a *zotio* deliverable that renders
  papio state fetched via zotio — the socket/HTTP objection and the ownership
  split already resolve that design.

## Alternatives considered

| Option | Why not |
|---|---|
| Zotero plugin (papio-owned) | Fourth install component that shortens nothing; requires a localhost HTTP surface on the daemon (token auth, CSRF exposure); bakes today's action taxonomy into a second client while it is still being reworked; Zotero plugin-directory listing for a daemon-dependent plugin invites one-star "does nothing" reviews. |
| Zotero plugin (zotio-owned) | Right owner if ever built, but same install/maintenance cost for value the tag surface already covers; deferred pending beta demand signal. |
| Full lifecycle status tags (`papio:queued`, …) | Transient states over a versioned, rate-limited sync channel = write churn with no reader; success is already self-indicating via the attachment. |
| Initiation tags (`papio:get`) | Initiation is a computed predicate (missing attachment) served by backfill/watch; a manual selection gesture duplicates config with worse discoverability. |
| Event-driven tag writes (no ledger, no reconcile) | Lost clears (daemon restart, CLI cancel, offline zotio) leave permanently lying tags; the reconciled pure-function design self-heals. |
| Direct Zotero API writes from papio | Duplicates zotio's mutation envelope, version preconditions, and rate limiting; violates the "papio never holds Zotero credentials" boundary. |

## Validation

- papio: `go test ./internal/zotio ./internal/config ./internal/store ./internal/doctor ./internal/api ./internal/cli ./internal/bootstrap` — convergence/idempotence, ownership-safe manual tags, serialized passes, per-item 404 isolation, applied outcomes alongside errors, manual-PDF clearing, disable cleanup, personal-scope provenance, version gate, cool-down re-eligibility, config validation, and forward migration from the committed v13 ledger to schema v14.
- zotio: `go test ./internal/cli ./internal/mutation ./internal/mcp` — automatic add/type-aware plans, automatic-only removal preserving manual tags, write-through and journal undo type parity, generated MCP surface, and command docs.
