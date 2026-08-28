# Changelog

Notable changes to zotio. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow [SemVer](https://semver.org/).

## [Unreleased]

### Fixed

- **Connector imports now return permanent parent and attachment keys.** Zotero
  can expose a connector-created item only after its stored PDF finishes
  attaching. zotio previously checked before that attachment, reported the
  write as applied without keys, and left acquisition clients unable to record
  the successful import. It now repeats the bounded title-and-type lookup after
  the file lands, confirms the one attachment child, and returns both keys.
  Missing or ambiguous read-back becomes a conflict instead of false success.

## [0.21.0] — 2026-08-24

### Changed — breaking

- **Local tag reads now preserve Zotero's `(name, type)` identity.** Manual and
  automatic tags with the same text remain separate through sync, filtering,
  collection scoping, and full-text search. Scripts that assumed one result per
  tag name must keep the type or deliberately merge the two rows.
- **Citation health and validation results are more selective.** Conference
  papers and preprints no longer fail `missing_citation` because Zotero does
  not define `publicationTitle` for those types. `items enrich --validate` now
  adds provider-confirmed volume, issue, and page gaps. Existing finding counts
  and `library health --for citation` gate results can change.
- **A cancelled sync now reports every requested resource.** Dequeued, queued,
  and not-yet-enqueued resources each produce one result instead of disappearing
  from the report. The command still returns its cancellation error, but
  consumers will see a complete result array.
- **Multi-route capability records now include `routes`.** Each route declares
  its own write target and preconditions, so a connector route no longer
  inherits an unrelated Web API storage requirement. The top-level fields keep
  describing the default route. Strict JSON decoders must accept the new field.
- **The local store schema advances to version 6.** The first open reindexes
  synced full-text rows from raw JSON to PDF body text, so JSON field names no
  longer match as prose. Older binaries refuse the newer database instead of
  reading it with stale search semantics.
- **The MCP `search` limit is now an integer from 1 through 100.** Larger,
  fractional, and non-finite limits are rejected before a query starts instead
  of allocating the complete result set before output framing.

### Added

- **Citation health now has a complete automated repair path.**
  `items enrich --missing-citation` fills only missing creators, title, date,
  venue, volume, issue, and page fields that the stored item type supports and
  CrossRef supplies. Items without a DOI are skipped with a specific next step.
  Health, audit, and bibliography checks now recommend this command.
- **Bibliography exports now cover CSL-JSON, BibTeX, BibLaTeX, and RIS.**
  `items bibliography` exports any shared scope in these machine-readable
  formats. CSL-JSON uses unique Better BibTeX citation keys.
  `collections export` now writes one BibTeX, RIS, or CSL-JSON artifact across
  the selected collection and its subcollections.
- **PDF full-text search now returns the source item.** `search --fulltext`
  searches only the synced PDF body and returns each matching parent item key,
  attachment key, title, and a UTF-8-safe snippet capped at 4096 bytes. It
  excludes orphaned attachments and trashed parents. The MCP `search` tool
  exposes the same mode through its `fulltext` argument.
- **Library statistics can show item intake by month or year.**
  `library stats --added-by month|year` groups top-level citeable items by a
  valid Zotero `dateAdded` value. It ignores malformed dates. The result does
  not create snapshots or retain history.
- **Local item lookup now accepts URLs, OpenAlex work IDs, and exact titles.**
  `items find --url`, `--openalex`, and `--title` normalize their inputs and
  return the existing item result shape. URL matching ignores host case,
  fragments, and a trailing slash. Title matching ignores case and surrounding
  whitespace. OpenAlex matching accepts work IDs and `openalex.org` work URLs.
- **Offline tag reads now honor the full tag filter set.** Exact-name,
  contains, prefix, collection, and tag-type filters work against the synced
  mirror without requiring the live Zotero API.
- **Agents can discover keyless connector create routes.** Capability data now
  advertises connector-backed item creation when the live desktop can perform
  the write without a Web API key.

### Fixed

- **Top-level syncs no longer delete child rows.** An `items-top` refresh keeps
  attachments, notes, and annotations. A `collections-top` refresh keeps nested
  collections. Incremental local sync also advances its cursor from page
  versions when Zotero omits the version response header.
- **Store version guards and missing-row sweeps now use one atomic contract.**
  Nested `data.version` values participate in monotonic write checks and
  version resets. Missing-row detection and deletion now run under one writer
  lock and one transaction.
- **`auth logout` removes credentials under agentcookie management.** The
  command no longer reports success while leaving an older
  `credentials.toml` API key on disk.
- **Cancelled MCP mirror callers no longer leave two goroutines each.**
  Waiting for the single mirrored-command slot now stops directly on context
  cancellation.
- **Exact identifier lookup now keeps normalization and matching aligned.**
  DOI URLs match bare stored DOIs and vice versa. arXiv URLs require an
  `arxiv.org` host and a complete identifier. `arXiv:<id>`, `PMID:<id>`, and
  `Citation Key:<key>` accept Zotero's no-space Extra syntax without weakening
  token boundaries.
- **Quiet CSL-JSON bibliography failures stay quiet.** A missing or conflicting
  citation key still returns the precondition error, but `--quiet` no longer
  writes its failure envelope to stdout.
- **Release packages now carry complete third-party terms.** The generated
  notice file includes every shipped license, notice, patent grant, SQLite
  term, and Go standard-library license across all six release targets. The
  file ships in archives, system packages, MCP bundles, and the container.

## [0.20.1] — 2026-08-23

### Fixed

- **A connector create no longer reports an empty key for a write that
  succeeded.** `items create --via connector`, and every command that routes
  through it, recovered the new item's key with a single `/items/top` lookup.
  Zotero's `saveItems` has already committed by then, so the item exists — a
  miss means it has not surfaced in that list yet, and the one-shot lookup
  reported no key for a create that worked. A consumer that reads an empty key
  as a failed apply then re-derives the write and creates the item twice; papio
  reported exactly that duplicate risk. The recovery now polls within a bounded
  window. Ambiguity is deliberately not retried: more than one match will not
  resolve itself, so the refusal to guess is returned on the first lookup.

## [0.20.0] — 2026-08-23

### Added

- **`attachments add --via connector` attaches a file to an item that already
  exists, without using Zotero's cloud storage.** A Web API stored upload always
  lands in Zotero's own cloud storage and is billed to that plan, so it is
  refused when the desktop keeps files elsewhere — a personal WebDAV server, or
  file syncing turned off. Zotero's connector cannot address an item that
  already exists, so until now there was no route at all: the only remedies were
  attaching by hand in Zotero desktop, or a linked file that never syncs.

  The new route creates a temporary parent and the file in one connector
  session, so the bytes go through the running desktop into whatever file store
  it actually uses; moves the attachment onto your item with a single
  `PATCH {"parentItem": …}` guarded by `If-Unmodified-Since-Version`; then
  trashes the temporary parent. Moving it relocates no bytes, because every
  storage name — the local directory, the upload zip, and both WebDAV remote
  names — derives from the attachment's own key rather than its parent's.

  The route is **opt-in**. `--via auto` still refuses on such a library, so no
  existing caller changes behaviour; the refusal now names `--via connector` as
  its first remediation. It is retry-safe: an identical file no-ops against the
  target by content hash, creating nothing. It refuses rather than guessing when
  the temporary parent holds more than one attachment, and it never trashes that
  parent unless the file has already left it, so a failure is recoverable by
  hand from the keys it reports. Requires Zotero desktop running.

  Every fact this rests on was measured against a real WebDAV-backed library
  before the route was written, and the route itself was then smoke-tested on
  scratch items: see `dev/field-report-2026-08-22-papio-round2.md`.

### Fixed

- **A staged filename's DOI is no longer truncated at a parenthesis.** Tools
  that stage papers by identifier percent-encode the name, because a filename
  cannot contain `/`. Only `%2F` was decoded, so a surviving `%28` ended the
  DOI match early: `10.47205%2Fjdss.2023%284-ii%2934.pdf` was looked up as
  `10.47205/jdss.2023` and 404ed at both registries, while the full DOI
  answers 200 with its record. Parentheses are legal in a DOI suffix. Staged
  filenames are now fully percent-decoded, falling back to the previous `%2F`
  handling when the name contains an invalid escape and so was never encoded.
  A trailing `)` that has a matching `(` is also kept now: the trim that
  removes prose brackets from `(see 10.1000/foo)` used to turn
  `10.1000/ends(x)` into the unbalanced `10.1000/ends(x`. Reported by papio;
  verified live against CrossRef on 2026-08-22.
- **An arXiv ID in a staged filename now resolves.** `arxiv-2301.08745.pdf`
  produced no identifier, because filename extraction looked for DOIs only and
  the existing arXiv patterns require `arxiv.org/abs/` or a literal `arxiv:`.
  Names like `arxiv-<id>`, `arXiv_<id>` and `arxiv:<id>` now yield the paper's
  DataCite self-DOI, `10.48550/arXiv.<id>`, so DataCite resolution and the
  arXiv field mapping handle it with no second resolution path. The `arxiv`
  token is required: a bare `2301.08745.pdf` still yields nothing rather than
  an invented identifier, and an explicit DOI in the name still wins.
- **An unresolved import entry now says why it is unresolved.** A PDF with no
  extractable identifier became `status: unresolved` with an empty note, which
  reads as a missing registry record rather than a failed extraction. The note
  that existed was unreachable — it sat under the `create` action, which is
  only assigned once a DOI has been found. Such entries now name the failure
  and the next step: `import apply` hands them to Zotero's PDF recognizer.

- **DOI import resolves DataCite DOIs, not just CrossRef ones.** Every
  `10.48550/arXiv.*` DOI was marked `unresolved` with
  `fetching CrossRef metadata: HTTP 404: Resource not found`, because that
  prefix is registered with DataCite and a CrossRef-only lookup can never
  resolve it. The DOI was well-formed, registered, and resolvable — only the
  registry was wrong — and the manifest entry blocked the import with no
  consumer-side workaround. `import resolve`, `import doi`, `import discover`
  and `import scan --resolve` now ask CrossRef first and fall back to DataCite
  when CrossRef reports no such record, which also covers Zenodo, Dryad,
  figshare and other DataCite registrants. A CrossRef timeout or 5xx is *not*
  treated as "no record": retrying at a registry that does not own the DOI
  would turn a truthful transient error into a misleading permanent one. When
  both registries miss, the error names both attempts, because the
  single-registry message is what sent a downstream consumer hunting for a
  malformed DOI. arXiv self-DOIs land on the same `archiveID`/`repository`/
  `extra` fields as an arXiv-ID import, so the same paper imported either way
  produces the same item. Reported by papio; verified live against both
  registries on 2026-08-22.

- **`--compact`, and therefore `--agent`, no longer empties rows it does not
  recognise.** `compactFields` keeps an allow-list of item-ish field names, so a
  hand-written report row sharing none of them was reduced to `{}`. Measured
  live: `zotio --agent journal list` answered ten empty objects and the entire
  audit trail was gone, on the documented agent path. A row the allow-list
  matches nothing in is not a Zotero item record, so compaction now strips
  nothing rather than everything — the same reasoning as the existing nested
  envelope branch, which was added for this failure mode but only covered rows
  whose fields are nested under `data`. Rows the allow-list does recognise are
  still compacted, so `collections list`, `searches list` and `tags list` keep
  dropping `links`/`meta`/`version`/`library` as before.

### Changed — breaking

- **Four local-mirror reads now answer the provenance envelope instead of a
  bare JSON array.** `items missing-pdf`, `items stale`, `items unfiled` and
  `items venues` return `{"meta": …, "results": […]}` under `--json`/`--agent`,
  like every other record read, and honour `--select`/`--compact` inside it.
  They were the last record reads left behind by the envelope rollout that
  migrated `items find`, so a jq pipeline written against one read command
  broke on these four. The cost was measured downstream: a consumer decoded
  `items find` into a slice, that command gained the envelope, the decode
  failed on every lookup, and a neighbouring command still decoding fine as a
  bare array is what made a decode bug look impossible.
  **Migration:** read `.results[]` instead of `.[]`. Consumers that must
  tolerate both can branch on the top-level type.
- **analytics answers the provenance envelope in every mode instead of three
  different JSON shapes.** Every invocation now returns `{"meta": …, "results":
  […]}` where `results` is always an array of rows each carrying a `count` —
  the same envelope every other read command uses. `--group-by` was a bare
  array, `--type X` was a bare object, and the no-flag mode was a map whose
  keys carried the data (`{"items": 1}`), which could not be sorted, filtered,
  or fed to `--select` or `--csv` because the data lived in key names rather
  than row fields. It is now one `{"resource_type", "count"}` row per mirrored
  resource kind, sorted by count descending then `resource_type` ascending;
  `--group-by` rows remain `{"value", "count"}` sorted by count descending
  then `value` ascending with `--limit` applied, and `--type` remains a
  single `{"resource_type", "count"}` row. `--plain`, `--csv`, `--select`, and
  `--compact` were silently ignored, and a piped invocation with no format flag
  returned a human tab table instead of JSON; all now behave as they do on
  every other read command, including `filterFields`/`compactFields` inside the
  envelope and JSON-by-default when piped. `compactFields` is an allow-list, so
  the two new row fields had to be added to it: without that, `--compact` — and
  therefore `--agent`, the documented agent path — dropped `resource_type` and
  `value` and left every row a bare `{"count": N}` with the grouping key gone.
  Found by running the built binary against a real mirror, after the migration
  passed every unit test. `meta.group_by` is new and names the
  grouping field in `--group-by` mode, so a consumer receiving `{value, count}`
  rows knows whether `value` is a year, tag, or creator without re-reading its
  own command line. The no-flag human table previously iterated a Go map, so
  its row order was nondeterministic between runs; it is now sorted.
  **Migration:** `.[]` becomes `.results[]`, and `.items` becomes
  `.results[] | select(.resource_type=="items") | .count` — the second is the
  disruptive one, because a map lookup has no counterpart once the data moves
  out of the keys and into rows. Consumers that must tolerate both shapes can
  branch on whether the top-level JSON has a `results` array.

### Documentation

- **The `--via connector` route is now documented as unsafe to drive in a tight
  loop.** `import apply`, `import pdf` and `items create` carry the warning in
  their help text: the route hands work to the running Zotero desktop, which
  surfaces its own progress UI that zotio cannot dismiss, and no connector
  endpoint closes or completes a save session. Prefer one invocation carrying
  many records — `import file --via connector` and `items create` already share
  a single connector session, while `import apply` opens one per manifest
  entry. The warning also states that `--rate-limit` governs only Web API
  requests and does not pace connector calls, which was silently true before.
  Prompted by an operator observation on 2026-08-22, where roughly 78
  consecutive one-per-item invocations left Zotero desktop unresponsive with
  progress windows accumulating. No mechanism for the window accumulation has
  been proven, and the warning claims none; see
  `dev/field-report-2026-08-22-papio.md` finding 3 for the evidence, the two
  ruled-out theories, and the two remaining leads.

## [0.19.0] — 2026-08-18

One defect, found by running zotio against a Zotero desktop configured to keep
attachment files on a personal WebDAV server: every stored attachment upload
went into Zotero's own cloud storage instead, silently, because the Web API
file-upload protocol has no other destination and Zotero's file store is a
client-side setting the API never reports.

The obvious repair — route the bytes through the desktop connector — turns out
to be impossible for an item that already exists, and that was established
empirically rather than assumed: `POST /connector/saveAttachment` resolves
`parentItemID` only through its own save session, so a real library key answers
`500` on a live session and `400 SESSION_NOT_FOUND` otherwise. So the honest
behaviour is a refusal, and the one route that does reach a WebDAV store —
creating item and file inside a single connector session — had to be repaired
to make the refusal actionable.

### Changed — breaking

- **A stored attachment upload is refused when Zotero desktop keeps files
  somewhere else.** `attachments add --mode stored` and `import apply
  --attach-mode stored` exit 9 with a `precondition_unmet` envelope
  (`zotero_file_storage`) naming the hazard that was detected and the routes
  that do reach the configured store. Where the destination could not be
  established at all — an unreadable profile, or a storage protocol this
  version does not model — the envelope says exactly that instead of naming a
  store it never read. Previously the bytes went to Zotero's cloud storage and
  were billed to that plan with no warning.
  A stored upload discovered mid-run — `import pdf
  --on-duplicate attach` classifies duplicates while applying — is stopped by
  the same guard at the upload itself, but reports as a failed operation rather
  than exit 9. Absence of Zotero altogether stays permissive, so machines with
  no desktop are unaffected; a profile that exists but cannot be read or
  understood is refused, because inability to evaluate evidence that
  demonstrably exists is not evidence of its absence. Group *storage mode* is
  unaffected — Zotero always uses its own storage for groups — but a group
  upload is still refused when group file syncing is switched off.
  **Migration:** attach in Zotero desktop, use `import apply --attach-mode
  stored --via connector` for new items, or pass `--allow-zotero-cloud`.

### Added

- **`--allow-zotero-cloud`** uploads into Zotero's cloud storage anyway. Root
  persistent flag, mirroring `--allow-destructive`, and exposed on the MCP
  surface in the same write-gating set so an agent can act on the refusal's
  own remediation.
- **`doctor` reports `file_storage:`** — what the discovered `prefs.js`
  currently indicates about where Zotero keeps attachment files for the
  targeted library, whether uploads are consequently refused, and what to do
  about it, including the contributing profile paths and any it could not read.
  Reported as unknown when no Zotero profile is found — which allows uploads —
  and also when the protocol is one zotio does not model, which does *not*:
  a destination that cannot be established is refused like any other.

### Fixed

- **`import apply --attach-mode stored --via connector` no longer fails on a
  PDF with no source URL.** Zotero's `Attachments.importFromNetworkStream`
  rejects an empty `url`, which the connector reported as a bare `500` *after*
  committing the parent, leaving an item with no attachment and no reason —
  precisely the case for a locally scanned paper. The file's own `file://` URI
  is now used as provenance. `connector.SaveAttachment` rejects an empty source
  URL up front so the failure is named rather than opaque.
- **`localFileURL` builds a real file URI.** It concatenated `"file://"` with
  the path, so spaces, `#`, `?`, `%` and non-ASCII went unescaped (a `#` in a
  filename truncated the URL at the fragment) and a Windows drive letter parsed
  as an authority. Now constructed with `net/url`, yielding `file:///C:/...`
  and RFC 8089 UNC form. Affects `import pdf` as well.
- **`doctor` no longer claims the connector offers stored attachments
  generally.** It cannot attach to an already-existing item, and the text said
  otherwise.
- **Precondition failures now show their remediation without `--json`.** Every
  `precondition_unmet` carries a list of concrete next steps, but
  `writePreconditionUnmetEnvelope` returned early unless JSON was requested, so
  the terminal told operators they were blocked and never how to proceed. The
  human error now renders as the failure, a blank line, then a `What to do
  instead:` list. JSON output is unchanged: the envelope already carries the
  list in a parseable field, so the error there stays a single line for logs.
  Affects every precondition, not just the storage guard.
- **Errors printed twice.** The root command silenced Cobra's usage output but
  not its error output, so `main` and Cobra each printed the same message.
  Cobra's is now silenced; `main` is the only writer.
- **The storage refusal blamed the wrong thing when Zotero was closed.** It
  always said the connector cannot attach to an item that already exists —
  true for `attachments add`, but wrong for a create that fell back to the Web
  route because the desktop was unreachable, where the fix is simply to start
  Zotero. The refusal now names the actual obstacle: an unaddressable existing
  item, a desktop that is not running, a non-local `base_url`, a group write,
  or an explicit `--via web`. The apply-time backstop serves callers on both
  sides of that split and so makes no claim either way.
- **A profile that could not be READ was treated as no profile at all**, so an
  oversized, permission-denied, non-regular, wrongly-encoded, or explicitly
  pinned-but-missing `prefs.js` silently authorised the very upload this guard
  exists to refuse. `zoteroprefs.Load` had always documented that as operator
  error; the guard disagreed with it. Inability to evaluate evidence that
  demonstrably exists is now a refusal, with `--allow-zotero-cloud` still
  releasing it. Absence of Zotero altogether stays permissive.
- **Unreadable sibling profiles vanished from the reading.** `loadAcross`
  returned a clean representative whenever any one profile parsed, so a
  WebDAV profile with a corrupt `prefs.js` was invisible next to a readable
  cloud one. Unreadability is now a counted hazard, surfaced by
  `AnyUnreadableProfile`, and reported by `doctor`.
- **The refusal reconstructed its own routing by probing a second time**, so it
  reported what was true at the probe rather than at the decision. An automatic
  fallback whose desktop finished starting was reported as the operator forcing
  `--via web`, and `--via web` with Zotero closed was answered with "start
  Zotero and run this again", which fixes nothing. The route now carries the
  cause recorded when it was chosen.
- **Every connector failure was reported as "Zotero is not running."** A
  cancelled context, a deadline, and an HTTP error all say something else;
  two of the three were false. Failures are now classified.
- **Group scope matched `/groups/` anywhere in the path.** A reverse-proxy or
  deployment base such as `/proxy/groups/tenant/v1` was classified as a group,
  which skips the personal-WebDAV refusal entirely. The library prefix is now
  anchored to the final two path segments, as its contract always claimed.
- **One WebDAV-shaped remediation was attached to every refusal**, telling
  group callers to use a connector that has no group parameter, and telling
  sync-disabled callers that Zotero would sync the bytes. Remediation is now
  chosen from the reason.
- **A group refusal said the upload consumes "the account's storage plan".**
  Zotero bills group files to the group owner's quota.
- **`doctor` printed the representative profile's sync flag while refusing on
  the union**, so it could show an enabled library and refuse it in the same
  line. Both now read the same evidence.
- **`doctor`'s human output dropped `file_storage_hint` and
  `file_storage_profile`** — the actionable half was `--json`-only, the same
  defect fixed above for preconditions.
- **The apply-time backstop re-used preflight's snapshot**, so a desktop
  reconfigured mid-invocation was judged by a stale reading. It now takes one
  fresh reading at the planning/mutation boundary.
- **`prefs.js` parsing accepted a truncated prefix of a string expression**
  (`"zotero" + "-future"` read as `zotero`, positive evidence of cloud) and
  **turned a type mismatch into a positive hazard** (a quoted string in a
  boolean slot became `false`, manufacturing a syncing-disabled refusal). A
  present-but-malformed known key is now indeterminate rather than either a
  confident default or a fabricated negative. UTF-16 is rejected instead of
  read as an empty preference set, and the per-line scanner cap no longer
  fails a file well under the whole-file limit.
- **Profile discovery missed real installations.** The Linux no-INI fallback
  scanned `<root>/Profiles`, but Zotero puts Linux profiles directly under
  `~/.zotero/zotero`; a stale `profiles.ini` entry suppressed the directory
  fallback entirely, hiding a live unlisted profile; and Snap and Flatpak
  layouts had no candidates. All three are fixed.
- **A refusal did not say whose configuration produced it.** Profile evidence is
  machine-wide and cannot be bound to the Zotero account a command targets
  (`dev/adr/0006-unbound-profile-evidence.md`), so a profile belonging to a
  different account can refuse a correct upload. Refusals now name the profile
  directories the evidence came from — or, where a profile could not be read,
  the paths that could not be read — alongside the `ZOTERO_PROFILE_DIR` pin,
  making that case recognisable instead of inexplicable. Preconditions can now
  resolve remediation from the failure rather than from a static
  per-precondition list.
- **A refusal could name a profile that contradicted it.** Hazards were unioned
  across profiles as plain booleans, then reported against the risk-ranked
  representative — but `riskRank` orders storage *modes* only, so a
  syncing-off or unrecognised-protocol hazard is routinely carried by a
  profile that lost the ranking. "File syncing is off" could name a profile
  with file syncing on, sending the operator to check the one file guaranteed
  to disagree. Hazards are now attributed to the profiles that evidenced them,
  and every contributor is named. `doctor` reports the contributing profiles
  and the paths of any unreadable ones.
- **A stored create that committed a parent and then failed to attach its file
  reported nothing usable.** The connector route returned no evidence at all,
  and the Web route returned a bare key map that the human renderer printed as
  a Go map literal. Both now report the route, the created item's identifying
  evidence, and a deterministic next step. This is evidence only: the mutation
  model stays non-transactional and nothing is rolled back.
- **A mistyped subcommand printed help and exited `0`.** `zotio items
  empty-trash` — a command that does not exist — reported success having
  mutated nothing, and so did any unknown subcommand under a grouping command
  (`items`, `tags`, `collections`, …); only root-level typos exited non-zero.
  For the scripted and agent consumers this CLI is built for, that is a
  silently skipped operation indistinguishable from a completed one. Unknown
  subcommands now exit `2` with the conventional usage error and a
  "Did you mean this?" suggestion. Groups invoked bare still print help and
  exit `0`. Found while smoke-testing this release against a live library.

### Security

- **A WebDAV password containing `/`, `?` or `#` could reach doctor output and
  the refusal envelope.** `WebDAVHost` truncated at the first path delimiter
  before stripping userinfo, so the `@` was discarded along with the host and
  the credential prefix was returned as the hostname. Userinfo is now stripped
  first, and non-printable characters decoded from prefs.js are dropped so a
  crafted value cannot rewrite the terminal line it is reported on.

### Documentation

- `AGENTS.md` and `dev/zotero-api-coverage.md` record both connector
  invariants — session-local `parentItemID`, and the mandatory non-empty `url`
  — with the evidence and dates behind them. The guard is documented as
  best-effort rather than fail-closed: `prefs.js` is flushed lazily and can lag
  the running application in either direction, and Zotero's `-P` switch means
  the default profile may not be the running one.

## [0.18.0] — 2026-08-17

Four consecutive audit passes over one body of work, plus the export-durability
repair they exposed. No new commands: this release is entirely about paths that
reported success while losing data — a failed export destroying the export it
was replacing, two writers to one file silently overwriting each other, writes
dispatching an unguarded precondition, pagination stopping early and reporting a
complete result, and read-only commands writing. Every fix carries a negative
control: the bug reinstated, the test observed to fail, the code restored.

Breaking for scripted and agent consumers, so pre-1.0 it ships as a minor with
no major-version signal. papio's minimum-zotio floor is unchanged (0.13.0); its
`items tags add --automatic` path is affected only by the fail-closed
precondition below.

### Changed — breaking

Every item here changes an exit code, a JSON value, or a command's observable
behaviour.

- **A file-producing export refuses an output path that is not a regular file.**
  `--output` on a symlink previously wrote through to its referent; publishing by
  rename would instead replace the link, and resolving it first would widen a
  time-of-check/time-of-use window across the whole export. Directories, FIFOs,
  sockets and devices are covered by the same refusal. **Migration:** pass the
  resolved path.
- **`export`, `collections export` and `annotations export` exit 9 when another
  zotio writer holds the target.** They previously took no lock at all, so two
  runs to one file raced. The check happens before the first API request.
  `export snapshot`, `collections bundle` and the `vault` writers already
  behaved this way. **Migration:** retry after the active writer exits.
- **`--deliver=file` can now be skipped.** A busy target makes delivery warn on
  stderr and leave the artifact alone; the command's exit code is unchanged,
  matching delivery's existing warn-only contract. **Migration:** verify the
  delivered file, not just the exit code.
- **A `<output>.lock` sibling is left next to every published artifact.**
  `export snapshot` used to delete its own; with the key now shared across
  commands, unlinking it can split the lock namespace. Pre-existing content at
  that path is never truncated. **Migration:** exclude `*.lock` when globbing an
  output directory.
- **`collections export` fails instead of truncating** when the server ignores
  `start`. The CSL-JSON walk `break`ed and the subcollection walk returned its
  partial key list, both exiting 0 with a complete-looking artifact. **Migration:**
  a run that previously "succeeded" with partial output now reports the
  pagination error the text path already used.
- **Writes fail closed when the write plane's version cannot be resolved.**
  `items enrich`, `items preprint-check --fix`, `items tags`, `items move`,
  `items duplicates resolve`, `searches materialize` and `journal undo`
  previously dispatched an unguarded PATCH, or sent a local-plane version as a
  Web API precondition. **Migration:** a write that formerly went through
  unguarded now fails with a stated reason; resolve the key/route it names.
- **`searches run` reports the result as unavailable** instead of falling back to
  a query whose filter parameter the plane may ignore — which answered with the
  entire unfiltered library. **Migration:** treat unavailable as unavailable; do
  not read the previous output as a saved-search result.
- **`library health --verify-files` skips with an unmet precondition** under a
  Web API configuration, instead of checking every attachment through a
  local-only endpoint and reporting them all broken.
- **`items find --pmid`, `--citekey` and `--arxiv` no longer prefix-match.**
  `smith2023` matched `smith2023a` and PMID `123` matched `12345`. **Migration:**
  result sets shrink to exact token matches.
- **`items venues` reports different years and item types.** Years are parsed
  rather than substring-sliced (so `April 2023` and `n.d.` no longer yield `Apri`
  and a year), undatable rows are excluded, and the type is the venue's most
  common rather than an arbitrary row.
- **Over-limit imports fail instead of truncating.** A manifest over 64 MiB and a
  translator page over 4 MiB were silently cut short and reported success. The
  limits themselves are unchanged.
- **PDF-coverage percentages change.** `library stats` and `collections stats`
  counted attachment rows against a denominator of parent items, so coverage
  could exceed 100%. Both now count distinct parents.
- **`items trash` returns different pages.** Mirror rows were appended after
  pagination and unsorted, and the live side fetched one unpaginated page (25
  results from the Web API).
- **Connector creates and file imports report different JSON.** A create whose
  collection filing failed was reported as failed with no key; it is now reported
  committed, with its key, and the filing failure named as a follow-up. File
  imports report what Zotero actually imported rather than the parsed-record
  count.
- **`items annotations --color <name>` now matches rows.** Every documented
  colour name returned zero rows, because the name was compared against the
  stored hex.
- **`items summarize --max-chars` counts characters, not bytes.** A CJK summary
  was cut to roughly a third of the requested length, so non-Latin summaries get
  longer.
- **`tail` writes events to the command's output stream**, not process stdout, so
  redirected output receives them. Where the API does not implement `/deleted` —
  which the Zotero local API does not — the feed advances and says so once,
  instead of replaying the library on every poll.
- **Read-only commands no longer write.** `doctor`, `analytics`, `workflow
  status` and `items bibliography` no longer take the writer lock or persist
  state, and `agent-context` no longer creates `~/.zotio` as a side effect.
  **Migration:** if you relied on `agent-context` creating that directory, run
  `zotio init` or any writer first.
- **`workflow status` and `analytics` exit 0 on a fresh install**, reporting that
  nothing is archived instead of failing with a store-open error.

### Fixed

**A failed export no longer destroys the export it was replacing.** Every
file-producing export truncated its target before fetching anything, so any
failure mid-run published the wreckage. Measured on a 4,624-item library:
`zotio export items --output x.jsonl` against an unreachable API turned an
11,299,615-byte artifact into 0 bytes, and `zotio collections export NOSUCHKEY
--output refs.bib` exited non-zero after leaving 1,086 complete, valid
`@article` entries on disk — syntactically perfect, silently incomplete, and
indistinguishable downstream from a real bibliography. Neither needed any
concurrency to reproduce.

Exports now publish atomically: output is streamed into a temporary file in the
target's own directory and renamed over the target only on success, so a failure
leaves the previous artifact byte-for-byte intact and creates nothing at all when
there was no previous artifact. A reader can no longer observe a half-written
export. The contract is same-directory replacement using the host filesystem's
rename semantics — process-failure publication safety, not power-loss
durability; nothing is fsynced, because an export is a regenerable projection.

Two behaviour changes fall out of publishing by rename, both deliberate:

- An existing output path that is **not a regular file is now refused** with an
  actionable error instead of being written through. Previously `--output` on a
  symlink wrote to its referent; renaming would instead have replaced the user's
  link with a file, and resolving the link first would have widened a
  time-of-check/time-of-use window from a single `open` to the whole export.
  Refusing is the only option that is both safe and honest. This also covers
  directories, FIFOs, sockets and devices, which these paths already failed on.
- `annotations export --output` **preserves an existing file's permissions**.
  It used `os.WriteFile`, which leaves an existing mode alone, so normalising to
  `0600` would have silently tightened a mode the user chose. `export` and
  `collections export` still publish `0600`, matching the mode they already
  forced.

Streaming to stdout is unchanged, including its flush-on-failure behaviour: a
pipe consumer has already been handed every byte generated before the failure,
and `collections export` still writes stdout unbuffered so a broken pipe is
detected promptly rather than after another page is fetched.

**`collections export` no longer publishes a silently truncated export when the
server ignores pagination.** The three walks disagreed: the text path failed
loudly on a repeated page, while the CSL-JSON path `break`ed and the
subcollection path returned its partial key list — both reporting success. A
server that ignores `start` therefore produced a complete-looking JSON array
holding only the first page, or an export missing whole subtrees. Both now fail
with the same pagination error the text path already used. This had to land
alongside atomic publication, which would otherwise have faithfully committed the
truncated result.

**Two exports to the same file no longer race, and the writer lock no longer
releases itself early.** `export snapshot --output F` coordinated on `F`, a plain
`export --output F` took no lock at all, and `--deliver=file:F` was exempt from
both — three writers to one named file under three different policies. Atomic
publication (above) stopped either writer from being observed torn, but two
writers still silently overwrote each other's complete artifact. Every command
that names an output path now derives its lock through one helper, so the same
named target always yields the same lock identity, including through symlinked
ancestors: `export`, `collections export`, `annotations export`, `export
snapshot`, `collections bundle`, and the `vault` writers. A busy target fails
fast with exit 9 before the first API request, so a collision costs no traffic.
`--deliver=file` joins the same namespace but stays secondary: a busy target
skips the delivery with a warning and leaves the command's exit code alone,
because the command's real work already succeeded.

Repairing the lock machinery had to come first. `withPathWriterLock` could not
hold two locks: re-entering a path the same command already held deferred a
release of the **outer** ownership, so the outer transaction ran on to
publication with its lock already gone; reuse only ever inspected the innermost
lock, so an installation writer that nested an output lock could not re-enter its
own installation lock; and the installation wrapper identified its handoff by
owning command alone, so it would have released an output-scope lock the same
command acquired. All three were latent — every call site took exactly one path
lock — and all three would have become live the moment exports started locking.

`export snapshot` no longer deletes its lock file on success. Now that several
commands share that path, unlinking it can drop an inode a live acquirer already
holds and let a third writer lock a fresh inode under the same name, splitting
one namespace into two "locked" writers. Acquisition also never truncates or
rewrites a pre-existing `<target>.lock`: the sibling lives in the user's output
directory, so its bytes are not zotio's to destroy. The visible cost is a
retained `<target>.lock` next to each published artifact, which was already
documented as harmless — it is not evidence of an active writer.

ADR-0005 is rewritten around the four axes these bugs kept collapsing:
derivation dependency (does a stale load corrupt the next publication?),
coordination scope and key, collision policy as a property of the scope rather
than of each writer, and publication mechanism.

A fourth pass over the same body of work, this one from findings a weak model
produced against a tree that was being rewritten underneath it. Of 48 findings,
16 were noise — false positives, duplicates, or already fixed — and validating
them individually mattered more than fixing them: three of the surviving fixes
had to be reverted because they inverted deliberate contracts, each one caught
by an existing test whose *name* stated the behavior being removed.

Preceded by three earlier passes: 24 findings from a static audit, then 14
defects that seven reviewers found in those fixes, then two pre-existing issues
those reviewers flagged as out of scope. Every fix carries a negative control —
the bug reinstated, the test observed to fail, the code restored — because the
first pass shipped three tests that could not fail, one of which copied the
production SQL it was meant to check.

- **`items annotations --color` matched nothing for every documented value.**
  The flag's own help lists colour names (`yellow`, `red`, `green`, …) but the
  filter compared that name against the stored hex, so `--color yellow` returned
  zero rows while `--color '#ffd400'` returned 456 on the same library. A
  name-aware helper already existed and simply was not called.
- **An unresolvable Web API write route was reported as a version conflict.**
  With hybrid routing configured and no cached user ID, a failure to resolve the
  write base was discarded at three separate sites, and the write then fell
  through to a path that reported `status: conflict` with the server's opaque
  `Zotero-Server-ID not provided` — sending users to look for a concurrent edit
  when the real cause was, for example, an expired key. The resolver error is
  now retained and surfaced: `could not resolve Zotero Web API write route:
  resolving Zotero user ID: keys/current returned HTTP 403`. The deliberate
  non-latching of a failed resolution is unchanged, so the next write still
  retries.
- **`items update` sent a precondition read before the plan was applied.** The
  version was fetched outside `Apply` and frozen, and a 412/428 rejection was
  reported as a generic failure rather than a conflict. It now resolves the
  precondition on the write plane at apply time through the shared guard.
- **Local reads failed hard under WAL contention.** `QueryItems`, `QueryTrash`,
  and `QuerySimilarityCandidates` bypassed the BUSY-retry helper their siblings
  used, so an ordinary read could fail while a writer held the database. They
  now retry within the bounded window, and the retry honours the caller's
  context, so Ctrl-C and MCP cancellation abort a contended read promptly
  instead of waiting out the lock timeout.
- **MCP command execution ignored cancellation.** `command_search` and
  `command_run` blocked on the in-process serialization mutex without watching
  the caller's context, so a cancelled request still queued behind a long
  `sync`. Acquisition is now cancellable and the mutex is never left wedged.
- **`items find` over-matched identifier prefixes.** `--pmid`, `--citekey`, and
  `--arxiv` search the freeform `Extra` field with a trailing wildcard, so
  `smith2023` also matched `smith2023a` and PMID `123` matched `12345`. Since
  these lookups resolve item identity, a false match could send automation at
  the wrong item. Candidates are still found with `LIKE`, then filtered on an
  exact token boundary.
- **`items venues` reported garbage years and a nondeterministic item type.**
  Min/max year came from `SUBSTR(date, 1, 4)` over Zotero's freeform date, so
  `April 2023` yielded `Apri` and `n.d.` sorted as a year; and a bare
  `item_type` under `GROUP BY venue` let SQLite pick an arbitrary row, so the
  reported type changed between runs. Years are now parsed and undatable rows
  excluded; the type is the venue's most common, ties broken deterministically.
- **Silent truncation on import.** A manifest over 64 MiB and a translator page
  over 4 MiB were both silently cut short, so a subset was imported while the
  command reported success. Both now fail loudly and name the limit; the limits
  themselves are unchanged.
- **Errors that masked themselves as absence.** `items file` reported an
  attachment as having no file when the fetch had actually failed;
  `items fulltext` reported no local full text when the store errored; the MCP
  collection manifest reported a missing collection when storage failed; the MCP
  freshness resource reported healthy sync state it had never read; `vault audit`
  treated an unparseable state comment as fresh; and `zotio profile` reported
  zero profiles when the store was corrupt. Each now distinguishes failure from
  absence — the profile case as a one-time stderr warning, because that path is
  reachable from `mcp:read-only` commands.
- **`vault sync` could duplicate a managed note.** A non-`ENOENT` failure
  reading the vault directory produced an empty index, so an existing note was
  written again under a new name instead of being updated. A missing directory
  is still tolerated silently, which is what first-run sync depends on.
- **Two paths could not be cancelled or diagnosed.** `mutation.Run` did not wrap
  the cancellation error, so `errors.Is(err, context.Canceled)` was false for
  callers, and it would have counted an operation as applied had `Apply` returned
  a non-nil error alongside an `applied` status. Partial success — `applied` with
  a reason — is unchanged and now has its own test.
- **A journal run ID could collide.** `NewRunID` fell back to a constant `0000`
  suffix when entropy failed, precisely when uniqueness matters; the fallback now
  incorporates pid and nanosecond time.
- **Smaller hardening.** `client.New` no longer builds a relative cache
  directory when the home directory cannot be resolved, and the MCP server no
  longer opens a CWD-relative database for the same reason; the response cache
  key no longer lets two different query strings collide; `AcquireWriterLock`
  no longer ignores a non-permission `Chmod` failure; and `duplicateResolveVersion`
  no longer carries an unguarded type assertion, though no current caller can
  reach it.

- **Writes no longer send a local-plane version as a Web API precondition.**
  Zotero key-based writes need `If-Unmodified-Since-Version`, and the desktop
  local API and `api.zotero.org` number object versions in independent spaces.
  `items enrich` and `items preprint-check --fix` put a version read from the
  local mirror into the PATCH body, which either conflicts spuriously or, when
  the numbers happen to coincide, guards nothing and overwrites a concurrent Web
  edit. Five further paths — `items tags`, `items move`, `items duplicates
  resolve`, `searches materialize`, and `journal undo`'s tag and collection
  reversal — built their precondition header conditionally and dispatched an
  unguarded PATCH when the version read returned 0, surfacing an opaque 428
  instead of the real cause.
  All of them now resolve the version from the plane the write lands on and fail
  closed with a stated reason if it is unavailable.
- **`journal undo` reads the item it is reversing from the plane it writes to.**
  Both reversals read through the general client, which is only pointed at the
  write plane when route resolution succeeds — and that error was discarded. A
  failed resolution therefore read the local library and PATCHed the remote one.
  For tag and collection reversals this corrupted the write itself, not just the
  guard: the membership being inverted came from the wrong library, so the
  reversal could overwrite upstream tags with stale ones.
- **`tail` no longer skips changes it failed to read.** A malformed but
  successful response decoded to zero events and the cursor advanced anyway, so
  those changes were never observable again; a legitimately empty page still
  advances. Deletions had the same defect on the transport path and are now
  retried rather than skipped — except where the API does not implement
  `/deleted` at all, which the Zotero local API does not, where the feed advances
  and says so once instead of replaying the library on every poll. `tail` also
  wrote events straight to process stdout; it now uses the command's own output
  writer, so callers that redirect output receive them.
- **`searches materialize` walks every page of a saved search.** It issued one
  unpaginated request and reported a complete plan, so everything past the first
  page was silently left unfiled. Keys are also deduplicated within a page, not
  just across pages.
- **`searches run` no longer returns the whole library as a saved search.** When
  the saved-search endpoint was unavailable it fell back to a query with an empty
  search term and an unverified filter parameter, and accepted any non-empty
  response. An API plane that ignores that parameter answers with the entire
  unfiltered library. The unverified fallback is gone; the command now reports
  the result as unavailable.
- **PDF coverage can no longer exceed 100%.** `library stats` and `collections
  stats` counted attachment rows against a denominator of parent items, so one
  item with two PDFs contributed twice. Both now count distinct parents, and the
  item-eligibility predicate is shared between numerator and denominator so they
  cannot drift apart again.
- **`items restore` leaves the local mirror in a readable state.** It removed the
  trash row without reinstating the live one, so an immediately following
  `--data-source local` read, health check, or count reported the item as
  missing entirely. The reinstate and the trash removal are now one transaction,
  a stale cached payload is reported rather than silently resurrected, and a
  failed live-row read no longer discards the trash row on the way out.
- **`library health --verify-files` requires the local API instead of guessing.**
  It probed for the desktop connector but ignored every API error from the probe,
  so a Web API configuration — where the probe returns 404 — proceeded to check
  every attachment through a local-only endpoint and reported them all as broken.
  It now skips with an unmet precondition.
- **Connector item creates are no longer reported as failed after the item was
  created.** Filing the new item into a target collection is a second call with
  no transaction around it, and a failure there discarded the result, so the
  operation was journalled as failed without the key needed to reconcile it and a
  retry could duplicate the item. The commit is now reported with its key, with
  the filing failure named as a follow-up.
- **Connector file imports report what Zotero actually imported.** Every parsed
  record was reported as applied even when the translator merged, rejected, or
  returned fewer items, so automation could not tell a real import from a
  parser-count placeholder.
- **Enrichment no longer attaches a DOI to an unrelated non-Latin title.** The
  exact-title guard compared titles stripped to ASCII letters and digits, so any
  wholly non-Latin title — Chinese, Cyrillic, Arabic — normalized to an empty
  string, and two unrelated titles compared equal. It now compares Unicode
  letters and digits and refuses to match on an empty normalization.
- **PubMed imports stop swapping all-caps surnames into given names.** Any
  all-caps final token up to four characters was read as PubMed's initials
  convention, so `Lee`, `Wong`, `Kim` were inverted. Now limited to the
  documented one- and two-letter shape. `Smith JAB` is read as a surname by
  design: three-letter surnames are far commoner than three-initial authors, and
  a swapped surname corrupts every citation it appears in.
- **`items trash` paginates the union of both trash sources correctly.** Mirror
  rows were appended after pagination and without sorting, so `--start`/`--limit`
  could return rows from the wrong page, out of date order, or duplicated across
  pages. The live side also fetched only one unpaginated page, which the Web API
  answers with 25 results, so a larger trash paginated an incomplete set.
- **`schema drift --deep` no longer reports a clean audit against a shallow
  baseline.** The version fast path compared schema versions without checking
  whether the stored baseline actually contained the per-item-type maps that
  `--deep` compares, so newly added or removed fields went unreported until the
  schema version itself changed.
- **`library health` stops issuing external requests after cancellation.** Its
  retraction checks ran on a background context, so a cancelled or timed-out
  request kept querying CrossRef and delayed process shutdown.
- **Read-only commands no longer write.** `doctor`, `analytics` and `workflow
  status` opened the local mirror through the migrating writable path, taking the
  writer lock and stamping schema during what the capability registry advertises
  as a read; `items bibliography` persisted a resolved user ID from an
  `mcp:read-only` command outside that lock, letting concurrent reads clobber
  each other's unrelated config; and `agent-context` — the command an agent runs
  first — created `~/.zotio` as a side effect four calls deep inside a function
  that computes a path. This last one matters twice over, because `workflow run
  --dry-run` skips `--dry-run` injection precisely for commands annotated
  read-only, so their writes execute for real inside a preview. All 85 read-only
  commands are now audited by call-graph reachability, with legitimate writers
  allowlisted by reason and the list failing if an entry goes stale.
- **`workflow status` and `analytics` work on a fresh install.** Making them
  read-only left them opening a database that does not exist yet, so a first run
  failed with a store-open error instead of reporting that nothing is archived.
- **`workflow archive` takes the installation writer lock.** It writes cursor and
  checkpoint state but was classified as an untyped command and left out of the
  writer-lock set, so a concurrent archive and sync could load the same
  checkpoint, duplicate work, and publish last-write-wins cursor state.
- **The group scope is no longer read without synchronization.** MCP dispatch
  goroutines read the active group while Cobra's pre-run wrote it — a genuine
  data race, and a wrong-library read whenever the two values differed. Putting
  it behind an accessor then exposed a second defect: seven sites called that
  accessor twice around a single check-then-use, so a
  write landing between them produced torn values such as a `data-group-.db`
  path or a bare `group:` scope; compound operations now thread one snapshot
  through, so a deep link and the helpers it calls cannot disagree.
- **Cached responses are written atomically.** The cache truncated the live file
  and then wrote it, so a concurrent reader could observe a partial JSON document
  and surface a decode error as a command failure. Entries are now written to a
  temporary file and renamed.
- **`import scan` no longer leaks decompressors while scanning PDFs.** Readers
  for every compressed stream were allocated up front and the loop returned on
  the first success, leaving the rest unclosed. They are now constructed lazily.
- **`annotations timeline` sorts chronologically.** It compared RFC 3339
  timestamps as strings, so annotations from clients writing different UTC
  offsets were ordered by text rather than by instant. Timestamps are parsed once
  into a sort key, with malformed values kept last.
- **`items summarize --max-chars` counts characters.** It budgeted bytes, so a
  CJK summary was cut to roughly a third of the requested length — verified at 33
  of 100 characters — making the limit depend on the language of the source.
- **`items fulltext` reports its endpoint in command metadata.** It carried the
  annotations every other endpoint-backed command carries except the HTTP method
  and path, so tooling that reads command metadata saw an incomplete contract.

### Documentation

- **The docs landing page leads with the animated wordmark.** The mark that
  opens the README now opens <https://orgmentem.github.io/zotio/> too, in a
  full-width two-column hero rendered by a `home.html` template override. The
  hero columns are sized to the page's own: the mark sits over the navigation
  rail and the headline, blurb, and calls to action start exactly where the
  article text below them does. It stacks once the theme drops that rail.
  Both ink variants are stacked and cross-faded rather than display-toggled,
  so switching the palette does not restart the animation.

## [0.17.0] — 2026-08-08

This release is dominated by a single root cause: zotio reads from the Zotero
desktop local API but routes writes to `api.zotero.org`, and the two planes
number object versions independently. Code that compared or persisted a version
without qualifying it by plane was wrong, which is why writes could not happen,
the mirror could not refresh, and a preview could make the apply that followed
silently do nothing. Found by four consecutive adversarial walk-tests of the
deployed binary against a real 2621-item library.

### Changed — breaking

Every item here changes a JSON shape or a command's behaviour for scripted and
agent consumers. Pre-1.0, so these ship in a minor release with no major-version
signal.

- **`.results` is always a JSON array.** It was an object for `items get`,
  `collections get`, `searches get`, `tags get` and `schema new-item-template`
  (now one-element arrays), and `items find` returned a bare top-level array with
  no wrapper at all (now `{meta, results}`). `jq` written against one read command
  threw on another. **Migration:** `.results[0]` where you previously used
  `.results`.
- **Eight more read commands gained the `{meta, results}` envelope.** `tags audit`,
  `annotations search`, `annotations timeline`, `capabilities`, `groups list`,
  `profile list`, `journal list` and `items collections-of` previously returned
  bare arrays. **Migration:** `.results[]` instead of `.[]`.
  `annotations export` deliberately still returns its raw
  document; `items audit`, `journal show`, `doctor`, `which`, `analytics`,
  `schema drift`, `workflow status`, `reading-list`, `items duplicates` and
  `creators audit` remain report-shaped with purpose-built top-level keys.
- **`items delete` now moves an item to the trash instead of destroying it.** It
  documented "moves to trash" and `items restore` exists to reverse it, but it
  issued a hard `DELETE`: the item and its child PDF were gone, nothing was in the
  trash, and `--allow-destructive` did not gate it. **Migration:** the previous
  behaviour is `--permanent`, which is marked destructive and therefore requires
  `--allow-destructive`.
- **`items delete`, `items restore` and `items update` use the standard mutation
  envelope.** Each emitted a bespoke shape (`{action, resource, path, status}`, or
  `{"status": "noop"}`), so the `.result.items[0]` pattern that works for every
  other mutation threw. **Migration:** read `.result.items[0]`, and
  `.journal.run_id` to undo your own write.
- **`items new` reports the created item's key in `key`.** It reported the item
  *type* there, with the real key buried in `reason.key` — and on the connector
  route it reported nothing at all. **Migration:** none, unless you relied on the
  old placeholder.
- **`journal` is an object with a null `run_id` on no-ops**, not `null`, so one
  extraction path works across a command's outcomes. **Migration:**
  `.journal.run_id` no longer throws on a no-op.
- **`creators audit --json` findings are per alias.** Previously grouped, and 59
  of 73 safe renames carried no runnable command. Ambiguous same-surname
  candidates now carry no command at all and are marked `"unsafe": true` — see
  Fixed, they were proposing merges of different people.
- **Upgrading forces one full resync.** Version cursors now record the plane that
  issued them, and a cursor with no recorded plane is treated as foreign, so the
  first `sync` after upgrading does a complete pass and clears stored per-row
  versions. This is the repair for the frozen mirror described below; it happens
  once.

### Fixed
- **`items delete` no longer sends a redundant PATCH to an item that is
  already trashed.** N4-2's fix removed the false "already deleted" no-op on a
  404, but never added back the correct one — the write plane's own copy of the
  item genuinely carrying `deleted: 1` — so trashing an already-trashed item
  always re-applied, real version churn and journal noise for zero effect,
  same class as W-6's rename-to-itself. It now checks the fetched item body and
  no-ops with `code: already_deleted` before writing. `--permanent` is exempt:
  destroying an already-trashed item is what that flag is for.
- **`items delete --ignore-missing` and `collections delete --ignore-missing`
  return the standard mutation envelope.** N4-2's honest-404 fix broke the
  documented idempotent-retry contract for `--ignore-missing`: on the shipped
  `4bc96ea`, `collections delete <missing> --ignore-missing --allow-destructive
  --yes` regressed from a clean no-op to exit 3, because the flag's only
  handling (`classifyDeleteError`, keyed off the DELETE call's own error) never
  saw the version-read's 404, which now fails first and bypasses it entirely.
  Both commands now check `--ignore-missing` at every 404 point — the version
  read and the write call — and resolve as a genuine `no_op` through the
  standard mutation envelope, not the legacy bespoke
  `{"status":"noop","reason":...}` shape the removed `classifyDeleteError`
  path produced (which for `collections delete` specifically fell through to a
  raw, unpopulated `{"status":0,"success":false}` — a successful no-op that
  read as a failure).
- **`import scan` now explains when it receives a file.** The command previously
  passed an existing regular file to directory reads and surfaced the misleading
  filesystem error "not a directory"; it now directs single-file imports to
  `zotio import pdf <file>` while preserving missing-path and permission errors.
- **`export snapshot` now describes and cleans up its artifacts honestly.** The parent help previously advertised `--format` and `--no-cache` even though the snapshot subcommand rejected them; those legacy parent flags are no longer shown, and snapshot help now documents only its JSONL options. Successful snapshots write the content manifest to `<output>.manifest.json` and remove the transient `<output>.lock`; interrupted runs retain checkpoint state for `--resume`.
- **`library health` now labels its scope with the accepted flag spelling.** The
  human report previously said "preset quick", even though the selector is
  `--for`; it now prints `--for quick` so the displayed invocation can be copied.
- **`import pdf --on-duplicate` now classifies before importing.** The command
  previously ignored `import scan`'s `attach_candidate` verdict and could create
  a second item for a DOI already in the library. Duplicate matches now skip by
  default with a payload naming the DOI and existing item, `create` records the
  match while preserving the import, and `attach` uploads the PDF as a child of
  the existing item.
- **`creators audit` no longer emits unsafe merges.** Ambiguous same-surname
  candidates are hidden unless `--include-ambiguous` is requested, and then are
  labeled review-only without runnable rename commands. Safe exact and
  initials-compatible variants still emit pasteable commands, while canonical
  names now prefer the most complete spelling over frequency.
- **`items trash` shows what you just trashed.** Writes route to
  `api.zotero.org`, but the command read the Zotero desktop local API, which does
  not learn about a trash until Zotero syncs it down — it returned an empty trash
  for an item the web plane already reported as deleted. So immediately after
  `items delete`, `items trash` was blind, and `--data-source local` (normally the
  *less* current source) was the only one that was right. It now unions the two,
  de-duplicated by key, so items trashed in the Zotero UI and items trashed by
  zotio both appear, and says on stderr when the mirror supplied any.
- **An empty list response is no longer cached.** Reads hit the local desktop API
  while writes route to `api.zotero.org`, so for a few seconds after a write a
  filtered query legitimately returns nothing — and caching that pinned the
  emptiness for the full 5-minute TTL, long after the read plane had caught up.
  A `tags rename` previewed during that window then applied nothing and reported
  success, because the apply served the preview's cached empty match set:
  **previewing first, the careful workflow, was what broke the apply.** Selection
  for a write now also bypasses the cache and queries the plane it writes to.
- **The local mirror could not be refreshed at all, in two independent layers.**
  Zotero's local desktop API and `api.zotero.org` number versions independently.
  (1) The sync cursor was a single unqualified number guarded by `MAX()`, so a
  web-API value (12689) outranked every local value (71) forever and `?since=`
  matched nothing — every sync reported success with `total: 0`. (2) Row upserts
  are version-monotonic, so rows holding a web-API version (11973) rejected the
  same item from the local plane (version 65) as "older" and never refreshed —
  which defeated `--full` too. Cursors now record the plane that issued them and
  are ignored across planes; stored row versions are cleared when the plane
  changes or `--full` is requested. On a real 4306-item store frozen since
  2026-07-15 this restored 5343 records and took `tags audit` from 53 phantom
  duplicate groups to 0, matching the live library exactly.
- **`tags rename` silently renamed nothing on freshly written items.** Selection
  required a mirror row to carry a version, but write-through deliberately strips
  the stale version from rows it replays, so any item zotio had just written was
  invisible: `selected: 0`, `ok: true`, exit 0 — indistinguishable from "no such
  tag". The same rows aborted an entire `tags audit fix` batch with "item
  <key> missing version". The write precondition comes from the write plane, so a
  missing mirror version is now irrelevant to selection, and a run that matches
  items but plans none says so on stderr.
- **`items tags list` returned 404 for every item.** `/items/<key>/tags` exists on
  the Web API but not on the local desktop API, where reads are routed;
  `--data-source local` failed differently, parsing the path as resource `items`
  with id `tags`. Tags are now projected from the item payload, which both planes
  carry.
- **Mutation payloads report the journal run id.** Every applied run returned
  `"journal": null` while `journal list` showed the run, so an agent could not
  undo its own write from the write's own response. The envelope now carries
  `journal.run_id` (and `workflow_run_id` when set).
- **`sync --help` names the plane it pulls from.** It said "Sync data from the
  API", which readers reasonably took to mean api.zotero.org and their cloud
  library. It is the Zotero desktop local API, one direction only, and that
  ambiguity is what made the frozen mirror hard to diagnose.
- **`--plain` drops response wrappers.** `library`, `links`, `meta` and
  `relations` rendered as raw JSON objects inside single cells, pushing item rows
  past 2 KB across ~35 columns. An explicit `--select` still wins.
- **`journal undo` can reverse an `items new`.** A create recorded the item type
  where its key belonged, so undo refused with "field source is not reversible".
  Creates now journal the real Zotero key and undo moves the item to the
  reversible trash; a create whose key was never confirmed refuses with a message
  naming the item and the command to trash it.
- **`GetSyncState` tolerates a NULL cursor.** A `sync_state` row created by a
  library-version write before any pass had stored a cursor made every read of it
  fail with "converting NULL to string is unsupported".
- **`sync` no longer rolls back a write Zotero has not synced down yet.** Reads
  resolve against the local mirror and writes land on `api.zotero.org`.
  Write-through already replayed an applied write onto the mirror, but `sync`
  pulls from the *local desktop API* — which only learns of the write when Zotero
  itself syncs down — so the next sync re-applied the pre-write copy and the
  store silently reverted. A successful `items move` really did report `[]` from
  `items collections-of` afterwards. Applied writes are now marked pending and
  re-applied on top of each incoming row, so the read plane's own edits still
  land while the local write survives; the marker clears automatically once the
  read plane reports the written state. `sync` and `doctor` report the
  outstanding count (`pending_writes`).
- **A rename never writes without a precondition.** If the Web API write route
  cannot be resolved, the version lookup now fails loudly instead of quietly
  reading the local plane and sending a PATCH with no
  `If-Unmodified-Since-Version` header — the original failure, reintroduced by a
  transient resolver error.
- **`items collections-of`, `collections stats` and `reading-list` honour output
  flags.** All three bypassed the shared formatter, so `--plain`, `--csv` and (for
  two of them) `--select`/`--compact` were silently dropped and JSON came back
  instead.
- **`--plain` no longer mangles responses that have no tabular form.** A payload
  with several sibling arrays (`items audit` with more than one check) was
  rendered as a single row with a whole JSON array per cell; it now emits JSON
  and says so on stderr. A bare JSON string no longer prints its own quotes, and
  a mixed array no longer drops its scalar entries.
- **`tags rename` and `tags audit fix` can write again.** Every rename was
  planned with the read plane's object version and refused by Zotero with
  "Either If-Unmodified-Since-Version or object version property must be
  provided for key-based writes" — 0 of 54 renames from zotio's own `tags audit`
  merge plan applied, with no flag to work around it. The local API reports an
  empty version (and empty `Last-Modified-Version`) for items it has never
  pushed upstream, and its version space is unrelated to the web API's in any
  case. Apply now re-reads each item from the plane the write goes to, taking
  both the precondition version and the tag list from there, so the PATCH can no
  longer overwrite upstream state with a stale copy either.
- **`doctor` reports the rows the store actually holds.** The cache report
  printed `sync_state.total_count` — the last run's fetched delta — as `rows`, so
  a fully hydrated 4308-item cache displayed as `items: 0 rows` whenever the last
  sync was an empty delta. Rows are now counted from `resources`; the delta is
  reported separately as `last delta`.
- **`items audit` counts only the items it can score.** Attachments, notes and
  annotations were included in every check, so a 928-item library reported 4018
  "missing tags" and 3773 "missing abstracts" — mostly PDFs, for fields those
  types cannot carry. All checks and their listings are now scoped to top-level
  bibliographic items, and the summary states the denominator
  (`Scope: N top-level items`, `top_level_items` in JSON).
- **`library health` and `items audit` now agree on "how many items are in the
  library."** `health`'s scope line counted every row whose indexed
  `item_type` wasn't attachment/note/annotation, while `audit`'s denominator
  also excluded child rows via `parent_key` — two independently-typed
  predicates that could drift, on top of the store's raw mirrored-row count
  (`doctor`) and the live web plane, for a total of three different numbers
  answering "the library." Both commands now share one predicate
  (top-level bibliographic items), and `health`'s scope line names what it
  counted and shows the mirrored-row total alongside it:
  `Scope: library · 928 top-level items (4306 mirrored rows) · source local`.
- **`--plain` emits plain text instead of JSON.** The flag suppressed the human
  table and then fell through to the shared JSON path, so `items recent --plain`,
  `search --plain` and `collections list --plain` all returned JSON. Read
  commands now render tab-separated records, and an explicitly requested
  `--plain`/`--csv` is no longer overridden by the "stdout is piped, so emit
  JSON" default. Plain cells are not truncated the way table cells are.
- **`items move` no-ops say why.** A no-op returned a bare
  `{"status": "no_op", "changes": null}`, indistinguishable from a missing item
  or collection. Results now carry a machine-readable `code`
  (`already_member`, `already_moved`, `not_in_source_collection`) alongside a
  human message.
- **`tags audit` merge plans no longer cement three conventions in one run.**
  Each duplicate group picked its canonical spelling by frequency in
  isolation, so a single plan could resolve `Children`→`children`,
  `Cognitive Psychology`→`Cognitive psychology` and `Developmental
  psychology`→`Developmental Psychology` — three different case conventions,
  none of them library-wide. `tags audit` and `tags audit fix` now take a
  shared `--prefer frequency|sentence|title|lower` flag (default `frequency`,
  preserving today's output exactly) that both commands resolve identically,
  so the plan a user reads is the plan `fix` applies. Any duplicate group
  containing an automatic (type 1) tag — typically a MeSH term a translator
  imported, where Title Case is already correct — is skipped by a
  non-frequency policy and falls back to frequency, flagged in the plan.
- **`tags audit` now tells you `tags audit fix` exists.** The report emitted
  nothing but 54 copy-pasteable `zotio tags rename` lines with no pointer to
  the command that already batch-applies all of them. It now leads with
  `zotio tags audit fix --yes` (carrying `--prefer` when set) as the primary
  path, keeping the individual commands below as a manual escape hatch.
- **`zotio which` no longer misses commands with no curated write-up, and
  finds collection membership by name.** The ranked index only scored the
  ~30 curated hero entries, so a real command like `items move` or `items
  add-to-collection` scored zero for every query no matter how well its own
  name or description matched the words typed — `which "add an item to a
  collection"` surfaced `attachments add` and `schema drift` instead, and
  `collections --help` had nowhere to send you. `which` now indexes every
  command in the Cobra tree (curated entries keep a small tie-breaking
  boost so existing good answers are unchanged), curated intent aliases
  resolve phrasings like "file a paper into a collection" and "put item in
  collection" to `items move`/`items add-to-collection`, and `collections
  --help` now names the commands that add an item to one.
- **`import pdf` no longer creates duplicates it can already detect.** It never
  consulted `import scan`'s own DOI/PDF-presence classifier, so importing a PDF
  whose DOI already had a copy on file minted a second top-level item —
  `library health` then reported the pair it had just created as a duplicate.
  `import pdf` now runs the same classification before touching the connector
  and takes `--on-duplicate skip|attach|create` (default `skip`): `skip` warns
  and reports which existing item the DOI already matched instead of creating
  anything; `attach` uploads the PDF onto that existing item via the same
  stored-attachment protocol as `attachments add`, instead of minting a second
  item; `create` preserves the previous unconditional-create behaviour.
- **`import pdf` gained `--collection <key|name>`.** Every import landed in My
  Library root, forcing a two-step `import pdf` then `items move` — and the
  global `--connector-target` help text already advertised "overrides
  `--collection` target mapping" for a flag that did not exist on this command.
  Zotero's `saveStandaloneAttachment` connector endpoint saves into whatever
  the desktop pane currently targets and accepts no collection parameter, so
  filing now happens as a step after import, reusing `items move`'s membership
  writer (a name is resolved and created when absent, exactly like `items
  add-to-collection`). An item whose key could not be resolved is reported as
  not filed rather than silently dropping the flag.
- **`import pdf --dry-run` no longer plans an import Zotero desktop can't run.**
  With the desktop not running, `--dry-run` returned a full, clean plan; only
  `import pdf` itself (not its preview) checked the connector. The plan phase
  now probes `/connector/ping` through the existing `desktop_connector`
  preconditions-registry check, and the failure names the one alternative that
  needs no desktop: `zotio import doi <DOI>`.
- **`tags audit --prefer title` now capitalizes hyphenated and slash-separated
  words.** The policy previously capitalized only after spaces, so tags such as
  `meta-analysis` became `Meta-analysis`; it now produces `Meta-Analysis`
  and recognizes name-shaped one-letter apostrophe prefixes such as `O'Brien`
  without turning contractions such as `don't` into `Don'T`. Sentence case
  remains first-word-only, so `meta-analysis` stays `Meta-analysis` by design.
- **`schema new-item-template` now works against local Zotero.** The local API
  does not implement Web-only `/items/new`; the command now builds the faithful
  blank template from the local global schema endpoints, without credentials or
  a Web API round trip, and keeps the read envelope's `.results` array contract.
- **`items new` works on its default connector route again.** Every bare create
  failed with `connector saveItems: HTTP 500`; only `--via web` worked. Bisecting
  all 35 template fields against `/connector/saveItems` isolated exactly one:
  Zotero's item saver rejects a creator whose name fields are all empty, which is
  precisely the placeholder `schema new-item-template` emits and `items new` sends
  verbatim (creators omitted → 201, `[]` → 201, both names empty → 500, lastName
  only → 201). Creator entries with no name content are now dropped before the
  save, covering every caller; the template stays faithful to Zotero's own
  `/items/new` shape, since the constraint belongs at the transport.
- **A connector create reports the key it created.** Only the error-recovery path
  resolved the real Zotero key, so a *successful* connector create returned an
  empty key, the journal recorded nothing actionable, and `journal undo` had no
  item to trash. The key is now resolved on success too.
- **`items new` no longer sends an empty connector URI.** Zotero's
  `/connector/saveItems` passes `uri` into its item saver even for metadata-only
  creates, and the schema template has no source page. Connector saves now send a
  valid fallback URI (or the item's DOI/URL). This was originally believed to be
  the cause of the HTTP 500 above; it was not, but the empty URI was wrong on its
  own terms.
- **The agent skill no longer authorizes an agent to apply writes on its own.**
  `SKILL.md` said "pass `--yes` to apply" and never whose decision that is, so
  the only consent token for a merge, an enrichment, a permanent delete, or a
  `vault resolve` direction was one the agent handed itself. `--yes`,
  `--allow-destructive`, a conflict winner, and a raised `--max-changes` now
  wait on a user who has seen the current preview — and the skill states that a
  preview binds nothing, because `--yes` recomputes the change set before
  applying it.
- **The agent skill no longer describes library-wide writes as if they were
  scoped.** `items duplicates resolve` merges every detected pair and accepts no
  key selector, `tags audit fix` applies the entire library-wide plan, and
  `vault push` writes every managed note in the vault; all three read as
  targeted operations. Their real blast radius, and the flags that do and do not
  bound it, are now stated where each is named.
- **The agent skill no longer promises that every applied write is journaled.**
  `vault sync/push/pull/resolve` never enters the mutation envelope, so
  `journal undo` cannot see it, and most of what *is* journaled — field
  overwrites, merges, enrichment, permanent deletes — does not reverse. The
  skill says so now, and points at `export snapshot` as the recovery record to
  take before a bulk write.
- **The agent skill's recipes run again.** `annotations timeline --format
  markdown` was rejected outright (no such flag), and its `--since` was a
  hard-coded date that stopped meaning "the last 7 days" the day after it was
  written. `--select data.DOI,data.title` matched nothing on `items missing-pdf`,
  whose rows are flat, so the documented `jq` printed no DOIs;
  `--select venue,count,year_range` silently dropped the year, because the
  fields are `min_year`/`max_year`; and `collections list --select id,name,status`
  collapsed every row to `{}`. That `--select` accepts unknown field names
  silently, rather than refusing them, is now documented as the trap it is.
- **The agent skill's contract claims match the CLI.** Exit codes 1, 9, 11, 12
  and 13 were missing from its table — including the exit 13 the same file
  explains at length. Webhook delivery failure was described as fatal when it
  only warns; `--agent` output was described as always enveloped, when
  report-shaped commands return a bare object and `items missing-pdf`/`items
  venues` a bare array; "every input is a flag" ignored positional arguments;
  `library health --badge` is refused under the `--agent` the skill mandates
  everywhere; release binaries are unsigned, with Sigstore signing the
  checksums; and the hero list is no longer claimed to be the same index
  `zotio which` resolves against, since the two have diverged by four entries
  each way.

- **`workflow archive` now fetches every advertised resource from its real
  endpoint.** It previously treated internal store names such as `items-trash`
  and `schema-item-fields` as literal paths, causing four 404s; archive now
  maps `/items/trash` and the global schema endpoints outside the library
  prefix, matching the sync path.
- **`analytics` now counts and groups Zotero concepts instead of accepting
  chat-product boilerplate.** `--type items` and other mirrored resource kinds
  retain their census counts, while item types such as `journalArticle` filter
  the items mirror; unknown types and unsupported `--group-by` fields now
  error, and supported year, itemType, collection, creator, and tag groups
  honor `--limit`.
- **`items delete` and `collections delete` no longer report success for
  something they did not delete.** A 404 on the pre-write version read was
  treated as "already deleted", but a trashed item still GETs fine with
  `data.deleted: 1` — Zotero only 404s a key that never existed, was
  permanently destroyed, or (the case that broke this) was created moments ago
  and has not yet propagated from the local desktop up to the write plane
  (~15-20s observed). Reporting success in that window is a false success: an
  item deleted this way materialized live and untrashed. Both commands now fail
  honestly on a 404, exactly like `items tags add` / `items move` already do on
  the identical race. `--ignore-missing`, the separate opt-in idempotency flag,
  is unaffected — 404-as-no-op is still available, just no longer the default.
- **`sync --full` reaps `items-trash`, closing a tombstone class the earlier
  `items` fix left one resource short of complete.** The mark-and-sweep gated
  on the pass having reported at least one key, on the theory that an empty
  result might mean a failed fetch rather than a genuinely empty resource — but
  a request or decode error always returns before that point, so the pass
  actually completing is already the safety guarantee, and requiring a
  non-empty result on top of it left a row with no corresponding item on either
  plane permanently unreapable whenever the live trash was legitimately empty.
  The sweep now runs on every completed full pass, including one with nothing
  to report.
- **`tags list` and `tags audit` no longer disagree on how many tags exist.**
  `tags list` passes Zotero's raw per-`(tag, type)` feed straight through, so a
  name that exists as both an automatic and a manual tag — `Depression` is
  `type 1` on some items and `type 0` on others — produced two rows with an
  identical `tag` value and no visible difference, because `--plain` drops
  `meta` as a structural wrapper and the disambiguating `type` lives there.
  `type` and `num_items` are now promoted to columns for the human/plain
  render path; JSON output, which already had `meta.type`, is unchanged.


### Changed
- **Tests can no longer write to the developer's real zotio data directory.** The
  `internal/cli` suite appended 16 fixture runs per full-suite invocation to
  `~/.local/share/zotio/journal` — keys like `K1`, `ITEM0001` and `Example 0..50`
  interleaved with genuine library history, offered for reversal by
  `journal undo`. The package now runs against a throwaway `HOME`.

### Added
- **`import pdf` returns the keys it created** (`item_key`, `attachment_key`,
  `doi`). Zotero's connector reports only a title and item type, so the keys are
  resolved from the library after recognition. Resolution is anchored to a
  wall-clock floor captured before the import and requires an unambiguous match,
  because Zotero's connector saves into whatever library the desktop pane
  currently targets — which need not be the library zotio reads. An older item
  that merely shares the recognized title is never reported; anything unresolved
  comes back as `keys_note`. Filing an import no longer costs a title search plus
  a `links.up` walk.
- **`items move` and `items add-to-collection` cross-reference each other** in
  `--help`, so finding the key-based bulk command from the name-based one (and
  vice versa) no longer requires reading the whole `items` command list.
- **`creators audit` is no longer a dead end.** It found variant groups
  (`Adam J Rock` vs. `Adam J. Rock`) with nowhere to send them: no `creators
  rename`, no merge plan, not even copy-pasteable commands the way `tags audit`
  emits them. Every group's aliases now print with the exact
  `zotio creators rename --from … --to …` command to fold them into the
  canonical name (also carried as `rename_command` in the JSON groups and
  finding evidence), and `creators rename` applies it: one PATCH per affected
  item, preserving creator order, `creatorType`, and either name shape
  (`firstName`/`lastName` or a single `name` field). Sharing `tags rename`'s
  writer also means the same fix applies here: the precondition and the
  creators array being replaced both come from a fresh write-plane read at
  apply time, not the local mirror captured at plan time, so writes to items
  never pushed upstream (and the freshest state of items that have) no longer
  fail or overwrite concurrent changes. An item whose write-plane copy no
  longer carries the old name reports a structured `no_op` instead of erroring
  or corrupting the item.
- **`SKILL.md` is pinned to the CLI by two drift guards**
  (`cmd/docs-gen/drift_test.go`). The agent skill is the page an agent reads
  *instead of* the generated command reference, and nothing tied it to the
  cobra tree: a manual sweep found `annotations timeline --format` documented
  but never declared, and `duplicates resolve`/`preprint-check fix` written
  without their `items` parent, un-runnable as printed. Every `zotio …`
  invocation and every bare flag mention — including flags discussed in prose
  rather than on a command line — now resolves against the live tree, scoped to
  the commands its section names. Unlike papio's version, a relative command
  path fails rather than being skipped, since being silently ignored is how
  those two survived. Renaming a command or dropping a flag now fails the
  build.

## [0.16.1] — 2026-08-06
### Fixed
- **`zotio-mcp` no longer starts on a malformed `ZOTERO_GROUP`.** It previously
  served the *personal* library when the variable held a non-numeric value, so a
  typo answered every query from the wrong mirror under a group's name. Startup
  now fails closed with exit 2 and the same error `--group` already produces. If
  you set `ZOTERO_GROUP`, confirm it is a numeric Zotero group ID before
  upgrading; unset or numeric values are unaffected. (papio is not affected — it
  strips `ZOTERO_GROUP` before invoking zotio.)
- **Quoted boolean keywords in search are matched literally instead of becoming
  operators.** Searching for the phrase `"AND"`, `"OR"`, or `"NOT"` compiled the
  quoted term into an FTS logical operator — `foo "AND" bar` became
  `"foo" AND "bar"`, silently dropping the term the user explicitly quoted.
  Quoting now means "literal term" for keywords too; unquoted `AND`/`OR`/`NOT`
  are unchanged, including their case-insensitivity.
- **`analytics --group-by` reports real distributions instead of one empty
  bucket.** Grouping read the top level of each stored resource, but synced
  Zotero payloads carry their bibliographic fields under `data`, so
  `--group-by itemType` (and `title`, `date`, …) matched nothing and counted
  every row into a single `<nil>` bucket. Fields now resolve nested-first with a
  top-level fallback, and genuinely missing values report as `(unset)`.
- **MCP group-library reads no longer split-brain between two databases.** With
  `ZOTERO_GROUP` set, `zotero://freshness`, `zotero://health/*`, and the
  collection/item graph resources read the personal `data.db` while `sql`,
  `search`, `zotero://archive/status`, and `zotero://schema` read
  `data-group-<id>.db` — the same server answering the same group library from
  two different mirrors. Demo mode diverged the same way. Both surfaces now
  share one path resolver.
- **A panicking write command no longer strands the installation writer lock.**
  Release ran as a call argument, so a panic unwound past it; under `zotio-mcp`
  the panic is recovered and the server keeps serving, leaving every later
  mutating command failing as "another writer is active" until restart.
- **`items enrich` no longer leaks a connection pool per PDF download.** Each
  download cloned its guarded HTTP transport and never released it, exhausting
  sockets in long-running `zotio-mcp` and `zotio watch` processes.
- **A write command that fails flag validation no longer strands the writer
  lock.** Cobra validates required flags *after* the persistent pre-run and
  *before* `RunE`, so `import --yes items` with no `--input` acquired the
  installation lock and then returned without ever reaching the body that
  releases it. Harmless for a one-shot CLI, but under `zotio-mcp` it wedged
  every later writer. The lock is now taken only once validation would pass.
- Redirect gating for external fetches no longer mutates the process-global
  `http.DefaultClient`, and `zotero://archive/status` no longer leaks a database
  cursor when its context is canceled mid-query.

## [0.16.0] — 2026-08-05
### Changed — breaking
- **Every write now previews by default and applies only under `--yes`.** The
  contract already held for most of the CLI, but eleven commands were exempt and
  wrote the moment you invoked them, previewing only under `--dry-run`:
  `import file`, `import url`, `items new`, `import pmid`, `import arxiv`,
  `import isbn`, the generic `import <resource>`, and `vault push`/`pull`/`sync`/
  `resolve`. All of them now route through the shared mutation gate. Scripts and
  agents that relied on a bare invocation writing MUST add `--yes`. `vault
  resolve` was the sharpest edge: it never honored `--dry-run` at all, and
  because `workflow run` previews a plan by injecting `--dry-run` into each step,
  a *preview* containing a resolve step performed a real, irreversible overwrite
  of the remote note.
- **`--dry-run` no longer suppresses reads.** The HTTP client short-circuited
  every verb, not just mutating ones, returning a fabricated `{"dry_run": true}`
  body with a nil error — and the read helpers discard the status, so
  `zotio items get ABCD1234 --dry-run` exited 0 and printed that sentinel as if
  it were the item. `--dry-run` now means "change nothing", not "talk to nobody":
  GET and HEAD execute and their errors surface, while every mutating verb is
  still suppressed exactly as before. This is what lets a preview describe real
  state instead of a fiction.
- **`import pmid`, `import arxiv`, `import isbn`, and `import <resource>` no
  longer declare their own `--dry-run`.** Each shadowed the global persistent
  flag, which is precisely why the gate could not see it. The flag name and
  preview behavior are unchanged; it now composes correctly with `--yes` and is
  listed under global flags.
- **Previews from the one-item importers return the standard mutation
  envelope.** `import file`, `import url`, `items new`, and `import
  pmid|arxiv|isbn` previously emitted an ad-hoc `{"dry_run", "source", "item"}`
  object. They now emit the same `mode`/`preview_reason`/`plan` envelope every
  other gated command produces, with the proposed item under the plan's changes.
  Parse the envelope, not the old shape.
- **`journal undo` discloses refusals in the envelope, and a fully-refused run
  exits 13.** Refusals were printed as unstructured stderr text and never
  appeared in JSON, so an agent could not distinguish a fully-reversed run from
  one where nothing was reversed — and the all-refused case printed prose and
  returned 0 even under `--json`. Refusals now appear in `warnings` and in a
  structured `journal.refused` array, and an all-refused run renders a real
  envelope with `ok: false` and exit 13. Because none of the newly journaled CRUD
  kinds are losslessly invertible, that is now the common outcome for undoing
  them.
- **`items add-to-collection` previews instead of erroring.** Without `--yes` it
  returned an error telling you to pass `--yes`. It now reports what it would do,
  including whether the named collection would be created or reused.
- **`items restore` sends `If-Unmodified-Since-Version` and fails on a missing
  item.** It was the one key-based write without the precondition, so it could
  clobber a concurrently modified item, and it used the read client rather than
  the write client. A 404 is now a genuine error (exit 3) rather than a silent
  no-op: unlike `items delete`, whose target state is already satisfied by a
  missing item, restore cannot succeed against one that does not exist.
- **Vault writes count against `--max-changes`.** `vault push`/`pull`/`sync`/
  `resolve` never consulted the cap. Each planned note write is now one change,
  so a large vault can be refused where it previously proceeded — the default cap
  is 500, and 50 under `--agent`. Raise it with `--max-changes` for a big first
  push.

### Fixed
- **Single-resource CRUD writes are journaled and replayed into the local
  mirror.** `items create/update/delete/restore` and `collections
  create/update/delete` previewed and honored `--yes` but issued their write
  directly, so `journal list` never saw them and a following `--data-source
  local` read returned pre-write state. They now run through the shared engine.
  A journal entry is an audit record, not a promise of reversibility: `journal
  undo` still refuses anything it cannot invert losslessly, so a delete is
  recorded but not undoable — recover it from Zotero's trash with `items
  restore`.
- **A partially rejected batch is no longer missing from the journal.** `items
  create` and `collections create` wrapped a whole batch in one operation that
  reported failure if Zotero refused any element, and runs with nothing applied
  are not journaled — so a 100-item create where one element was malformed wrote
  99 items and recorded none of them. Each item is now its own operation behind a
  shared batch executor, so the request count is unchanged and the applied/failed
  split is real.
- **`items create` charges each item against `--max-changes`.** A batch counted
  as a single change, so a 10,000-item array sailed past a cap of 50. Both the
  Web API and desktop-connector routes now count per item and refuse before any
  request.
- **The local mirror no longer accepts writes it cannot verify.** Replay wrote
  any unrecognized field with a string value straight into the cached item, so a
  command naming a synthetic or mis-scoped field silently corrupted the local
  copy — invisible until the next `sync`, and served to agents as confirmed
  read-your-writes state. Replay is now restricted to an allowlist of settable
  Zotero fields, rejects identity fields outright, and leaves everything else for
  `sync`.
- **`tags rename` and `tags audit fix` mirror correctly and are undoable.** Both
  recorded a singular `tag` field, which fabricated a bogus key while the real
  tag array kept the stale name, and which `journal undo` refused — despite the
  documented promise that tag renames are reversible. They now record a proper
  tag membership swap, preserving manual/automatic tag type.
- **`items enrich` no longer truncates the mirrored Extra field or mislabels an
  attachment.** It recorded only the new provenance line while sending the full
  appended value, so replay replaced cached Extra — including Better BibTeX
  `Citation Key:` lines — with just that line. Its `--missing-pdf` path also
  attributed the child attachment's URL to the parent item's own `url` field.
- **`items duplicates resolve` records a half-applied merge.** If the merge-target
  write succeeded and the subsequent trash failed, the run reported nothing
  applied and was skipped by the journal, leaving a destructive merge with no
  audit trail. The half-applied state is now journaled and surfaced as a warning.
- **`collections move` is journaled and capped.** It honored `--yes` and its
  version precondition but wrote directly, so it never reached the journal and
  `--max-changes 0` did not stop it.
- **A cancelled multi-operation run stops between operations.** The engine ran
  every remaining operation after a `ctx` cancellation; in-flight requests already
  aborted, but the loop did not. Remaining operations are now reported as
  `not_attempted` with a cancellation warning.
- **`vault pull` fetches each note once and applies exactly the plan it gated.**
  Its apply pass re-read every note independently, doubling remote requests and
  letting a note reclassified mid-run be written without being counted.
- **`items update` no longer double-encodes the item key** in its request path.
- **`--stdin` and `--input -` read the command's input stream, not the process's.**
  Under the stdio MCP server the process stdin is the JSON-RPC transport, so a
  model-issued call that triggered a stdin read consumed the protocol stream and
  hung the session.
- **Opening a fresh database from two processes at once no longer fails with
  `database is locked`.** The migration path retried the schema-version read
  against the WAL-init race but not the connection acquisition immediately
  before it, so the failure simply moved one line earlier. Both now share one
  retry budget.

## [0.15.0] — 2026-08-01
### Changed — breaking
- **A batch create that Zotero partially rejects is no longer reported as
  success.** `items create`, `collections create`, and `import file` send
  batched writes, and Zotero answers a batch with HTTP 200 even when it
  rejected some elements — naming them in a `failed` map in the body. That map
  was discarded, so a run whose items were refused for a missing `itemType`,
  `title`, or `name` printed success and exited 0, and an import silently lost
  records. The failures are now decoded and reported with the source index, the
  per-element status code, and Zotero's message, and the command exits 13
  (degraded). Import indices refer to positions in the source file, not to the
  offset inside whichever batch carried them. Scripts that only checked for a
  zero exit will now see failures they were previously blind to.
- **`import doi` previews by default and requires `--yes` to apply.** It wrote
  on invocation, contradicting the write contract every other mutating command
  honors and the gates the MCP surface advertises for it. It now routes through
  the shared mutation gate: the preview shows the create, `--max-changes` is
  checked *before* the CrossRef request rather than after, and a conditional
  `--fetch-pdf` attachment appears in the plan. Non-interactive callers must add
  `--yes`.
- **`import file --format csljson` without `--via connector` is refused.** The
  direct Web API path posted CSL fields (`author`, `container-title`, `issued`,
  `type`) straight to Zotero, which expects `itemType`/`creators` and its own
  field schema — so records were created incomplete or wrong while the command
  reported success. CSL JSON now requires the connector translator, which maps
  it properly. BibTeX and RIS are unaffected.
- **`collections update` sends `If-Unmodified-Since-Version`.** It was the one
  key-based collection mutation without the precondition its siblings use, so
  two concurrent renames or reparentings both returned success and the later one
  silently discarded the earlier. A stale update now fails instead of
  overwriting; re-read the collection and retry.
- **One zotio writer per installation or output, and busy writers fail fast
  (exit 9).** Atomic file replacement was doing duty as concurrency control,
  which it is not: concurrent invocations could lose profile saves, resurrect
  credentials deleted by a logout, regress a sync cursor, or interleave two
  exports into one output directory. Commands that actually write now take a
  single installation-scoped advisory lock (`~/.zotio/.writer.lock`), which also
  covers an applied `workflow run` and its checkpoints. `export snapshot` and
  `collections bundle` instead lock their canonical output path, so runs writing
  to different directories stay parallel; vault writers hold both the
  installation lock and a canonical vault-path lock. Writers do not queue — a
  second writer in the same scope exits 9 immediately with retry guidance.
  Reads, `--dry-run` previews, and unapplied `--agent` invocations stay
  concurrent, and nested workflow steps inherit their parent's ownership.
  `--config`, `ZOTERO_CONFIG`, and `ZOTIO_DATA_DIR` do **not** create
  independent writer scopes, because profiles and credentials remain shared. See
  [ADR-0005](https://github.com/OrgMentem/zotio/blob/main/dev/adr/0005-single-writer-concurrency-contract.md).
- **MCP results that carry library content are now framed as untrusted data.**
  An item title, abstract, note, or tag is authored by whoever can write to the
  library — a group co-member, a scraped web page, an enrichment provider — and
  it reached the host model in the same channel as its operator's instructions,
  on a surface that also applies writes. Result shapes are preserved: `search`
  keeps its `items`/`count` envelope, `sql` keeps its `rows` object, and library
  resources keep their object shapes and `application/json` MIME type — each now
  carries a top-level `_zotio_provenance` object (`source`/`trust`/`notice`)
  that library content cannot forge, because it is written after decoding.
  Mirrored CLI stdout that is not JSON is wrapped in a per-call nonce-delimited
  block with a data-not-instructions preamble; mirrored JSON keeps its shape.
  Opaque text has unsafe C0/DEL bytes neutralized (tab, LF, and CR survive), and
  JSON results encode control bytes as JSON escapes, so no raw terminal-control
  introducer reaches the transport. Trusted `zotero://context`,
  `zotero://agent-context`, `zotero://status`, `zotero://freshness`,
  `zotero://schema`, and `zotero://capabilities` stay unframed. Library
  resources also adopt the 60-KB result bound they previously bypassed: a
  bundle, manifest, or item list larger than that is now truncated, with
  oversized nested arrays reduced to at most 50 entries and the truncation
  recorded in the payload.
- **`items list --data-source local` no longer claims to handle `--since` and
  `--format`.** The local planner implements a subset of Zotero's item
  parameters and short-circuited the generic path, so `--since` returned older
  items and `--format bib/csljson` returned ordinary JSON rows — both while
  claiming the live item-list contract. Unsupported parameters now fall through
  to the honest generic local dump with a warning, per ADR-0002.
- **`zotio watch <resource>` watches that resource instead of the whole
  library.** The positional was dropped on the way to the inner sync, so a
  scoped `zotio watch items` quietly synced everything on every tick. Scripted
  watchers will now see fewer resources fetched — and fewer written to the local
  mirror — than before.
- **The MCP `sql` tool returns `[]byte` columns as text.** `database/sql` scans
  dynamic columns into `[]byte`, which `encoding/json` rendered as base64, so
  every title, key, and DOI came back unreadable. The normalization is applied
  to every driver `[]byte` value, not only to TEXT columns: BLOB results change
  from lossless base64 to a string that JSON encoding may replace lossily.
  Read binary columns with `hex()` or `base64()` in the query itself.
- **`collections bundle` fails on a full-text read error instead of silently
  writing an incomplete `synthesis.md`.** A locked, busy, or corrupted local
  store made the bundle omit full-text content and still exit 0. The command now
  reports the read error; a run that previously produced a quietly-degraded
  bundle now produces none.
- **`zotio-mcp` exits cleanly on SIGTERM/SIGINT while a client holds the SSE
  stream.** `mcp-go` v0.57.0 closes active sessions during shutdown; before it,
  every signal burned the full 5-second drain timeout and exited 1, so
  supervisors recorded a failed stop. The observed exit status changes from 1 to
  0 — process supervisors treating that 1 as expected must be updated.
- **In-process MCP capture now sees no-op and API-error envelopes.** They were
  written outside the command's own streams, so a mirrored command that made no
  changes, or that failed at the API, returned empty output to the host rather
  than the structured envelope (ADR-0001). Agents that treated empty output as
  "no result" will now receive a parseable envelope.

### Changed
- **`library health` colors its human output.** The headline command and CI gate
  rendered entirely monochrome: the verdict, the `Critical`/`High`/`Info` group
  headers, every `[check_name]` label, the gate result, and the freshness line
  were all the same weight, so nothing was scannable and a finding scrolled away
  from its header was no longer classifiable. Severity now carries color —
  critical red, high yellow, info cyan — on both the group header and each
  finding's own kind label, remediation commands read cyan with dimmed notes, and
  the gate verdict is green/red/yellow for passed/failed/indeterminate. Piped,
  `--agent`, `--no-color`, `NO_COLOR`, and `TERM=dumb` output is byte-identical
  to before.

### Fixed
- Dates like `July 2026` or `15 July 2026` yield a year. The extractor only
  looked at the first four characters, so non-ISO Zotero dates produced an empty
  `year` in vault notes and note templates, breaking indexes and filters.
- Four-segment versions compare correctly (`1.2.3` → `1.2.3.4` is an upgrade);
  everything past the third component used to be ignored.
- A cyclic collection parent chain is detected instead of recursing until the
  process dies, and `stripHTMLTags` is quote-aware, so a `>` inside an attribute
  no longer leaks into rendered text.
- `tags rename` deduplicates its PATCH body.
- `import apply --attach-mode stored` validates the attachment path before
  creating the parent item, so a bad stored-upload path no longer leaves an
  orphaned item behind.
- `items enrich --keys` filters in SQL instead of scanning the whole
  missing-metadata queue and discarding rows in memory.
- Cancellation is honored where it was ignored: `sync --fulltext`, `schema
  drift`, `tail`, and the workflow archive now run under the command context,
  `tail` no longer advances its cursor when a `/deleted` fetch is interrupted,
  and the workflow archive distinguishes an aborted command from an HTTP
  timeout.
- Irreplaceable state is fsynced before it is published — credentials, config,
  profiles, vault conflict artifacts, and health baselines survive a power loss
  rather than only a process crash; hot response/provider caches stay atomic but
  deliberately non-durable, and an expired cache file is now deleted when it is
  read instead of lingering.
- A GET that started before a mutation can no longer repopulate the response
  cache with pre-mutation data after the invalidation, and a torn final line in
  the mutation journal degrades that one entry instead of failing the whole
  listing. Read-only opens wait, boundedly, for a concurrent migration to
  publish its schema instead of erroring on a missing table; the context-aware
  MCP opens can also be canceled during that wait.
- A nil store context no longer panics inside `database/sql`.

### Security
- **`go.mod` now requires Go 1.26.5.** The module declared only `go 1.26`, so a
  source build on any locally installed 1.26.x produced a binary whose
  crypto/tls carried CVE-2026-42505 (ECH privacy leak) — on the exact stack that
  carries the Zotero API key and every metadata-provider call. The go directive
  is now patch-level, so an older 1.26.x is refused and the default
  `GOTOOLCHAIN=auto` fetches the patched toolchain. Shipped release artifacts
  were never affected; they build on the newest patch.
- Library content delivered over MCP is labelled as data, not instruction, and
  unsafe control bytes are neutralized (see the breaking entry above).

### Documentation
- **The Linux install instructions no longer print a `<version>` placeholder.**
  The `.deb`/`.rpm`/`.apk` packages are release assets whose filenames embed the
  version, and the docs asked you to substitute it by hand. The guide now
  resolves the latest tag and your architecture, and both the README and the
  guide state outright that there is no apt/dnf/pacman repository to add and
  that Homebrew is macOS-only (the tap ships a cask, not a formula).
- Added `docs/assets/demos/preview.png`, a static card for external directory
  listings, screenshotted from the `demo-tour` tape by `make demos`.

## [0.14.0] — 2026-07-30
### Changed — breaking
- **The local store now actually runs in WAL mode, and converts existing
  databases on first open.** Both DSNs used mattn/go-sqlite3 shorthand
  (`_journal_mode`, `_busy_timeout`, …) that the pinned `modernc.org/sqlite`
  does not implement and silently ignores, so every pragma had been dropped:
  the store ran in rollback-journal mode with `busy_timeout=0`,
  `synchronous=FULL`, and foreign keys off, while the code and its comments
  assumed the opposite. Reading them back off a live connection is what
  surfaced it. Three consequences worth knowing before upgrading. The first
  *writable* open after this release converts the file to WAL — a read-only
  command leaves a legacy database untouched, so the conversion happens the
  first time you run something that writes. It is persistent but reversible
  with `PRAGMA journal_mode=DELETE`. Effective durability moves from `FULL` to
  the intended `NORMAL`, which can lose the most recent commits on power loss
  or OS crash (not on process crash) — the right trade for a re-syncable cache,
  but a real change from shipped behavior. And **WAL does not work on a network
  filesystem**, because its index relies on shared memory: if you point `--db`
  at an NFS or SMB path, move the database to local storage before upgrading.
  Reverting the file to `DELETE` mode is not a workaround — every writable open
  re-applies the WAL pragma, so the next command undoes it. `zotio doctor` now
  reports live pragma state under `cache.pragmas` and flags a non-WAL journal
  mode or a zero busy timeout.
- **`collections export --limit` changed meaning.** It was effectively a cap on
  how much of a collection you got, because the command made a single request
  and kept whatever came back (default 200, silently clamped to the API's 100).
  It is now a page size, clamped to 100, and the export always walks every page
  — so `--limit 10` used to return at most 10 items and now returns the whole
  collection, 10 items per request. Scripts relying on it as a cap will see
  much larger output and many more requests.
- **`collections create`, `collections update`, and `collections delete` now
  preview by default and require `--yes` to apply**, and `collections delete`
  additionally requires `--allow-destructive`. They previously wrote on
  invocation, honoring none of the write-safety gates — while the MCP surface
  advertised `yes`, `dry-run`, and `allow-destructive` on them, because it
  advertises those on every mutating command. An agent host was shown gates
  that did not exist. `collections create --stdin` now also counts each
  collection in the payload against `--max-changes` (50 under `--agent`), so a
  large batch needs an explicit higher cap. Scripts calling these three
  non-interactively must add the matching flags.
- A workflow step that writes more than 256 MiB to one stream now fails with an
  actionable error instead of growing until the process dies. Step output is
  piped and checkpointed in full, so it cannot be silently trimmed; the
  overflowing step's output is discarded rather than returned or checkpointed.

### Fixed
- `collections export` walks every page instead of returning only the first.
  Any collection over 100 items was silently truncated, and the subcollection
  listing was unpaginated too, so broad collections lost whole subtrees.
  BibTeX citation keys are now deduplicated across pages and subcollections —
  Zotero's translator only dedupes within one response, so concatenated pages
  could repeat a key. A server that ignores `start` is now an error rather than
  a silent partial export.
- `reading-list add`, `start`, and `done` work under `--dry-run`. They read the
  live item to diff its tags, which a dry-run client answers locally, so every
  invocation failed on "item response missing data object". The dry-run plan is
  built from the transition and reports the upper bound of changes, since it
  cannot know which tags the item already carries.
- A stored attachment upload now verifies the bytes it sends against the digest
  Zotero authorized, hashing the stream as it uploads, and fails if the source
  changed after planning rather than registering a file the server believes it
  verified. Stored uploads also no longer hold the file in memory: the digest
  streams and the payload is re-read from disk as it is sent, so peak usage no
  longer scales with attachment size.
- Oversized MCP mirror results become a self-describing preview envelope
  reporting the true output size, instead of an unbounded result.
- The MCP HTTP transport bounds slow request bodies (30s) and idle keep-alives
  (120s). Long-lived Streamable HTTP and SSE responses are unaffected — no
  write deadline is set, deliberately.
- `--deliver` streams to disk rather than buffering the whole result in memory,
  and each run now spools through a unique temporary file, so concurrent
  deliveries to the same target cannot corrupt each other.

### Security
- Credential-shaped strings are redacted before error bodies are truncated.
  A key straddling the 200-byte cap was previously cut down to a prefix the
  redaction pattern no longer matched, and that fragment was printed. Error
  bodies are also truncated on UTF-8 rune boundaries now, so a multi-byte
  character at the cap is no longer split into invalid UTF-8.
- Library content returned through the MCP command mirror is now framed as
  data rather than instructions. Item titles, abstracts, notes, tags, and
  annotations are authored by anyone who can write to the library (a shared
  group, a downloaded PDF's metadata) and reached the host model in the same
  channel as operator instructions, unlabelled. Opaque output — export blobs,
  rendered tables — is wrapped in a nonce-delimited data block with an explicit
  "not instructions" preamble, and C0/DEL control bytes are neutralized on that
  path (JSON results are unaffected: `encoding/json` already escapes them, and
  they keep their exact shape so hosts can still parse them). The framing also
  covers failed and panicking commands, which emit partial library content.
  This is mitigation, not a boundary — the control that holds is the write
  gate above.

## [0.13.1] — 2026-07-27
### Fixed
- In-process MCP mirrored commands and workflow-run steps now restore
  process-global CLI state (output flags, `--group` scope) even when a command
  panics — restoration is deferred instead of running only on the non-panic
  path — and a panicking mirrored command returns an MCP error result carrying
  the captured output instead of crashing the server.

### Security
- Docs CI installs the documentation toolchain from a hash-pinned lock
  (`docs/requirements.lock.txt`, `pip install --require-hashes`), closing the
  one third-party fetch that lacked a content-digest check.
  `docs/requirements.txt` stays the human-edited source; regenerate the lock
  on version bumps with the uv command noted there.

### Removed
- Deleted the retired CLI Printing Press input spec (`spec.yaml`) — the generator was
  retired 2026-07-08 with no regeneration path, and the spec was kept only as
  coverage-reference data. Endpoint coverage now lives in the matrix in
  `dev/zotero-api-coverage.md`; stale reprint references in `AGENTS.md`, the coverage
  doc, and the diagrams skill were cleaned up.

## [0.13.0] — 2026-07-23
### Added
- `items tags add --automatic` writes new tags as Zotero automatic tags
  (type 1); `items tags remove --automatic-only` removes only matching type-1
  tags. Both carry `tag_type` through preview, journal, write-through, and
  undo, so callers can manage namespaced automatic tags without retyping or
  deleting a same-name manual tag.

## [0.12.0] — 2026-07-21
### Changed — breaking
- **More paths now fail loud instead of silently degrading.** Extending the 0.10.0/0.11.0 degraded-exit contract: `export` lockfile builds error on unkeyed/unhashable rows (previously the integrity set silently shrank), `groups inspect` exits non-zero on config-load errors, malformed duplicate-keys JSON is a hard error rather than an empty candidate set, import manifest builds propagate path-resolution failures, and default/demo DB path resolution surfaces `UserHomeDir` errors through every caller. Scripts keying on a `0` exit from these must inspect the error.

### Fixed
- MCP `sql`/`context` handlers surface column-read and marshal errors instead of returning empty or corrupt success payloads; the desktop connector includes body-read errors on non-200 responses; dead store accessors that swallowed scan errors are deleted.

## [0.11.0] — 2026-07-20
### Added
- Local read-parity coverage extended (ADR-0002 scope): `resolveRead` now routes `annotations` search/timeline/export and `items` collections-of/note-template/fulltext against the synced local store, and `--refresh` vs `--data-source local` conflicts are rejected explicitly instead of silently preferring one source.
- The MCP mirror surface (`ZOTIO_MCP_SURFACE=mirror`) now exposes the endpoint commands that were missing from it (`collections_create`, …), bringing the per-command mirror back in line with the CLI command tree (golden regenerated). `workflow run` failed-step checkpoints are resumable over MCP, archive pagination carries real Zotero keys through `start`/`limit`, and capability metadata reports per-command safety.
- Install & packaging: the README install story is now per-OS tabbed (macOS/Linux/Windows/Prebuilt/From-source), documenting the previously undocumented Linux distro packages (`.deb`/`.rpm`/`.apk`) and the Windows Scoop bucket.

### Changed — breaking
- **More commands now exit non-zero on partial failure instead of a silent `0`.** Extending the 0.10.0 degraded-exit contract: `watch` exits non-zero when it stops without a successful sync cycle, `vault push`/`sync` exit degraded on unreadable/partial content, and JSONL `import`/import-apply exit non-zero on per-record failures. Scripts keying on a `0` exit from these must inspect the error/warnings.
- **`items create/update/delete/restore` are now preview-by-default; mutation applies only under the resolved apply mode.** A bare `items create` (and the update/delete/restore paths) renders a dry-run preview rather than applying immediately; scripted/agent callers must pass the apply flag. `items update`/`delete` also require a `version` (or `--dry-run`) up front.
- **`collections create` now accepts an object *or* an array of objects on stdin** (a non-object/array payload is rejected) and threads `parentCollection` (including `parentCollection:false` for top-level). Callers that relied on single-object-only parsing are unaffected; those parsing the response should expect the array-payload contract.
- **`/collections/top` no longer resolves the literal segment `top` as a local collection key** — it now returns an explicit unsupported-local-scope error (ADR-0002).
- **Homebrew distribution migrated from a formula to a cask** (`brews` → `homebrew_casks`). `brew install orgmentem/tap/zotio` still works on macOS and now installs both `zotio` and `zotio-mcp` (with a quarantine-stripping post-install hook), but **casks are macOS-only** — Linuxbrew is no longer a supported install path; Linux users use the `.deb`/`.rpm`/`.apk` packages.

### Fixed
- Local resolver: the get-by-ID fallback now `url.PathUnescape`s the path segment before using it as the store key (percent-escaped tag names previously missed) and wraps malformed-escape errors instead of passing them through.
- Dry-run isolation: `items move`, `tags add`/`remove`, and file `import` (including the `--via connector` route) short-circuit before client creation and version-precondition/desktop-connector fetches, and write clients skip hybrid route resolution (`keys/current`) under `--dry-run` — so a preview never touches the network or resolves a write target.
- Local store: pure reads open the store read-only and migration-free (writable reopens only for ORCID evidence and write-through mirrors); negative `limit`/`start` are rejected; completed-but-empty syncs return `[]` rather than a missing-data error; keyed single-object live reads are write-through cached; and store-open failures now surface contextually in `import scan` and `items summarize`.
- Error propagation across the read/sync paths: sync page-decode and empty-envelope classification, checkpoint-read propagation, `--full` cursor reset, store `ListIDs`/`ResolveByName` scan errors, `search` decode errors, and `items enrich` local-read fan-out failures are no longer swallowed.
- Assorted correctness: CrossRef empty-envelope rejection, trashed PDFs excluded from `import` coverage, `feedback list` surfaces corrupt journal lines, demo seeding propagates sync-state errors, MCP archive status propagates scan failures, `AdaptiveLimiter` releases slots for cancelled waiters, external fetch reuses memoized transports, `export` honors an explicit local data source, and `items summarize` fulltext uses a streaming, parent-scoped join.
- Contracts/retry: the safe-retry predicate now only retries idempotent requests — GET/HEAD, or writes carrying a `Zotero-Write-Token` or an RFC conditional precondition (`If-Unmodified-Since-Version`/`If-Match`/`If-None-Match`) — so a rate-limited (429) or transport-failed non-idempotent write is no longer blindly retried; `export` gains pagination with scope-fingerprinted resume, CSL-JSON output is a single array, `items recent` pages the full candidate set, and linked-url PDF `contentType` is set.
- CI now gates releases on the tagged commit: `go test ./...` runs before GoReleaser, the test matrix adds `macos-latest` and `-race`, a cross-build job compiles/vets windows/darwin/linux, vuln scanning moved to a weekly cron, and local git hooks (`make hooks`) enforce identity/gofmt/vet at commit and merge/am time.

## [0.10.0] — 2026-07-18
### Added
- Opt-in release-update discovery, surfaced in `zotio doctor`. Disabled by default — a nil `[updates]` config section means no checks; `zotio init` offers to enable it, and `doctor` then reports when a newer public release is available with a channel-appropriate upgrade hint (Homebrew vs. source build). The check is one anonymous GET to the public GitHub releases endpoint, cached in the data dir and rate-limited to once a day; it collects no user data, and every network/cache/decoding failure is soft (you get the last cached result or nothing, never a surfaced error).
- Per-command context propagation on the CLI path: the root command runs under the interrupt context and each command's `cmd.Context()` is seeded into the HTTP client (`Client.SetContext`), so per-command deadlines and MCP request cancellation now abort in-flight Zotero/provider requests — previously only process-level Ctrl-C/SIGTERM did.
- Brand: the README header wordmark (`logo-wordmark.svg`, `logo-wordmark-dark.svg`) is now an animated SVG. On a calm ~10s loop the ring draws on, the z snaps into place, the wordmark rises in, and the gold i-dot rolls along the ring rim, wakes up, realises it is off its mark, leaps into the break with a squash landing, blinks, and gives a little left-eye wink before settling. The ring now tracks the wordmark's ink/paper text color per theme (ink on light, paper on dark) while the z keeps a fixed indigo identity (a lighter `#6366F1` in the dark variant, matching the docs' own dark-mode indigo token, since the light-mode hex reads too flat against a dark background) — mirroring sister project *papio*'s ring-tracks-text / letterform-carries-the-brand-hue split. Pure CSS keyframes inside the SVG (no scripts or SMIL, safe for GitHub's `<img>` rendering); the resting state is identical to the prior static logo and `prefers-reduced-motion: reduce` shows it with no animation. The static standalone icon (`logo.svg`) gets the same ink-ring/indigo-z split; `logo-mono.svg` (the flat header-bar mark, always rendered on `mkdocs.yml`'s indigo `primary` background) intentionally keeps its single flat color.

### Changed — breaking
- **Read/report commands now exit non-zero (13, "degraded") with a machine-readable `warnings[]` instead of silently succeeding while dropping errors.** `vault push`, the `doctor` cache report, `import scan`, `items summarize`, `export`, and `collections export` previously omitted unreadable notes/rows/attachments/PDFs (or a truncated write) and still exited `0`; they now surface the failure and exit `13`. Scripts and agents that keyed on a `0` exit from these commands must treat `13` as "completed with warnings" (inspect `warnings[]`) rather than a hard failure.

### Fixed
- Swallowed errors are now surfaced across the read, write, sync, and MCP paths (35 static-audit findings over two passes). Writes fail closed: `items update`/`delete` abort on a failed version read instead of masking it behind a later 428 (delete keeps the 404 idempotent no-op); `workflow archive` stops advancing its cursor and exits non-zero on per-resource failures; fulltext sync and `tail` no longer advance the checkpoint past a fetch/persist/delivery failure; a store upsert fails its transaction on an FTS-index error so a committed row is always searchable; and an applied mutation whose journal write fails reports degraded rather than claiming reversibility. MCP argument serialization is fixed too: array flags serialize as repeated `--flag` pairs (facade, mirror, and `workflow_submit`), explicit `false` bools render `--flag=false`, the shell arg parser handles backslash escapes and single quotes, and the `sql` tool always checks `rows.Err()`.
- Local-mirror writes are now version-monotonic: a strictly-older Zotero version can no longer clobber a newer local row (the FTS rebuild is skipped when the row is retained, keeping the index consistent), and the library-version checkpoint takes `MAX(existing, incoming)` so a slower concurrent `sync`/`tail` cannot regress it. Closes an out-of-order live-read regression, a `tail` cursor regression, and a data-loss vector under concurrent sync; equal/newer and versionless payloads still update, preserving idempotent re-sync.
- Assorted correctness and concurrency-safety fixes: the `QueryItems` FTS join is scoped by `resource_type` so a same-keyed collection/tag/search row can no longer surface or duplicate an item; a `query:` scope now enumerates the full cohort (a negative limit means unlimited) instead of capping at 50; ORCID sidecar upserts route through a write-serialized path; cache and profile writes use a unique temp file plus atomic rename so a concurrent reader never observes truncated JSON; and the MCP orchestration tree builds under the same lock command execution holds, so `command_search` cannot race `command_run` on package-global output flags.
- MCP `capabilities` and `analytics` now return their output through the command writer instead of `os.Stdout`, so in-process MCP execution captures the payload (previously empty) and no longer leaks it to the server process stdout.

## [0.9.0] — 2026-07-15
### Added
- `workflow run` is now transactional: without `--yes` it renders one consolidated preview (mutating steps are forced to `--dry-run`; read-only steps run normally), a single `--yes` on `workflow run` is the one approval for every step (specs that embed their own `--yes`/`--dry-run` are rejected), every step applied under that approval records its journal entry with a shared `workflow_run_id` (`journal list --workflow <id>` filters to one run), and an interrupted apply leaves a `<spec>.checkpoint.json` sidecar so `workflow run --yes --resume` continues where it stopped (spec-hash-verified, succeeded steps skipped, same run id) — re-running without `--resume` while a checkpoint exists is refused.
- `workflow run` specs are now expressive: top-level `vars` with `${vars.NAME}` placeholders in step args (overridable per run with repeatable `--var NAME=value`; undeclared names are refused), inter-step data-flow — a named step's captured output is addressable as `${steps.NAME.output}` in later args and pipeable into a later step's stdin with `"stdin_from"` (so `library health --json` can feed `items enrich --keys-from -` as one workflow) — and per-step conditionals via `"when": {"step": ..., "is": "ok"|"failed"|"skipped"}`. All references are validated at load time (unknown/forward references and malformed placeholders are rejected loudly); the resume checkpoint (schema v2) records resolved variables and completed-step outputs, so `--resume` refuses a changed `--var` set and cross-interruption data-flow keeps working.
- Workflows can now be triggered and agent-submitted: `watch --workflow <spec.json>` runs a workflow after every successful sync cycle and `tail --workflow <spec.json>` runs one only after a poll cycle that emitted change events (quiet when nothing changed) — triggered runs preview unless the invocation carries `--yes`, a trigger failure never stops the loop, and a failed applied run leaves its checkpoint so later applied triggers refuse loudly until resumed; the MCP server gains a dedicated `workflow_submit` tool (both facade and mirror surfaces) that accepts an inline validated step schema — each step names a mirrorable command and is checked against the same per-command safe-flag allowlist as `command_run`, closing the bypass that kept `workflow run` MCP-hidden — then executes through the transactional runner (preview unless `yes`, one journal run id, temp spec and checkpoint always cleaned up; failed applies are re-submittable, not resumable).
- New `library prisma` reports PRISMA 2020 identification-stage counts for a screening corpus from the synced local store: records identified with a per-source-database breakdown (Zotero's libraryCatalog provenance), duplicate records removed (DOI + normalized-title detectors with cross-detector cluster merging so double-flagged pairs count once), and records after deduplication — the input to screening — scoped to a collection or tag via `--scope`, with a `prisma` JSON block that maps one-to-one onto the flow-diagram boxes; screening itself stays out of scope by design (Rayyan/ASReview own it — the wedge is arriving there with a certified, deduped, counted corpus).

### Changed — breaking
- **`workflow run` is now preview-by-default.** In 0.8.0 `zotio workflow run <spec>` executed every step immediately; it now renders one consolidated dry-run preview and applies only with an explicit `--yes` on the `workflow run` command. Specs that embed their own per-step `--yes`/`--dry-run` are rejected at load — the workflow owns approval. Scripts or agents that relied on `workflow run` applying without `--yes`, or on step-level approval flags inside a spec, must pass `--yes` to `workflow run` and drop the step-level flags.

### Fixed
- MCP-applied mutations now record journal entries and write through to the local mirror. Since the `command_run` facade shipped (0.7.0) the `zotio-mcp` server never installed the journal/mirror hooks (only `cli.Execute` did), so writes applied over MCP left no audit trail and could leave the `search`/`sql` tools reading stale local state until the next sync; the server now installs them at startup, covering both `command_run` and `workflow_submit`.

## [0.8.0] — 2026-07-12

### Added
- `items similar <itemKey>` ranks locally similar items with explainable signals — Jaccard overlap on shared collections (0.30), tags (0.25), and creators (0.10), an exact-match venue signal (0.10), plus synced-fulltext rare-word overlap (0.25). Deterministic, offline, no embeddings; every hit carries human-readable "why" reasons, per-signal scores in `--json`, and `--limit`/`--min-score` filters. Complements `items related` (explicit relation edges) with discovered similarity. Requires a synced local store (`zotio sync`; text signal needs `zotio sync --fulltext`).
- `items enrich --missing-pdf` can now download the open-access PDF instead of only linking it: `--attach-mode linked-url|linked-file` (default `linked-url`, unchanged behavior). `linked-file` downloads the Unpaywall-resolved PDF to `--pdf-dir` — content-type check (`application/pdf`, `application/octet-stream`, or absent), `%PDF-` magic-header validation, 100 MiB streaming cap, non-public destination addresses rejected at dial time, never clobbers an existing file — and creates a `linked_file` child attachment. Downloads happen only at apply time; preview names the mode and destination. Stored (imported-file) retro-attachment waits on the deferred Web API upload protocol and is refused with that reason.
- Colored terminal output is now on by default at a TTY (previously gated behind `--human-friendly`): bold card titles, dim labels and timestamps, cyan item types. Kill switches unchanged — `--no-color`, `NO_COLOR`, and `TERM=dumb` always win; piped output still auto-switches to JSON, so agents are unaffected. `--human-friendly` now forces color on for non-TTY output.
- `search` renders human-readable cards/tables at a terminal like the other list commands, instead of dumping raw JSON envelopes.

### Fixed
- `tags list` no longer warns "8/8 tags items skipped (no extractable ID field found)" on every run: the store and sync each had their own resource ID-override map and the store's copy was empty, so `UpsertBatch` could never key tags by name. There is now one shared map, which also means live tag lookups are write-through cached for offline use instead of silently dropped.
- Synced local reads no longer surface stale live copies of items moved to Zotero's trash. Store schema v4 atomically reconciles `items`/`items-trash` by Zotero object version (trash wins ties), migrates existing stores and removes stale FTS rows; selecting `items` for sync now also fetches `items-trash`. `items trash --data-source local` now reads the correct resource, preserves Zotero's `dateModified` ordering before `--start`/`--limit`, and distinguishes synced-empty libraries from unsynced stores.
- Outbound HTTP policy now rejects cross-origin redirects for fixed metadata providers and Zotero Web API requests, refuses every Connector redirect, enforces public-IP checks at the actual dial (including IPv4-mapped IPv6 forms), and prevents injected HTTP clients or redirect callbacks from weakening those invariants. `/keys/current` responses are capped at 1 MiB.
- Container builds now stamp an explicit version and pin both base images by digest; Official MCP Registry publication verifies the pinned publisher's SHA-256 before OIDC execution and runs in a separately recoverable post-release job.
- Human tables and cards now align on display width: ANSI style codes no longer skew tabwriter padding, East Asian wide runes count as two columns, and the card label column was off by one for the longest label. Cell truncation is rune-safe (no more mojibake on long non-ASCII titles).
- Nested Zotero objects in card output render domain summaries instead of raw JSON: tags show the tag name, creators show "First Last" (annotated with non-author roles); the previous generic summarizer targeted shop-order shapes (`qty`/`price`/`Side1`) that never occur in Zotero payloads.
- Card and table field order is deterministic: fields sort alphabetically within priority tiers instead of following map iteration order, which shuffled output between runs.

## [0.7.0] — 2026-07-10

### Added
- `zotio-mcp` now reports its build version via `--version`, the MCP server's version field, and the startup banner (previously unversioned); the release workflow fails if either binary stops reporting the tag.

### Fixed
- Better BibTeX citekeys are now also read from the `citationKey` data field the BBT plugin exposes via the local API — previously only pinned `Citation Key:` Extra lines were recognized, so libraries with dynamic (unpinned) keys got a false `better_bibtex` precondition refusal from `items bibcheck` and empty results from `items citekey-conflicts`; `items find --citekey` matches the field too.
- `import discover` no longer aborts the whole chase when one source item's provider fetch fails (for example OpenCitations returning an oversized response for a heavily-cited paper): the failure is recorded per source in the summary and the remaining sources proceed; the run only errors when every source fails. OpenAlex forward pagination now requests only `id,doi`, keeping pages of heavily-cited works under the response cap.

### Changed — breaking
- **Removed the 28 typed spec-derived MCP endpoint tools** (`collections_*`, `items_get/list`, `schema_*`, `tags_*`, …). They were frozen at generator retirement, bypassed the CLI's mutation gates and fixes (e.g. the `schema` library-prefix 404), and already rejected writes. The CLI command tree is now the single MCP source of truth: the `zotio-mcp` server exposes framework tools (`context`/`search`/`sql`) plus the `command_search`/`command_run` facade by default, or the per-command mirror via `ZOTIO_MCP_SURFACE=mirror`. **Hosts pinned to the old typed tool names must switch to `command_run`** (facade) or the mirror surface. See `notes/adr/0003-retire-typed-mcp-endpoint-tools.md`.

## [0.6.0] — 2026-07-10

### Added
- `items related <itemKey>` lists an item's relation edges from the synced store — outgoing and incoming, predicate-tagged (`dc:relation`, `owl:sameAs`, …), preserving cross-library and off-store targets as external edges; also exposed as the MCP resource `zotero://items/{key}/related`.
- `creators audit` inventories creator-name variants in three confidence tiers (exact-after-normalization, compatible initials, ambiguous surnames) with canonical candidates and the shared findings envelope; `--orcid` corroborates variants against Crossref author ORCIDs, persisted in a local-only sidecar (never written to Zotero).
- `creators audit fix` renames creator variants preview-first: exact-normalization variants are auto-planned, initial-vs-full variants only via explicit `--map "J. Smith=John Smith"`, ambiguous variants never; applies as full-creators-array PATCHes with version preconditions (journaled; not undoable).
- `import discover --scope <expr>` chases citations backward (`--direction backward`, default), forward, or `both` via OpenCitations/Semantic Scholar/Crossref/OpenAlex, dedupes against the library (DOI and normalized title) before emitting, and writes ranked, provenance-tagged entries (`discovery.direction/provider/count/cited_by_keys`) into a reviewable import manifest for `import apply`.
- Import manifests are now schema v2 (optional per-entry `discovery` provenance); v1 manifests remain readable.
- External metadata-provider requests (OpenCitations, Semantic Scholar, Crossref, OpenAlex) used by `import discover` and `collections gaps` are cached for 7 days under the user cache dir; `--no-cache` bypasses.

### Fixed
- `items enrich --yes` no longer replaces the item's entire Extra field with the provenance line — existing Extra content (Better BibTeX `Citation Key:` lines, user notes) is preserved and the provenance line is appended; the mutation preview now shows the Extra change, and same-day re-runs do not duplicate the line.

## [0.5.0] — 2026-07-09

### Added
- `items bibliography` renders a scope-wide formatted bibliography in any CSL style, server-side via the Web API (`--scope`, `--style`, chunked at 50 keys per request).
- `items bibcheck` accepts multiple manuscripts, flags cited items missing citation-core fields (`incomplete_citation` findings with file:line evidence), emits the canonical findings envelope in JSON, and gains `--fail-on <high|any|none>` (exit `11`) alongside the existing `--fail-on-unknown`.
- `export snapshot verify <lockfile>` classifies drift against the current library as added/removed/changed/touched by comparing the recorded content SHA-256 — version-only churn is `touched`, never drift; `--fail-on-drift` exits `11`.
- Diagnostics (`library health`, `items audit`, `vault audit`, `items citekey-conflicts`, `items duplicates`, `items enrich --validate`, `items preprint-check`) emit a shared `findings` array (kind/severity/item_key envelope), and `--keys-from` ingests it: `zotio library health --json | zotio items enrich --missing-doi --keys-from -` now composes directly.
- **Package distribution expanded** — tagged releases now publish a Scoop manifest to `OrgMentem/scoop-bucket` (`scoop bucket add zotio https://github.com/OrgMentem/scoop-bucket && scoop install zotio`), open a WinGet manifest PR (`winget install OrgMentem.zotio`), and attach Linux `.deb`/`.rpm`/`.apk` packages; Homebrew (`brew install orgmentem/tap/zotio`) covers macOS and Linux.

### Changed
- `library wrapped --card` gains `--card-style overview|rhythm|picks|cycle`: three share-card layouts (overview: hero + type mix + highlights; rhythm: streak/busiest-day/weekday stat blocks with a large labeled month chart; picks: deep cut, most annotated, top tag, ranked venues/authors) plus `cycle`, a single SVG that crossfades through all three with CSS keyframes (works in GitHub READMEs, honors prefers-reduced-motion). The README embeds the cycling card instead of a terminal GIF.
- The demo library fixture gains a 4-day addition streak and a 2-item busiest day in June 2026 so sandbox wrapped output exercises every highlight.
- The wrapped SVG share card is redesigned to match the terminal overhaul: gradient background with accent strip, hero counter with annotation/streak chips, a full-width type-mix ratio bar with legend, a Highlights list (deep cut, most annotated, busiest day, top tag), peak-highlighted month chart, severity-colored PDF-coverage meter, and a "computed locally" footer. Sections with no data are omitted.
- `which` renders through the styled table path (bold headers, aligned display-width columns, clipped cells) instead of raw `%-24s` formatting; the README tour now closes on `which 'undo a bad edit'` so retraction checking isn't shown twice across the demo media.
- `library wrapped` redesigned: hero counters, monthly bars with a highlighted peak, a stacked type-mix ratio bar with color legend, a Highlights block (busiest day, favorite weekday, longest streak, deep cut, hot-off-the-press count, most-annotated item, top tag), full first-author names ("LeCun, Yann"), severity-colored PDF-coverage bar, and a share-card hint. The SVG card gains a streak/busiest-day footer. JSON output is additive (`highlights` object); existing fields unchanged.
- `items audit` and `tags audit` summaries render through the styled table path instead of raw tabwriter/markdown headings; table headers show `DATE ADDED` instead of `DATE_ADDED`; provenance lines are dim and pluralize correctly ("1 result").
- The write-safety diagram's flow arrows are consistent and orthogonal (no more bezier that appeared to route REFUSE into APPLY); gate annotations no longer clip.
- `library stats` renders proportional bar charts with aligned counts instead of bare tabwriter columns.
- `printTable` commands (`retract-check`, `bibcheck`, `groups`, `which`, importer listings) render through the width-aware styler: bold headers, dim keys/dates, severity-colored STATUS cells (red retracted, yellow correction, green ok), and cells clipped to 48 columns so rows stay terminal-sized (JSON output keeps full values).
- Demo GIFs re-recorded against the styled output; the docs/README tour now walks search, duplicate detection, stats, and goal resolution.
- `--deliver` now delivers rendered reports when a quality or freshness gate fails (exit `11`/`12`) — previously the report was dropped exactly when a CI consumer needed it. Usage and config errors still skip delivery, and a delivery failure never masks the command's exit code.
- Export snapshot lockfiles record each item's title and normalized content SHA-256.

### Changed — breaking
zotio is pre-1.0, so these ship in a minor release without a major-version signal. Scripted and agent consumers should review before upgrading:
- **Precondition enforcement replaces silent empty-success.** A command whose declared `requires` (synced store, live local API, Better BibTeX, desktop connector) is unmet now refuses with a structured `precondition_unmet` envelope and **exit `9`** instead of returning empty results with exit `0`. Scripts that treated an empty result as success will now observe a non-zero exit. MCP `command_search`/`command_run` command detail now carries `operation`/`requires`/`destructive`.
- **Diagnostic JSON shapes replaced by the canonical findings envelope** for `items citekey-conflicts`, `vault audit`, `items enrich --validate`, and `items preprint-check`. Consumers parsing the old per-command shapes must switch to the shared `findings` array. (`items audit` and `items duplicates` keep their existing fields and add `findings` alongside — non-breaking.)
- **`items cite --style <csl-id>` renders named CSL styles through the Web API** instead of silently falling back to Zotero's default style, and **refuses with exit `9` without an API key**. Scripts relying on the silent default-style fallback now fail loudly rather than emitting wrong-style output.

## [0.4.0] — 2026-07-09

### Security
- Redirect handling now strips the Zotero API key/Authorization header on any scheme-or-host change (previously an https→http downgrade on the same host kept the credential).
- Webhook delivery (`workflow run`, `watch --health`) and feedback submission now validate and dial the destination IP together, closing a DNS-rebinding SSRF gap where the resolved address could change between validation and the request.
- The local SQLite mirror and the mutation journal are now created with private permissions (`0700` directories, `0600` files, with a defensive `chmod` on pre-existing paths), matching the existing API response cache.
- `zotio-mcp --mcp-auth-token <value>` now refuses a literal token (visible in `ps`/shell history) with guidance; use the new `--mcp-auth-token-file` or `ZOTIO_MCP_TOKEN` instead.
- `zotio doctor` redacts userinfo passwords and token-like query parameters (`token`, `key`, `api_key`, `secret`, `password`, `auth`) from the reported base URL.
- OpenAlex abstract reconstruction (`items enrich`) rejects out-of-range word positions instead of sizing an allocation directly from provider-controlled input.
- Terminal table/card output strips C0 control bytes and DEL from synced item metadata, closing an ANSI/OSC terminal-escape injection path.
- The MCP `sql` tool now runs under a 15s deadline with a 5000-row cap (`{rows, truncated, row_limit}` response envelope); `sql` and `search` results are now clamped through the same response-budget limit as typed MCP tools.

### Fixed
- The API response cache key now includes request headers, so header-varying `GetWithHeaders` calls no longer collide.
- `collections move` now requires `--yes` to apply (preview makes no HTTP call) and sends a version-checked write, matching `items move`.
- `tags audit fix --max-changes` now counts actual per-item writes instead of tag aliases, so a popular alias can no longer slip thousands of writes past a small approved cap.
- The `sync` worker pool now checks for cancellation immediately after dequeuing a resource, so a canceled sync can no longer start another long resource pass.
- `sync` NDJSON events are now built with `encoding/json` instead of hand-escaped strings; control characters or backslashes in error messages no longer corrupt the event stream.
- `vault` sync now recognizes CRLF frontmatter delimiters, fixing key extraction and duplicate notes on Windows-synced vaults.
- `tail`'s file sink now creates missing parent directories instead of silently dropping events.
- The connector client no longer disables the shared HTTP client's timeout as a side effect of a single recognition request; its 2xx response reads are now capped instead of unbounded.

### Changed
- MCP server environment variables were renamed: `PP_MCP_SURFACE` → `ZOTIO_MCP_SURFACE` and `PP_MCP_TRANSPORT` → `ZOTIO_MCP_TRANSPORT`. The old `PP_*` names are no longer recognized.
- Cobra command annotation keys surfaced through `agent-context` were renamed from the `pp:` prefix to `zotio:` (e.g. `zotio:endpoint`, `zotio:method`, `zotio:path`, `zotio:destructive`).

### Removed
- CLI Printing Press generator scaffolding was retired: generation provenance (`.printing-press.json`), the patch catalog (`.printing-press-patches.json`, with history in git), vendored press dev skills, and the `Generated by CLI Printing Press ... DO NOT EDIT` headers on 97 hand-maintained Go files. The project is fully hand-maintained.
- Source comments, filenames, notes, and CI configs no longer reference the maintainer's local review tooling: patch markers and tracking IDs were scrubbed, and the README bootstrap attribution sentence was removed.

## [0.3.0] — 2026-07-08

### Added
- `library health --baseline <path>` compares the current findings with a saved baseline; a missing file is treated as an establishing run with zero new findings, and baseline-mode human output reports `New since baseline: N (resolved M)` or `Baseline established (N findings recorded)`.
- `library health --write-baseline <path>` atomically writes schema-versioned baseline JSON with an RFC3339 `generated_at`, the selected preset, and sorted finding identities shared with `watch --health`.
- `library health --fail-on-new <critical|high|info|any>` gates only findings that are new since `--baseline`; it is a usage error without `--baseline` and exits `11` when a new finding meets the threshold.
- `library health --report <path>` writes the full JSON health report sidecar in both human and badge modes, while the existing `--badge --json` conflict remains unchanged.
- `library health --fail-on none` disables the absolute findings gate, overriding the preset default so delta-only CI can combine `--baseline`, `--write-baseline`, and `--fail-on-new`.

## [0.2.0] — 2026-07-07

### Added
- **`zotio demo` — zero-setup trial sandbox.** Seeds a bundled sample library (34 classic papers — including one genuinely retracted — with duplicates, citekey conflicts, tag drift, annotations, and a reading queue) into a separate `demo.db`; `ZOTIO_DEMO=1` reroutes any command to the sandbox with a pristine, key-less config that never touches the real store, config file, or credentials.
- **Recorded demos** — VHS tapes (`docs/tapes/`, `make demos`) render deterministic GIFs of the hero features against the demo sandbox; embedded in the README and docs site.

### Changed
- `reading-list` now supports `--data-source local` read parity (works offline from the synced store — and in the demo sandbox).

## [0.1.2] — 2026-07-07

### Added
- **MCPB bundles for Claude Desktop** — every release now ships per-platform `zotio-mcp_<version>_<os>_<arch>.mcpb` bundles (manifest + binary, one-click install).
- **CI guide** on the docs site — [CI for your bibliography](https://orgmentem.github.io/zotio/guide/ci/): the GitHub Action, manuscript gating, badge publishing, exit codes.
- Grouped, conventional-commit release notes (goreleaser changelog) and this curated CHANGELOG.

### Changed
- Install documentation now leads with the first-party channels: `brew install orgmentem/tap/zotio`, signed release binaries, and build-from-source — replacing broken external installer links.
- MCPB manifest refreshed: MIT license, OrgMentem authorship, release-pinned version, brand-consistent display name.
- Zotero trademark disclaimer added to the README, docs footer, and companion action.

## [0.1.1] — 2026-07-07

### Added
- Automatic Homebrew tap publishing on tagged releases (scoped `HOMEBREW_TAP_GITHUB_TOKEN`; formula lands in `Formula/`).
- Live bibliography badge on the README — the docs deploy syncs the maintainer's real library and publishes shields.io endpoint JSON (weekly refresh).

### Fixed
- Honest Homebrew formula description (removed print-time overclaim).
- goimports grouping and test-file permissions flagged by CI.

## [0.1.0] — 2026-07-07

First tagged release: the trust-and-automation layer for Zotero.

### Added
- **Library trust** — `library health` (ranked, CI-gateable report with `--for` presets, `--fail-on` exit-code gate, shields.io `--badge`), `items retract-check` (Crossref Retraction Watch data; opt-in health gate via `--check-retractions`), `items duplicates` + `resolve`, `tags audit` + `fix`, `items audit`, `schema drift`, `collections gaps` (citation-graph gap analysis via OpenCitations/Semantic Scholar).
- **Safe writes** — one preview-first mutation engine behind every write (`--dry-run`/`--yes`, version-guarded PATCHes), reviewable import (`import scan → resolve → apply`, plus `import doi|pmid|arxiv|isbn`), `items enrich` (CrossRef/OpenAlex/Semantic Scholar/Unpaywall, `--validate`), `items preprint-check` + `fix`, append-only `journal` with `journal undo`.
- **Manuscript side** — `items bibcheck <manuscript>` resolves `\cite{}`/pandoc `@citekeys` against the library (`--fail-on-unknown`), `items citekey-conflicts`.
- **Agent surface** — `zotio-mcp` MCP server, machine-readable trust plane (`agent-context`, `capabilities`, freshness), `items summarize` bounded context bundles, `zotio which` goal-to-command resolution.
- **Sync & automation** — local SQLite mirror (`sync`, `watch`, `--health` drift notifications with webhook delivery), reproducible `export snapshot` with content-hash lockfile, `workflow run`.
- **Reading & PKM** — two-way Obsidian/Logseq `vault` sync with conflict-safe write-back, `annotations export`/`timeline`, `reading-list`, `items note-template`, `library wrapped` year-in-review with shareable SVG card.
- **Onboarding** — `zotio init` guided setup (Zotero detection, local API, key, first sync, health check).
- Release engineering: goreleaser builds for 6 platforms, cosign-signed checksums, SBOMs, Homebrew tap.

[Unreleased]: https://github.com/OrgMentem/zotio/compare/v0.21.0...HEAD
[0.21.0]: https://github.com/OrgMentem/zotio/compare/v0.20.1...v0.21.0
[0.20.1]: https://github.com/OrgMentem/zotio/compare/v0.20.0...v0.20.1
[0.20.0]: https://github.com/OrgMentem/zotio/compare/v0.19.0...v0.20.0
[0.19.0]: https://github.com/OrgMentem/zotio/compare/v0.18.0...v0.19.0
[0.18.0]: https://github.com/OrgMentem/zotio/compare/v0.17.0...v0.18.0
[0.17.0]: https://github.com/OrgMentem/zotio/compare/v0.16.1...v0.17.0
[0.16.1]: https://github.com/OrgMentem/zotio/compare/v0.16.0...v0.16.1
[0.16.0]: https://github.com/OrgMentem/zotio/compare/v0.15.0...v0.16.0
[0.15.0]: https://github.com/OrgMentem/zotio/compare/v0.14.0...v0.15.0
[0.14.0]: https://github.com/OrgMentem/zotio/compare/v0.13.1...v0.14.0
[0.13.1]: https://github.com/OrgMentem/zotio/compare/v0.13.0...v0.13.1
[0.13.0]: https://github.com/OrgMentem/zotio/compare/v0.12.0...v0.13.0
[0.12.0]: https://github.com/OrgMentem/zotio/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/OrgMentem/zotio/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/OrgMentem/zotio/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/OrgMentem/zotio/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/OrgMentem/zotio/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/OrgMentem/zotio/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/OrgMentem/zotio/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/OrgMentem/zotio/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/OrgMentem/zotio/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/OrgMentem/zotio/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/OrgMentem/zotio/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/OrgMentem/zotio/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/OrgMentem/zotio/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/OrgMentem/zotio/releases/tag/v0.1.0
