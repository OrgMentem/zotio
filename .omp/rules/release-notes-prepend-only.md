---
name: release-notes-prepend-only
description: "Never replace GoReleaser's release notes — gh release edit --notes/--notes-file overwrites the whole body and destroys the generated changelog"
condition:
  - 'gh release edit[^\n]*--notes'
scope: ["tool:bash"]
interruptMode: always
---

**`gh release edit --notes` / `--notes-file` overwrites the entire release body.** GoReleaser's generated notes — the SHA-prefixed, grouped changelog (Features / Fixes / Documentation / Build & CI, ascending) plus the footer line — are destroyed, and nothing regenerates them for an existing release.

If you must add a breaking-changes callout:

1. Reconstruct GoReleaser's changelog verbatim:
   ```
   git log vLAST..vX.Y.Z --pretty='* %H: %s (@%an)'
   ```
   grouped Features / Fixes / Documentation / Build & CI, ascending, keeping the footer line.
2. **Prepend** your section above it.
3. Only then write the combined body.

Reading is safe (`gh release view`). This rule is about the write.
