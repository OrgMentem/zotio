---
name: snapshot-before-destructive-apply
description: "An irreversible apply against the real library needs a recorded snapshot first — the count/tag-hash detector is how silent write failures get caught"
condition:
  - 'zotio[^\n]*--allow-destructive[^\n]*--yes'
  - 'zotio[^\n]*--yes[^\n]*--allow-destructive'
scope: ["tool:bash"]
interruptMode: always
---

**This is an irreversible write against the user's real Zotero library.** `--allow-destructive --yes` is the combination that actually applies; there is no undo, and Web API writes sync down to the desktop.

Before running it:

1. **Record a snapshot** — item count, distinct tags, trash contents, and a hash of the key→tags map. This is a detector, not a courtesy: it is the only reason a connector write that reported `failed` while actually succeeding was ever noticed, where a record count of 2621 → 2622 was the entire signal.
2. **Dry-run the exact command first** (`--dry-run --agent`) and read the plan, not the exit code.
3. **Bound the blast radius** with `--max-changes` where the command supports it.
4. Afterwards, **re-snapshot and diff**. Treat any mismatch beyond the intended change as a finding, and remember propagation runs ~15–20s each way between the read plane (desktop API) and the write plane (`api.zotero.org`) — an immediate re-read can be legitimately stale, which is itself a thing to verify rather than assume.

Use an **independent oracle** for verification: check `api.zotero.org` directly, never another zotio command, and page every endpoint you use as an oracle (an unpaged `/tags` returns 25 rows, which once turned a restoration check into a page-size measurement).

If this is a scratch or test library rather than the user's real one, say so and continue.
