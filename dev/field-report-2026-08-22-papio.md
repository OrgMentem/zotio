# zotio field report — 2026-08-22 — papio

Source: papio (`/Users/ellis/@dev/papio`), downstream consumer that drives `zotio` as a subprocess. Measured against zotio 0.19.0 on 2026-08-22, macOS 25.6.0, Zotero desktop running. Every number in Finding 3a is the operator's observation in that harness; every code claim elsewhere is cited to a file and line range in this repo or in `zotero/zotero` current main. Nothing here contacts a live Zotero — the operator's desktop is wedged and must not be probed.

How this report labels claims: **VERIFIED** means the cited file and line range was read in this repo or in `zotero/zotero` main. **INFERRED** means a plausible reading of the code with no citation, and it is tagged `[INFERENCE]`. **NOT ESTABLISHED** means the search found no source path — it is stated plainly so the next investigator does not repeat it.

Related register: `internal/cli/results_array_test.go:12-14` references this report as the third field-report origin for the read-envelope invariant (after `dev/field-report-2026-08-08.md` finding 10 and `dev/field-report-2026-08-08-library-hygiene.md` finding 8). That test, and the two prior reports, define the convention this file follows.

---

## Finding 1 — Read-envelope rollout left four local-mirror commands behind — RESOLVED

**Status: RESOLVED in the current changeset.**

### Symptom

`items missing-pdf` answered a bare top-level JSON array while `items find` and `items list` answered the `{meta, results}` envelope (`internal/cli/results_array_test.go:4-14` names the invariant: `.results` is always a JSON array for resource-record reads). A consumer that reads `.results[]` silently got nothing from the bare-array commands.

Reproduced at the command level, then measured against the installed 0.19.0 binary on a seeded scratch store: `items missing-pdf`, `items stale`, `items unfiled`, and `items venues` all answered `array` while `items find` answered `object`. Three of those four — `items stale`, `items unfiled`, `items venues` — were NOT in the original report. The report asked whether other read commands were in the same position and the answer was yes (`internal/cli/results_array_test.go:185-193`).

### Fix

All four now route through `wrapWithProvenance` like `internal/cli/items_find.go:87-101`, which builds provenance with `localProvenance(rawDB, "items", "local_only")`, respects `selectFields`/`compact`, and prints the `{meta, results}` envelope. Before the fix the four local-mirror record reads printed the raw row array directly; after the fix they share the same envelope pipeline as `items get`, `items list`, `items find`, and `analytics` (`internal/cli/results_array_test.go:193-258` exercises all four from one seeded store — a single journalArticle with no PDF child, no collections, an old `dateAdded`, and a `publicationTitle` — so adding another local record read is one table row).

### Documentation note — VERIFIED

`docs/reference/commands.md` had promised the envelope for all ordinary reads and never listed these four as exceptions, so the documentation was correct and the code was wrong. `docs-drift` could not catch it because it only proves the generated file matches the generator, never that the generator's prose is true — that is a scope limit of the drift check, not a missed assertion.

---

## Finding 2 — No local route to attach a file to an existing item — NOT A DEFECT

**Status: NOT A DEFECT. Documented limitation; one feature proposal remains undecided.**

### What the caller asked for

Attach a file to an item that already exists in the library, via the local route (`--via connector`).

### VERIFIED: the refusal, the remediation, and the connector limit

