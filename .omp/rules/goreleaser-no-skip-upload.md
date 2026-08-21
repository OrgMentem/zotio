---
name: goreleaser-no-skip-upload
description: "A skip_upload publisher ships nothing while the release job stays green — six zotio releases never reached WinGet that way"
condition:
  - 'skip_upload'
  - 'skip_publish'
scope:
  - "tool:edit(.goreleaser.yaml)"
  - "tool:write(.goreleaser.yaml)"
interruptMode: always
---

**A publisher with `skip_upload` set silently publishes nothing, and the release job still reports success.**

`winget.skip_upload: true` was set during the New-Package wait so parallel PRs would not stack on Microsoft's moderators. It then stayed set through **v0.8.0–v0.13.1**: GoReleaser wrote the manifests, logged exactly one line —

```
pipe skipped or partially skipped  reason=winget.skip_upload is set
```

— and published nothing, while every checklist step in `dev/releasing.md` passed. **Six releases never reached WinGet.** The only evidence was that `microsoft/winget-pkgs` still carried 0.7.0 and the fork had no branch past `zotio-0.7.0`.

If you are setting a publisher skip as a temporary measure, both of these are required:

1. Write the **removal condition** into the config comment beside it — what has to become true for this to come back out.
2. Add the log grep to release step 6, so a skipped pipe is visible on every release:
   ```
   gh run view <run-id> -R OrgMentem/zotio --log \
     | grep -i "pipe skipped" | grep -v "disabled during snapshot mode"
   ```
   Expect no output. The `grep -v` is required, not cosmetic: the "Build snapshot for version smoke test" step legitimately logs a skip on every release, and a check that fires every time is a check nobody reads — which is exactly how this footgun survived six releases.

A green job is not evidence that a channel published.
