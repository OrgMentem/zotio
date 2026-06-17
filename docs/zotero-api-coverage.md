# Zotero API Coverage & Refresh

Living record of which Zotero endpoints this CLI uses, what it's missing, and
**how to re-check** so the tool keeps maximal endpoint coverage as Zotero evolves.
Re-run the refresh procedure when a new Zotero version ships (the release cycle is
fast now — see below) and update the "Last reviewed" line at the bottom.

This is maintenance knowledge for agents *developing* the repo. Product-usage docs
live in `README.md` / `SKILL.md`; the missable invariants live in `AGENTS.md`.

## Where the truth lives

The CLI targets Zotero's **local API**, not Better BibTeX and not the cloud API by
default. Canonical, continuously-updated sources (check the "Last updated" date on
each page):

- Local API contract: https://www.zotero.org/support/dev/web_api/v3/local_api
- Web API basics (endpoint list, params): https://www.zotero.org/support/dev/web_api/v3/basics
- Item types & fields: https://www.zotero.org/support/dev/web_api/v3/types_and_fields
- Write requests (when local writes land): https://www.zotero.org/support/dev/web_api/v3/write_requests
- Version history / release notes: https://www.zotero.org/support/changelog (current major) plus split `…/8.0_changelog`, `…/7.0_changelog`
- Beta builds (unreleased changes): https://www.zotero.org/support/beta_builds
- Source of truth ahead of the changelog: https://github.com/zotero/zotero/commits/ and local-API PRs (e.g. [#2842](https://github.com/zotero/zotero/pull/2842) original local API, [#5004](https://github.com/zotero/zotero/pull/5004) `/fulltext`)

NOT API references: the per-version "Zotero N for Developers" pages are
Mozilla-platform migration guides for plugin authors, tied to big Firefox jumps
(7 = FF115, 8 = FF140). They do not document REST endpoints. Don't rely on their
presence/absence to judge API changes.

## How the CLI is generated (why coverage can lag)

`spec.yaml` was derived (by the build agent) from a community OpenAPI spec
(bitowl/zotero-openapi, ~25 Web API v3 endpoints), **not** a live probe of a running
Zotero — the press's sniff gate skipped because Zotero wasn't running at build time.
Endpoint coverage is therefore exactly: what that community spec described, plus the
hand-written commands in `internal/cli/`. New endpoints never appear on their own;
they must be added to `spec.yaml` (then regen) or written as a `// PATCH:` command.

## Invariants & constraints (the gotchas)

- **Local API is GET-only** (Local API doc dated 2026-06-07; write support is
  "coming"). **Verified 2026-06-17 against Zotero 10.0-beta:** `POST /items` → `400`
  "Endpoint does not support method", `PUT /items/<key>` → `501` "Method not
  implemented", while `GET` returns `200`. So `items create/update/delete`,
  `items enrich --yes` apply, and `import` writes do **not** work against
  `localhost:23119`. They only succeed against the Web API (`api.zotero.org` + API
  key) or with a community local-write plugin. Treat local mutation as unsupported
  until the docs say otherwise.
- **Schema/type endpoints are global**, served under `/api` directly, NOT under the
  `/users|groups/<id>` library prefix the configured base URL carries:
  `/api/itemTypes`, `/api/itemFields`, `/api/itemTypeFields`,
  `/api/itemTypeCreatorTypes`, `/api/creatorFields`, `/api/items/new`. The generated
  `schema *` commands keep the prefix and therefore **404** against live Zotero;
  `schema drift` works around it by stripping the prefix (`stripLibraryPrefix` in
  `internal/cli/schema_drift.go`). Apply the same fix if you repair the generated
  schema commands.
- **Web API v3 endpoint set is stable/versioned**; the **local API is the evolving
  surface**. New Zotero releases (fast cycle: 8 → 9 → 10 …, every 6–10 weeks) almost
  always add *fields/data*, rarely endpoints. Use `schema drift` to catch field/type
  deltas after an upgrade.
- **`Zotero-Schema-Version` response header** is returned on every local-API response
  and reflects the install's schema version — a cheap one-request signal that the
  schema changed (potential fast-path for `schema drift`).
- The local API must be enabled in Zotero: Settings → Advanced → "Allow other
  applications on this computer to communicate with Zotero" (else `403`). Pass user
  ID `0` or the real numeric ID; any other ID returns `400`.

## Endpoint coverage matrix

Covered = exercised by a generated or hand-written command. Verify with
`grep -nE 'path:' spec.yaml` and `grep -rn 'c.Get' internal/cli`.

| Endpoint(s) | Covered | Where |
| --- | --- | --- |
| `/collections`, `/collections/top`, `/collections/<key>`, `…/collections`, `…/items[/top]` | ✅ | collections commands |
| `/items`, `/items/top`, `/items/trash`, `/items/<key>`, `/items/<key>/children`, `/items/<key>/tags` | ✅ | items commands |
| `/searches`, `/searches/<key>` | ✅ | searches commands |
| `/searches/<key>/items` (execute saved search — local-only) | ✅ | `searches run` |
| `/tags`, `/tags/<name>` | ✅ | tags commands |
| `/itemTypes`, `/itemFields`, `/itemTypeFields`, `/itemTypeCreatorTypes`, `/creatorFields`, `/items/new` | ⚠️ | generated `schema *` (404 — prefix bug); `schema drift` works |
| `/items/<key>/fulltext`, `/fulltext?since=` | ✅ | `sync --fulltext`, `items fulltext` (hhup) |
| `/items/<key>/file/view/url` (on-disk attachment path — local-only) | ✅ | `items file` |
| `/publications/items`, `/publications/items/tags` (My Publications) | ❌ | gap (low value) |
| `format=keys`, `format=versions` modes | ❌ | not used (we sync via `since=`) |
| `/keys/<key>` | ❌ | n/a for local (no auth) |

### Known gaps worth considering

1. **My Publications** (`/publications/items`) — niche.
2. **Generated schema commands 404** — fix them to strip the library prefix (mirror
   `schema drift`), or fix upstream (per-endpoint base-path/scope override in
   cli-printing-press).

Resolved: attachment file paths are now covered by `items file` (`/items/<key>/file/view/url`).

## Refresh procedure

Run this when a new Zotero version ships, or periodically:

1. **Read the docs**, note each page's "Last updated" date: Local API, Web API
   basics, types_and_fields. Diff against this matrix.
2. **Skim the changelog** for the current major (`/support/changelog`) and the
   GitHub commit log / local-API PRs since the last review. Beta changelogs are NOT
   published — the commit log is the only source for unreleased (e.g. *-beta) changes.
3. **Diff documented endpoints vs. what we implement**:
   `grep -nE 'path:' spec.yaml` and `grep -rn 'c\.Get\|c\.GetWithVersion' internal/cli`.
4. **Run `zotero-pp-cli schema drift`** against live Zotero to catch new/removed item
   types and fields (the realistic between-version delta). `--deep` for per-type.
5. **For genuinely new endpoints**: add to `spec.yaml` (then regen) or implement a
   hand-written command (`// PATCH:` marker + `.printing-press-patches.json` entry,
   per AGENTS.md). Add a `which` index entry if it's a hero feature.
6. **Update this doc**: the matrix + the "Last reviewed" line below.

## Last reviewed

- **2026-06-17** — against Zotero 9.0.5 (stable) and 10.0-beta; Local API doc dated
  2026-06-07. No new REST endpoints since `/fulltext` (Jan 2025). Web API v3 stable.
  Open gaps: attachment file-path endpoints, My Publications, generated schema-command
  prefix bug. `schema drift` added this session to track field/type drift.