* The refusal already states the limitation at `internal/cli/file_storage_guard.go:393` — `return "Zotero's connector cannot attach a file to an item that already exists in the library, so this upload has no local route"` — and the remediation at `internal/cli/file_storage_guard.go:544` already names the manual desktop route (`file_storage_guard.go:544`).
* The connector limitation was established empirically against Zotero 7 on 2026-08-17 and is recorded at `internal/connector/connector.go:173-176` and `internal/cli/file_storage_guard.go:10-16` (`connector.go:173-176` describes the Zotero-side `saveItems`/`saveAttachment` boundary; `file_storage_guard.go:10-16` records the guard's reading of that limit).
* No code path was found that would attach through the connector to an existing item without creating a new parent — the absence is the defect's absence.

### The one route that could close the gap — VERIFIED, with cost

Upload bytes to the operator's WebDAV server directly, after creating the attachment item through the Web API. `internal/zoteroprefs/zoteroprefs.go:119-121` records that Zotero keeps the WebDAV password in the OS keychain and not in `prefs.js`, so credentials must be supplied separately — zotio cannot read them from prefs alone (`zoteroprefs.go:119-121`).

That route is not free: it needs a WebDAV credential input, a bytes-upload implementation distinct from the connector's `saveAttachment` flow, and a decision about whether papio-class callers should be allowed to supply a keychain password to a subprocess. No design choice has been made. Mark this as a **feature proposal needing a decision**, not a defect. Do not treat the absence of a local attach route as a bug to be fixed in the connector.

---

## Finding 3 — Connector session-per-entry and no pacing; stacked Zotero `Progress` windows — OPEN

**Status: OPEN. Three zotio-side defects are proven regardless of the unresolved window mechanism; the mechanism itself is NOT ESTABLISHED.**

This finding is split into three distinct parts so the next reader can trust the evidence without inheriting an unproven theory.

### a. Symptom — operator observation (NOT a code claim)

On the operator's machine, papio invoked `zotio import apply --via connector` roughly 78 times in a tight loop. Zotero desktop is now WEDGED — unresponsive, 0.1 percent CPU — with 44 or more stacked windows titled `Progress` (operator count: "44 or more", not an exact window count). These numbers are the operator's measurement on 2026-08-22; they are not derived from the code.

Two facts about the symptom are unexplained by the code alone (see part c): the 78 invocations produced 44 or more windows, not 78, and the ratio has no mechanism in the current evidence.

### b. What is VERIFIED — in zotio and in Zotero

#### In zotio — session allocation, pacing, and documentation

* `import apply --via connector` allocates a new connector session per manifest entry. `internal/cli/create_route.go:208-216` — `routeCreateItemVia` allocates `connector.NewID` (16 random bytes) **inside** each item create (`create_route.go:208-216`). One invocation therefore creates one session per entry, not one per invocation.
* The two sibling paths already share one session per command. `internal/cli/import_file.go:213-232` — `importFileConnectorSession` allocates ONE session in `run`, guarded by a `done` flag, and shares it across every record in the file (`import_file.go:213-232`). `internal/cli/items_create.go:115-133` likewise uses one session for all items in the command (`items_create.go:115-133`). `import apply` is the outlier; the shared-session pattern already exists in this codebase twice.
* `internal/cli/import_pdf.go:224-250` — one session per applied PDF operation (`import_pdf.go:224-250`). This is not the papio path, but it shows the same per-operation pattern in a third place.
* The connector client has no pacing. `internal/connector/connector.go:33-46` builds its own `http.Client` with no limiter, retry, backoff, or pacing (`connector.go:33-46`). The global `--rate-limit` flag at `internal/cli/root.go:276-277` is passed only to `internal/client.New` at `root.go:454-455` and `root.go:555-556`, the Web API client (`root.go:276-277`, `root.go:454-455`, `root.go:555-556`). It therefore does NOT govern connector calls. A user who sets `--rate-limit` expecting it to bound connector traffic is silently wrong — VERIFIED by the wiring, not by a live probe.
* The connector surface has no session close. `internal/connector/connector.go` exposes twelve endpoint methods — `Ping`, `SaveItems`, `SaveAttachment`, `SaveStandaloneAttachment`, `GetRecognizedItem`, `HasAttachmentResolvers`, `SaveAttachmentFromResolver`, `Import`, `SelectedCollection`, `UpdateSession`, `GetTranslators`, `DetectTranslators` — NONE closes, completes, or cancels a session. `UpdateSession` at `connector.go:408-438` only files target, tags, and note (`connector.go:408-438`). If sessions accumulate server-side, the client has no way to release them.
* Connector batch/loop warning — original absence now resolved in command help: At report time nothing documented the connector route as unsuitable for batch or loop use — VERIFIED by search across `SKILL.md`, `AGENTS.md`, `docs/`, and every command `Long` help; the warning did not exist. Since the report was written the warning HAS landed in the `Long` help of `internal/cli/import_apply.go` (fullest note — states this command opens one connector session per manifest entry), `internal/cli/import_pdf.go`, and `internal/cli/items_create.go` (states this command already shares one session), regenerating into `docs/reference/commands.md`. Each of the three warnings states: (1) `--via connector` hands work to the running Zotero desktop, which surfaces its own progress UI that zotio cannot dismiss, and no connector endpoint closes or completes a save session; (2) the 2026-08-22 observation — roughly 78 consecutive one-per-item invocations left Zotero unresponsive with progress windows accumulating, with no proven mechanism established; (3) prefer one invocation carrying many records — `import file --via connector` and `items create` share one session while `import apply` opens one per manifest entry; (4) `--rate-limit` governs only Web API requests and does not pace connector calls. `SKILL.md` and `AGENTS.md` were NOT changed, so a reader looking there will not find the warning.

#### In Zotero (`zotero/zotero`, current main) — session reuse, window identity, and the connector handler chain

* Session reuse is impossible by protocol. `chrome/content/zotero/xpcom/server/saveSession.js:25-44` — `SessionManager.create` throws when the session id already exists; the save endpoints answer HTTP 409 (`saveSession.js:25-44`). An external HTTP client therefore CANNOT reuse one session id across calls. "Reuse a single session across invocations" is impossible by protocol — VERIFIED.
* Connector handler paths, so the next person does not re-trace them — VERIFIED at:
  * `saveItems` calls only `session.saveItems(targetID)` — `chrome/content/zotero/xpcom/server/server_connector.js:294-340` (`server_connector.js:294-340`);
  * `saveStandaloneAttachment` calls `Attachments.importFromNetworkStream` then the recogniser — `server_connector.js:442-458` (`server_connector.js:442-458`);
  * `saveAttachment` calls `Attachments.importFromNetworkStream` — `server_connector.js:510-518` (`server_connector.js:510-518`).
* The `SaveSession` holds progress state that nothing on the connector path consumes. `saveSession.js:62-70` — `SaveSession` holds `_progressItems` and `_orderedProgressItems`, but the current client tree contains no consumer of those fields and no `Zotero.ProgressWindow` construction on the connector save path (`saveSession.js:62-70`) — VERIFIED as absence in the searched tree.
* The title `Progress` belongs to `Zotero.ProgressWindow`. `chrome/content/zotero/progressWindow.xhtml:39-43` is the window, whose title entity resolves through `<!ENTITY zotero.progress.title "Progress">` at `chrome/locale/en-US/zotero/zotero.dtd:169` (`progressWindow.xhtml:39-43`, `zotero.dtd:169`). `Zotero.ProgressWindow` exposes `startCloseTimer` and `close` at `chrome/content/zotero/xpcom/progressWindow.js:218-268` (`progressWindow.js:218-268`).
* Stacking is inherent to that class. `chrome/content/zotero/xpcom/progressWindow.js:91-130` — `Zotero.ProgressWindow.show` opens a NEW window per instance and guards only its own `_windowLoading`/`_windowLoaded`; there is no global reuse (`progressWindow.js:91-130`). VERIFIED structural fact: any caller reached once per invocation will stack windows — the mechanism's SHAPE is therefore known even though the specific caller is not.
* The headless-no-timer hypothesis is refuted. `progressWindow.js:218-244` — `startCloseTimer` defaults to 2500 ms and returns early only when `requireMouseOver` is true and the mouse has not touched the window (`progressWindow.js:218-244`). So running headless, with no focus and no user interaction, does NOT by itself prevent the window from closing. That kills the obvious hypothesis, and this report states it so the next reader does not chase it.
* The `attachments.js:1574` call site is a false lead. `chrome/content/zotero/xpcom/attachments.js:1560-1585` — the `ProgressWindow` call at `attachments.js:1574` sits in the `findAvailableFiles` flow, not on the `importFromNetworkStream` path the connector uses; it builds a popup only when no eligible items exist and arms `startCloseTimer(4000)` with no `requireMouseOver`, so it is headless-safe (`attachments.js:1560-1585`) — VERIFIED.

### c. What is NOT established — the dead end, stated without softening

**No source path was found by which a connector HTTP request constructs `Zotero.ProgressWindow`.** The search covered the connector save routes through `server_connector.js:294-340`, `server_connector.js:442-458`, and `server_connector.js:510-518` into `session.saveItems` / `Attachments.importFromNetworkStream`, and found no `new Zotero.ProgressWindow` / `ProgressWindow.show` on that chain. The mechanism that produced the 44 stacked `Progress` windows is therefore **UNPROVEN**. Do not invent one.

**The 78-to-44 ratio is unexplained.** 78 invocations produced 44 or more windows, not 78, and the code gives no mechanism for that ratio — VERIFIED as absence of a counting or coalescing path in the cited files.

**Two attractive theories were investigated and RULED OUT — do not repeat this work:**

* The PDF recogniser dialog is NOT the source. Its title is `Metadata Retrieval` at `chrome/locale/en-US/zotero/zotero.properties:1111` (`zotero.properties:1111`), and `Zotero.ProgressQueueDialog` caches ONE process-wide dialog at `chrome/content/zotero/xpcom/progressQueueDialog.js:27-57` (`progressQueueDialog.js:27-57`). It cannot stack 44 windows and its title does not match.
* The browser-extension iframe is NOT the source. The connector save routes listed above do not reach the iframe path, and the `SaveSession` progress fields at `saveSession.js:62-70` have no consumer on the connector path (`saveSession.js:62-70`).
* The `attachments.js:1574` popup is NOT the source — see `attachments.js:1560-1585` above; it is on `findAvailableFiles`, not on `importFromNetworkStream`.
* The "headless means windows never close" hypothesis is refuted — see `progressWindow.js:218-244` above; windows with no `requireMouseOver` close after the default timer even with no mouse or focus.

**Conclusion of the dead end:** the connector save routes do not provably reach any `Zotero.ProgressWindow` constructor, the headless-no-timer hypothesis is refuted, and the recogniser and the browser-extension iframe are both ruled out. The stacked `Progress` windows remain without a proven caller.

**Two concrete leads left for whoever picks this up — VERIFIED as the narrowest next steps:**

1. Find a caller reached once per invocation that passes `requireMouseOver` to `startCloseTimer` (`progressWindow.js:218-244`). That is the only `ProgressWindow` configuration class that would stack and stay open without user interaction — every other configuration is headless-safe by the timer default. Search the release tree for `new Zotero.ProgressWindow` / `ProgressWindow(` call sites and filter to those that set `requireMouseOver`.
2. Check the Zotero 7 RELEASE branch against main. All of the Zotero evidence above is from `zotero/zotero` current main and the operator runs a release build; a `ProgressWindow` call site present in release but removed or moved in main would explain why the chain is absent here.

### Three zotio-side defects that are proven regardless

These do not depend on the unresolved window mechanism. Each is VERIFIED by the citations above, and each has a fix shape that is **unverifiable until a working Zotero desktop is available** — live `ProgressWindow` behaviour cannot be confirmed against a wedged install, and the operator's desktop must not be contacted.

| # | Proven defect | Citations | Fix shape (unverifiable live) |
|---|---------------|-----------|-------------------------------|
| 1 | `import apply --via connector` allocates one session per manifest entry while `import file --via connector` and `items create --via connector` share one session per command. The per-entry session is the outlier. | `internal/cli/create_route.go:208-216` (per-entry `NewID`); `internal/cli/import_file.go:213-232` (shared session, `done` flag); `internal/cli/items_create.go:115-133` (shared session) | Move `NewID` out of `routeCreateItemVia` and share the session across all entries in the `import apply` invocation, matching the `import_file.go:213-232` pattern. Note the protocol constraint: `saveSession.js:25-44` forbids reusing one id across separate HTTP calls with 409, so "share" here means one session whose id is created once and whose save calls are issued against that one id — verify against the live connector that the server accepts multiple `saveItems` against the same session, or else keep per-entry ids but add pacing/cleanup. Either shape needs a live Zotero to prove. |
| 2 | The connector path has no pacing, retry, backoff, or limiter, while `--rate-limit` silently governs only the Web API client. A user who sets `--rate-limit` expecting connector pacing is silently wrong. | `internal/connector/connector.go:33-46` (no limiter); `internal/cli/root.go:276-277`, `root.go:454-455`, `root.go:555-556` (flag wired only to `client.New`) | Add a connector-scoped limiter (or wire `--rate-limit` to both clients, or add `--connector-rate-limit`), with backoff and a concurrency cap of 1 for connector saves. Needs live verification that pacing prevents the wedge and that 409/429 handling is correct. |
| 3 | Nothing documents the connector route as unsuitable for a tight loop. The failure mode is a wedged desktop with stacked OS windows, not a CLI error. | Absence verified in `SKILL.md`, `AGENTS.md`, `docs/`, and command long-help | **RESOLVED** — Warning has landed in the `Long` help of `internal/cli/import_apply.go`, `internal/cli/import_pdf.go`, and `internal/cli/items_create.go`, regenerating into `docs/reference/commands.md` (see part b for the four facts each warning states: hands work to Zotero desktop with undismissable progress UI and no session close, the 2026-08-22 ~78-invocation observation with no proven mechanism, prefer one invocation carrying many records, and `--rate-limit` does not pace connector calls). `SKILL.md` and `AGENTS.md` were NOT changed. Review wording once the window mechanism is proven, because the warning attributes an observation rather than a proven mechanism. |

Do not implement the remaining fix shapes (rows 1–2) without live verification against a responsive Zotero desktop. The documentation warning (row 3) HAS landed in the `Long` help of `internal/cli/import_apply.go`, `internal/cli/import_pdf.go`, and `internal/cli/items_create.go` (regenerated into `docs/reference/commands.md`); review its wording once the window mechanism is proven, because the warning attributes an observation rather than a proven mechanism.

---

## Verification notes

* No Zotero desktop was contacted. No request was sent to `localhost:23119` or `127.0.0.1:23119`; no screenshot or probe was attempted, per the absolute constraint.
* No test, build, formatter, linter, `make` target, or `zotio` binary was run. Edits are file-only; gates run offline at the end.
* No file outside `dev/field-report-2026-08-22-papio.md` was created or modified; `SKILL.md`, `CHANGELOG.md`, and every `.go` file were left to their sibling owners.

## Suggested order

1. **Finding 1** — already resolved; no further code work. Confirm `internal/cli/results_array_test.go:193-258` stays green.
2. **Finding 2** — decision only: accept or reject the WebDAV-direct feature proposal (`internal/zoteroprefs/zoteroprefs.go:119-121` credential boundary).
3. **Finding 3** — documentation warning (row 3) DONE — it has landed in `internal/cli/import_apply.go`, `internal/cli/import_pdf.go`, `internal/cli/items_create.go` / `docs/reference/commands.md` (see part b). Queue the session-sharing and pacing code changes (rows 1–2) behind a live Zotero desktop, starting from the two leads in part c; review the warning wording once the window mechanism is proven, because it attributes an observation rather than a proven mechanism.
