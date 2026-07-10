# Releasing zotio

How a zotio release actually works, the decisions to make, and the footguns that
have bitten us. **Source of truth for mechanics is the config, not this file** —
read `.goreleaser.yaml` and `.github/workflows/release.yml`; this note captures
only what those files *don't* say (decisions, sequencing, and sharp edges).

## Flow in one paragraph

Push a `vX.Y.Z` tag → `.github/workflows/release.yml` runs GoReleaser on
`ubuntu-latest` → GoReleaser builds all targets, signs with cosign (keyless,
GitHub Actions OIDC), then publishes in one pass: the **GitHub release**
(archives, `.deb`/`.rpm`/`.apk`, SBOMs, checksums, sigstore bundle, MCPB
bundles), the **Homebrew tap** (`OrgMentem/homebrew-tap`), the **Scoop bucket**
(`OrgMentem/scoop-bucket`), and a **WinGet PR** (`OrgMentem/winget-pkgs` fork →
`microsoft/winget-pkgs`). CI builds the *tagged commit* from a clean checkout, so
local uncommitted changes never enter a release.

## Decisions before you tag

- **Version (pre-1.0 semver).** New behavior/features → minor (`0.N.0`); fixes
  only → patch (`0.N.M`). We are pre-1.0, so **breaking changes ship in a minor
  release with no major-version signal** — they MUST be called out explicitly
  (see below).
- **Breaking changes.** Anything that changes JSON shapes, exit codes, or
  command behavior for scripted/agent consumers is breaking. Put them in a
  dedicated `### Changed — breaking` section in `CHANGELOG.md` (don't bury them
  under Added/Changed), and surface them in the GitHub release notes.
- **Commit type drives release-note grouping.** `feat:` → Features, `fix:` →
  Fixes, `docs:` → Documentation, `ci|chore|brand:` → Build & CI
  (`.goreleaser.yaml` `changelog.groups`). GoReleaser regexes the **subject only**
  — a `BREAKING CHANGE:` footer is NOT parsed. Name commits accordingly.

## Prerequisites (one-time, verify before first use)

- Repos exist: `OrgMentem/homebrew-tap`, `OrgMentem/scoop-bucket`, and a fork
  `OrgMentem/winget-pkgs` of `microsoft/winget-pkgs`.
- Actions secrets on **`OrgMentem/zotio`** (Settings → Secrets → Actions):
  `HOMEBREW_TAP_GITHUB_TOKEN`, `SCOOP_BUCKET_GITHUB_TOKEN`,
  `WINGET_GITHUB_TOKEN`.
  - **`WINGET_GITHUB_TOKEN` MUST be a classic PAT with `public_repo`** — see
    footgun below. The other two can be fine-grained (contents:write on their
    target repo).
  - Fine-grained PATs on org repos may sit in a **pending-approval** queue; an
    org owner must approve or the first release 403s.
- `nfpms.maintainer` in `.goreleaser.yaml` is **permanent and public** in every
  `.deb`/`.rpm` — currently `OrgMentem <zotio@orgmentem.com>` (that alias must
  route somewhere real).

## Checklist

**Pre-tag**

> **Tag only a commit whose `ci` workflow is already green.** `release` does NOT
> re-run `tidy`/`docs-drift`/`format`/`lint`/`test`, so a `go mod tidy` drift or
> lint failure sails straight into a tagged release (this bit us on v0.6.0 — the
> tag pointed at a commit with an untidy `go.mod`). Check the commit's CI first.

1. `git log vLAST..HEAD` — confirm the changeset and that unrelated sibling WIP
   is *not* included. Stage only your files if the tree has others' work.
2. Update `CHANGELOG.md`: rename `[Unreleased]` → `[X.Y.Z] — DATE`, add the
   `Changed — breaking` section if needed, fix the link refs at the bottom.
