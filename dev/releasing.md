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
`microsoft/winget-pkgs`). After that job succeeds, the dependent
`publish_registry` job downloads the published MCPB assets, regenerates their
hashes in `server.json`, and publishes `io.github.OrgMentem/zotio` to the
**Official MCP Registry**. CI builds the *tagged commit* from a clean checkout,
so local uncommitted changes never enter a release.

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
- Official MCP Registry publication uses GitHub Actions OIDC and needs no
  registry secret. The `id-token: write` identity from **`OrgMentem/zotio`**
  proves control of the `io.github.OrgMentem/*` namespace; moving/forking the
  workflow or changing the manifest namespace changes that ownership claim.
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
6. Watch both jobs. `publish_registry` starts only after the GoReleaser
   `release` job succeeds. If signing fails with `invalid character 'u' ...
   fetching ambient OIDC credentials`, check <https://www.githubstatus.com> for
   an Actions incident *before* touching config — it is almost always a GitHub
   OIDC outage, not us. Wait for recovery, then re-run the failed run.

**Validate distribution** (post-publish, from a Linux box with `gh`, `jq`,
`curl`, and `sha256sum`)
7. GitHub release: `gh release view vX.Y.Z --repo OrgMentem/zotio --json isDraft,assets`
   (expect not-draft, `.deb`/`.rpm`/`.apk`, `checksums.txt.sigstore.json`).
   The release workflow's "Smoke-test version stamp" step already gated this,
   but spot-check anyway: download one archive and confirm `zotio version` and
   `zotio-mcp --version` both print `X.Y.Z` (guards the `-X zotio/internal/cli.version`
   ldflag both builds share in `.goreleaser.yaml`).
8. Official MCP Registry: fetch the exact version, not `latest`, and compare
   every published package identifier and SHA-256 with a freshly generated
   manifest over the GitHub release assets. `diff` must produce no output:
   ```
   version=X.Y.Z
   workdir="$(mktemp -d)"
   mkdir -p "$workdir/mcpb"
   gh release download "v${version}" --repo OrgMentem/zotio \
     --pattern '*.mcpb' --dir "$workdir/mcpb"
   python3 scripts/gen_server_json.py "$version" \
     --mcpb-dir "$workdir/mcpb" --out "$workdir/server.json"
   jq -r '.packages[] | "\(.fileSha256)  \(.identifier | split("/")[-1])"' \
     "$workdir/server.json" > "$workdir/mcpb.sha256"
   (cd "$workdir/mcpb" && sha256sum -c "$workdir/mcpb.sha256")
   curl -fsS \
     "https://registry.modelcontextprotocol.io/v0.1/servers/io.github.OrgMentem%2Fzotio/versions/${version}" \
     -o "$workdir/registry.json"
   test "$(jq -r '.server.version' "$workdir/registry.json")" = "$version"
   diff -u \
     <(jq -S '[.packages[] | {identifier,fileSha256}] | sort_by(.identifier)' "$workdir/server.json") \
     <(jq -S '[.server.packages[] | {identifier,fileSha256}] | sort_by(.identifier)' "$workdir/registry.json")
   rm -rf "$workdir"
   ```
9. Scoop: read `https://raw.githubusercontent.com/OrgMentem/scoop-bucket/master/bucket/zotio.json`
   — version matches, both arches, both binaries.
10. Homebrew: read `https://raw.githubusercontent.com/OrgMentem/homebrew-tap/main/Formula/zotio.rb`
    — version matches.
11. WinGet: `gh pr list --repo microsoft/winget-pkgs --search "OrgMentem.zotio"`.

**WinGet PR completion** (Microsoft side)
12. First-ever submission gets the `New-Package` label, requires the **CLA**
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
- **Recovery after GitHub succeeds but registry publication fails.** Do not
  recreate the tag, delete/recreate the GitHub release, or re-run GoReleaser.
  First use the commands in validation step 8 through the `sha256sum -c` line:
  they download the existing release's MCPB assets, regenerate `server.json` for
  that exact version, and prove its recorded hashes match those assets. In the
  failed tag-triggered Actions run, choose **Re-run failed jobs**, not “Re-run
  all jobs.” The workflow's job boundary is intentional: only
  `publish_registry` reruns, downloads those same existing assets, regenerates
  `server.json`, obtains a fresh approved `github-oidc` identity, and republishes.
  Then run all of validation step 8 against the exact-version Registry endpoint.
  GitHub Actions cannot rerun an individual step; do not rerun the `release` job
  or substitute a workstation/PAT publication.
- **Upgrading `mcp-publisher` requires upgrading its checksum in the same
  review.** Choose a specific publisher release (never `latest`) and obtain the
  Linux amd64 asset digest from the official GitHub release metadata:
  ```
  publisher_version=X.Y.Z
  gh api "repos/modelcontextprotocol/registry/releases/tags/v${publisher_version}" \
    --jq '.assets[] | select(.name == "mcp-publisher_linux_amd64.tar.gz") | .digest'
  ```
  Download that exact asset independently and confirm its SHA-256, then update
  both `publisher_version` and `publisher_sha256` in `release.yml`. Keep the
  archive download, `sha256sum -c`, and extraction as separate ordered commands;
  never pipe publisher bytes into `tar` or execute before the check passes.
- **`brews:` is deprecated** (GoReleaser renamed it toward `homebrew_casks`). It
  still works on the pinned action; migrate deliberately, not mid-release.
- **`go install` does not work** — the module path is bare `module zotio`, not a
  fetchable URL. Don't advertise it. Source installs use
  `git clone && go build -o zotio ./cmd/zotio`.
