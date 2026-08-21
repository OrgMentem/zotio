---
name: release-tag-requires-green-ci
description: "A release tag must point at a commit whose ci workflow is already green, validated by make ci and a real-library smoke run — release re-runs none of those gates"
condition:
  - 'git tag -a[^\n]*v[0-9]+\.[0-9]'
  - 'git push[^\n]*origin[^\n]*v[0-9]+\.[0-9]'
scope: ["tool:bash"]
interruptMode: always
---

**Tagging is publication.** Pushing a `vX.Y.Z` tag runs GoReleaser and publishes in one pass: GitHub release, Homebrew tap, Scoop bucket, WinGet PR, then the Official MCP Registry. Confirm all three gates below first.

1. **CI must already be green on that exact commit.** `release` does **not** re-run `tidy` / `docs-drift` / `format` / `lint` / `test`, so a `go mod tidy` drift or a lint failure sails straight into a tagged release. That happened on v0.6.0 — the tag pointed at a commit with an untidy `go.mod`.
2. **Run `make ci` locally** — the same gates, job for job, ~2.5 minutes, including `-race` and the six-target cross-build. Neither is in `make test`. v0.19.0 was pushed green locally and failed CI three times: a data race two new tests had just introduced, a Linux-only discovery bug macOS structurally cannot reach, and a test that only runs when Zotero is not holding `:23119`.
3. **CI green does not prove the binary works.** Every P0 in the 0.17.0 cycle was invisible to a green suite — `tags rename` could not write a single tag while `go test ./...` passed, because the tests mocked away the read/write plane split. Install the release binary and exercise the changed commands against a real library:
   - **Snapshot, revert, re-snapshot, and treat a mismatch as a finding.** Record count, distinct tags, trash, and a hash of the key→tags map; revert every mutation; confirm byte-identical. This detector is the only reason a connector write that reported `failed` while actually succeeding was ever noticed — a record count of 2621 → 2622 was the entire signal.
   - **Both planes, both directions.** Reads resolve against the desktop API, writes route to `api.zotero.org`, propagation ~15–20s each way. Test read-then-write, write-then-read, and write-then-read-immediately.
   - **Use an independent oracle and page it.** Check against `api.zotero.org` directly, never against another zotio command; an unpaged `/tags` returns 25 rows.
   - **Distrust green.** "PASS" ≠ "no panics" (`net/http` recovers per connection, so a panicking test still prints `ok` — grep the raw log) and "PASS" ≠ "actually ran" (check for `(cached)`, use `-count=1`).

Also confirm `CHANGELOG.md` is finalized (`[Unreleased]` → `[X.Y.Z] — DATE`, link refs fixed, breaking changes in their own `### Changed — breaking` section) and that a snapshot dry-run inspected the generated manifests. Full runbook: `dev/releasing.md`.