3. **Snapshot dry-run** (validates manifests locally, publishes nothing):
   ```
   go run github.com/goreleaser/goreleaser/v2@latest release --snapshot --clean --skip=sbom,sign
   ```
   Then inspect `dist/scoop/bucket/zotio.json`, `dist/winget/manifests/.../*.yaml`,
   `dist/homebrew/Formula/zotio.rb`. (`--skip=sbom,sign` bypasses `syft`/`cosign`,
   which aren't needed locally; CI installs them.)

**Tag & push**
4. `git commit` your release changes (stage specific files — never `git add -A`
   into a tree with foreign WIP).
5. `git tag -a vX.Y.Z -m "..."` && `git push origin main && git push origin vX.Y.Z`.

**Watch**
6. Watch the run. If it fails at **signing** with `invalid character 'u' ...
   fetching ambient OIDC credentials`, check <https://www.githubstatus.com> for
   an Actions incident *before* touching config — it's almost always a GitHub
   OIDC outage, not us. Wait for recovery, then re-run the failed run.

**Validate distribution** (post-publish, from a mac/linux box)
7. GitHub release: `gh release view vX.Y.Z --repo OrgMentem/zotio --json isDraft,assets`
   (expect not-draft, `.deb`/`.rpm`/`.apk`, `checksums.txt.sigstore.json`).
8. Scoop: read `https://raw.githubusercontent.com/OrgMentem/scoop-bucket/master/bucket/zotio.json`
   — version matches, both arches, both binaries.
9. Homebrew: read `https://raw.githubusercontent.com/OrgMentem/homebrew-tap/main/Formula/zotio.rb`
   — version matches.
10. WinGet: `gh pr list --repo microsoft/winget-pkgs --search "OrgMentem.zotio"`.

**WinGet PR completion** (Microsoft side)
11. First-ever submission gets the `New-Package` label, requires the **CLA**
    (the submitter comments `@microsoft-github-policy-service agree` on the PR —
    a human legal act, not automatable), and goes through **manual moderation**.
    Contributors do NOT merge winget-pkgs PRs; `mergeState: BLOCKED /
    REVIEW_REQUIRED` is normal. Just wait; act only if a bot requests changes.

## Footguns

- **WinGet PR 403 "Resource not accessible by personal access token".** A
  fine-grained PAT can push the branch to the fork but **cannot open a cross-org
  PR** to `microsoft/winget-pkgs`. `WINGET_GITHUB_TOKEN` must be a **classic PAT
  with `public_repo`**. Stopgap for a release that already pushed the fork branch:
  open the PR by hand — `gh pr create --repo microsoft/winget-pkgs --head
  OrgMentem:zotio-X.Y.Z --base master ...` — using a token/login that can PR
  public repos.
- **Never *replace* release notes; only prepend.** `gh release edit
  --notes-file` overwrites the entire body, destroying GoReleaser's SHA-prefixed,
  grouped changelog. If you must add a breaking-changes callout, reconstruct
  GoReleaser's changelog verbatim (`git log vLAST..vX.Y.Z --pretty='* %H: %s (@%an)'`,
  grouped Features/Fixes/Documentation/Build & CI, `asc`) and prepend your
  section above it, keeping the footer line.
- **Signing failure ≠ config bug.** `cosign` keyless signing depends on the
  GitHub Actions OIDC service. During an Actions outage the token fetch returns
  non-JSON and cosign dies with `invalid character 'u'`. Check status first.
- **WinGet needs non-prerelease semver.** A `-rc`/`-beta` tag skips the WinGet
  publisher (GoReleaser `prerelease: auto`). Scoop/Homebrew still run.
- **Re-runs after a partial publish.** Signing runs *before* publish, so a
  signing failure leaves nothing published and a re-run is clean. If a run fails
  *after* the GitHub release is created, a re-run may error on an existing
  release — fix forward (open the missing winget PR / push the missing manifest)
  rather than re-running the whole pipeline.
- **`brews:` is deprecated** (GoReleaser renamed it toward `homebrew_casks`). It
  still works on the pinned action; migrate deliberately, not mid-release.
- **`go install` does not work** — the module path is bare `module zotio`, not a
  fetchable URL. Don't advertise it. Source installs use
  `git clone && go build -o zotio ./cmd/zotio`.
