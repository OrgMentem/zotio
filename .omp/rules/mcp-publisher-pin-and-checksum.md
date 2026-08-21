---
name: mcp-publisher-pin-and-checksum
description: "Upgrading mcp-publisher means bumping publisher_version AND publisher_sha256 in the same review, from official release metadata, never latest"
condition:
  - 'publisher_version'
  - 'publisher_sha256'
  - 'mcp-publisher'
scope:
  - "tool:edit(release.yml)"
  - "tool:write(release.yml)"
interruptMode: never
---

`publisher_version` and `publisher_sha256` in `.github/workflows/release.yml` are a **pair**. A version bump without its digest either fails the `sha256sum -c` gate or, worse, verifies the wrong bytes.

Choose a specific publisher release — **never `latest`** — and take the Linux amd64 digest from the official GitHub release metadata:

```
publisher_version=X.Y.Z
gh api "repos/modelcontextprotocol/registry/releases/tags/v${publisher_version}" \
  --jq '.assets[] | select(.name == "mcp-publisher_linux_amd64.tar.gz") | .digest'
```

Download that exact asset independently, confirm its SHA-256, then update **both** values.

Keep the archive download, `sha256sum -c`, and extraction as **separate ordered commands**. Never pipe publisher bytes into `tar`, and never execute anything before the checksum passes — that ordering is the entire security property of this step.

Registry publication needs no secret: it uses GitHub Actions OIDC, and the `id-token: write` identity from `OrgMentem/zotio` is what proves control of the `io.github.OrgMentem/*` namespace. Moving or forking this workflow, or changing the manifest namespace, changes that ownership claim.
