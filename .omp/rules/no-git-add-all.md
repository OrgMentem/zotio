---
name: no-git-add-all
description: "Never git add -A / --all / . in this tree — stage specific files, because sibling sessions leave foreign WIP"
condition:
  - 'git add (-A|--all)\b'
  - 'git add \.(\s|$)'
scope: ["tool:bash"]
interruptMode: always
---

**Stage specific files by name.** This tree routinely carries work from sibling sessions, so `git add -A` sweeps someone else's in-progress changes into your commit — and a release commit built that way tags code nobody reviewed.

`dev/releasing.md` makes this explicit for the release path (step 1: confirm unrelated sibling WIP is *not* included; step 4: "stage specific files — never `git add -A` into a tree with foreign WIP"), but it applies to every commit here.

Do this instead:

```
git status --short          # see what is actually in the tree
git add path/one path/two   # name your files
git diff --cached           # confirm the staged set before committing
```

If you genuinely intend to stage everything and have just verified the tree holds only your work, say so and continue.
