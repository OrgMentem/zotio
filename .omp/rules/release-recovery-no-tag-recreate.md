---
name: release-recovery-no-tag-recreate
description: "Never recreate a tag or delete/recreate a GitHub release to recover a failed publish — re-run the failed job instead"
condition:
  - 'git tag[^\n]*-d[^\n]*v[0-9]'
  - 'git push[^\n]*(--delete|:refs/tags)'
  - 'gh release delete'
  - 'git tag[^\n]*-f[^\n]*v[0-9]'
scope: ["tool:bash"]
interruptMode: always
---

**Do not recreate the tag, delete or recreate the GitHub release, or re-run GoReleaser to recover a failed publish.** The published assets are the input to recovery; destroying them destroys the only thing that can be verified.

When GitHub publication succeeded but registry publication failed:

1. Run validation step 8 in `dev/releasing.md` through the `sha256sum -c` line — it downloads the existing release's MCPB assets, regenerates `server.json` for that exact version, and proves the recorded hashes match those assets.
2. In the failed tag-triggered Actions run, choose **Re-run failed jobs** — *not* "Re-run all jobs". The job boundary is intentional: only `publish_registry` reruns, downloads those same existing assets, regenerates `server.json`, obtains a fresh approved `github-oidc` identity, and republishes.
3. Then run all of validation step 8 against the exact-version Registry endpoint (never `latest`).

GitHub Actions cannot re-run an individual step, and a workstation/PAT publication is not a substitute — the `id-token: write` identity from `OrgMentem/zotio` is what proves control of the `io.github.OrgMentem/*` namespace.

Also: if signing failed with `invalid character 'u' ... fetching ambient OIDC credentials`, that is almost always a GitHub Actions OIDC outage, not a config bug. Check <https://www.githubstatus.com> first, wait for recovery, then re-run the failed run.
