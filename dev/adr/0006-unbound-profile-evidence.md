# ADR-0006: The storage guard uses unbound, machine-wide profile evidence

- **Status:** Accepted.
- **Date:** 2026-08-18.
- **Deciders:** enieuwy.
- **Supersedes:** nothing. Refines the guard introduced alongside `internal/zoteroprefs`.

## Context

`internal/cli/file_storage_guard.go` refuses Zotero Web API "stored" uploads when
Zotero desktop is configured to keep personal-library attachment files on the
operator's own WebDAV server. The evidence is read from Zotero's `prefs.js` by
`internal/zoteroprefs`, which discovers every profile on the machine and unions
the safety-relevant facts across all of them.

An adversarial review (GPT-5.6 Pro, session `adversarial-storage-guard`,
`dev/scratch/oracle/20260818T050317Z-adversarial-review/answer.md`) raised this
as its first release blocker:

> `FileStorage` identifies a profile path and its preferences, but carries no
> Zotero user ID, account identity, API-key identity, local-API port, or target
> library ID. […] Every piece of profile evidence needs a subject.

The concrete failure is real and was introduced by this guard. Profile A belongs
to account A and uses WebDAV. zotio is configured with an API key for account B,
which legitimately uses Zotero's cloud. The union refuses B's correct upload
because of a profile that has nothing to do with it.

## Decision

**Profile evidence stays machine-wide and unbound to the target account.** The
guard does not attempt to determine which Zotero account a profile belongs to.

To keep the resulting false refusals recoverable, every refusal derived from
profile evidence names **every profile that positively evidenced that specific
reason**, and points at `ZOTERO_PROFILE_DIR` as the way to select a different
one.

Attribution is per-hazard, not per-reading. `hazardSet` therefore carries
profile paths rather than booleans. The first version of this decision named
the *representative* profile instead, which was wrong: `riskRank` orders
storage MODES only, so a hazard drawn from `Enabled`/`GroupsEnabled` is
routinely carried by a profile that lost the ranking. A refusal saying "file
syncing is off" would then name a profile with file syncing on — sending the
operator to verify the one file guaranteed to contradict the message, and
leaving the actual cause unnamed. Caught by adversarial review before release;
regression-tested by
`TestRefusalNamesTheProfileItsEvidenceCameFromNotTheRepresentative`.

## Why binding was rejected

Binding requires joining two identifiers. Both were investigated against the
live system rather than assumed, and no sound join exists:

1. **`prefs.js` does not carry a Zotero user ID.** Zotero's own
   `Zotero.Users.getCurrentUserID()` reads it from `zotero.sqlite`
   (`SELECT key, value FROM settings WHERE setting='account'`), not from
   preferences. Verified against Zotero 7 source, `chrome/content/zotero/xpcom/users.js`.

2. **The one shared field is not comparable.** `prefs.js` has
   `extensions.zotero.sync.server.username`, and the Web API's `keys/current`
   returns `username`. They do not match: the preference holds the *email
   address* used to sign in, while `keys/current` returns the *account
   username*. Confirmed on the maintainer's own machine, where the two differ.
   The table joining email to username (`settings` / `account` / `emails`)
   exists only in `zotero.sqlite`, and `keys/current` does not return an email.

3. **`zotero.sqlite` is not reliably readable while Zotero is running.** A
   read-only open (`file:…?mode=ro`) fails with `database is locked` whenever
   Zotero holds a write transaction; observed as consistently locked during
   normal use, with an earlier successful read being a lucky window. The only
   mode that always succeeds is `immutable=1`, which asserts to SQLite that the
   file cannot change — false for a live database, and licensed to return torn
   or stale pages.

Binding would therefore make a safety guard depend on a source that is
intermittently unavailable and, when forced, unsound. A guard that refuses
non-deterministically is worse than one that is honestly over-broad.

## Consequences

**Accepted:** a profile for an unrelated account can refuse a correct upload.
This errs toward refusing, which is the recoverable direction: the refusal
names the contributing profiles, `ZOTERO_PROFILE_DIR` selects a different one, and
`--allow-zotero-cloud` overrides outright. The opposite error — permitting a
silent misroute into a storage plan the operator does not use — is the original
defect and is not recoverable, because the bytes are already billed.

**Accepted:** a WebDAV operator running zotio on a machine with no Zotero
profile (CI, a server) gets no protection at all. There is no local evidence to
read. Closing this needs zotio-owned, per-target configuration stating the
storage backend, which is a separate decision and a new config surface.

**Accepted:** the guard does not model Zotero's account-state gate.
`getEnabledForLibrary` returns false before consulting any preference when
there is no current user ID, so a logged-out or database-reset profile is
reported by zotio as cloud-and-enabled while Zotero itself considers file
syncing disabled. That state lives in `zotero.sqlite` and is unreachable for
the reasons above.

**Revisit if** Zotero exposes account identity through the local API (which
already reports the running profile's real library ID at
`/api/users/0/items`, so the information is present on that surface), or
publishes a stable read path for account state that does not contend with the
running application's write lock.
