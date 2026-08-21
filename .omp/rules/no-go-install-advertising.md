---
name: no-go-install-advertising
description: "Never document go install for zotio — the module path is bare `module zotio`, not a fetchable URL, so it cannot work"
condition:
  - 'go install\s+\S*zotio\S*'
scope:
  - "tool:write(*.md)"
  - "tool:edit(*.md)"
interruptMode: always
---

**`go install` cannot work for zotio.** `go.mod` declares `module zotio` — a bare module path, not a fetchable URL — so there is nothing for the Go toolchain to resolve. Documenting it hands users a command that fails.

Source installs are:

```
git clone https://github.com/OrgMentem/zotio
cd zotio && go build -o zotio ./cmd/zotio
```

For everyone else, point at the real distribution channels, which a release publishes automatically: the GitHub release archives (plus `.deb` / `.rpm` / `.apk`), the Homebrew **cask** (`OrgMentem/homebrew-tap`), the Scoop bucket (`OrgMentem/scoop-bucket`), WinGet (`OrgMentem.zotio`), and — for the MCP server — the Official MCP Registry entry `io.github.OrgMentem/zotio`.

Changing the module path to make `go install` work is a real option, but it is a breaking distribution decision affecting every import path and the registry namespace claim — raise it, do not land it inside a docs edit.
