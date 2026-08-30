# Install

`zotio` comes in three pieces you can install independently: the **CLI** (the engine — everything runs through it), the **agent skill** (drives the CLI inside coding agents), and the **MCP server** (exposes the CLI to MCP hosts). Most people want the CLI; add the skill or MCP server for your agent of choice.

!!! tip "Try it before wiring anything"
    `zotio demo` seeds a bundled sample library with no Zotero desktop or API key. Use `ZOTIO_DEMO=1 zotio --data-source local <command>` to keep reads inside it. When you are convinced, `zotio init` sets up the real library.

## 1. The CLI — `zotio`

=== "macOS"

    **Homebrew** — installs both `zotio` and the `zotio-mcp` MCP server; `brew upgrade` tracks new releases:

    ```bash
    brew install orgmentem/tap/zotio
    ```

=== "Linux"

    **Distro packages** — every [GitHub release](https://github.com/OrgMentem/zotio/releases) ships `.deb`, `.rpm`, and `.apk` for amd64/arm64, each bundling both `zotio` and `zotio-mcp`. There is no apt/dnf/pacman repository to add: the packages are release assets, and their filenames carry the version. This resolves the latest release and your architecture for you:

    ```bash
    ver=$(curl -fsSL https://api.github.com/repos/OrgMentem/zotio/releases/latest |
      sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p')
    arch=$(uname -m); case "$arch" in x86_64) arch=amd64;; aarch64) arch=arm64;; esac
    pkg=zotio_${ver}_linux_${arch}

    # Debian / Ubuntu
    curl -fsSLO "https://github.com/OrgMentem/zotio/releases/download/v${ver}/${pkg}.deb" && sudo dpkg -i "${pkg}.deb"

    # Fedora / RHEL / openSUSE
    curl -fsSLO "https://github.com/OrgMentem/zotio/releases/download/v${ver}/${pkg}.rpm" && sudo rpm -i "${pkg}.rpm"

    # Alpine
    curl -fsSLO "https://github.com/OrgMentem/zotio/releases/download/v${ver}/${pkg}.apk" && sudo apk add --allow-untrusted "${pkg}.apk"
    ```

    Upgrades are the same command again (`dpkg -i` / `rpm -U` / `apk add`). Homebrew works on Linux too — the tap ships formulae, not casks (`brew install orgmentem/tap/zotio`).

=== "Windows"

    **WinGet** — installs both `zotio` and `zotio-mcp`:

    ```powershell
    winget install OrgMentem.zotio
    ```

    **Scoop** — the same two binaries; `scoop update zotio` tracks new releases:

    ```powershell
    scoop bucket add orgmentem https://github.com/OrgMentem/scoop-bucket
    scoop install zotio
    ```

    !!! note "WinGet can lag a new release by a few days"
        Each release opens a version-bump PR against `microsoft/winget-pkgs`,
        which their moderators merge on their own schedule. `winget install`
        always works, but it may serve the previous version for a short window
        after a release. Scoop and the prebuilt archives update immediately.

=== "Prebuilt binary"

    Every [GitHub release](https://github.com/OrgMentem/zotio/releases) ships archives for macOS, Linux, and Windows (amd64/arm64) with cosign-signed checksums and SBOMs — both `zotio` and `zotio-mcp` in each archive. Unpack and put the binaries on your `PATH`:

    - **macOS:** clear the Gatekeeper quarantine — `xattr -d com.apple.quarantine zotio`, then `chmod +x zotio`
    - **Linux:** `chmod +x zotio`
    - **Windows:** unzip and add the folder to your `PATH`

=== "From source"

    ```bash
    git clone https://github.com/OrgMentem/zotio && cd zotio
    go build -o zotio ./cmd/zotio
    go build -o zotio-mcp ./cmd/zotio-mcp   # optional: the MCP server
    ```

    Needs **Go 1.26.5 or newer** — `go.mod` declares `go 1.26.5`, so the default
    `GOTOOLCHAIN=auto` downloads that toolchain when your Go is older, and
    `GOTOOLCHAIN=local` refuses the build instead of producing a binary you
    should not ship. The floor is a security one: 1.26.5 fixes CVE-2026-42505 in
    `crypto/tls`, the stack every API-key-bearing request rides. Release
    archives are always built on a patched toolchain.

Then let the CLI walk you through setup — Zotero detection, the local-API toggle, an optional Web API key, first sync, and a health check:

```bash
zotio init
```

!!! tip "Enable the local API"
    Reads and keyless item creation talk to your Zotero desktop app at `localhost:23119`. Turn it on once in Zotero: **Settings → Advanced → "Allow other applications to communicate with Zotero."** (`zotio init` walks you through this.)

## 2. The agent skill

A focused skill — bundled in the repo as [`SKILL.md`](https://github.com/OrgMentem/zotio/blob/main/SKILL.md) — that teaches a coding agent to drive the CLI directly (the most efficient path; no MCP server in the middle).

**Recommended — the [`skills` CLI](https://skills.sh)** (works across Claude Code, Cursor, Codex, Cline, opencode, and 40+ agents):

```bash
npx skills add OrgMentem/zotio          # detect your agents and install
npx skills add OrgMentem/zotio --list   # preview without installing
npx skills add OrgMentem/zotio -g       # install globally (all projects)
```

**Manual:**

- **Claude Code:** copy `SKILL.md` into `~/.claude/skills/zotio/SKILL.md` (or your project's `.claude/skills/zotio/`).
- **Any other agent:** point it at the raw file — `https://raw.githubusercontent.com/OrgMentem/zotio/main/SKILL.md` — or paste it into your agent's skill store.

See [Use in a coding agent](agent-skill.md) for how to drive it.

## 3. The MCP server — `zotio-mcp`

`zotio-mcp` ships alongside the CLI — the Homebrew formula and every release archive include both binaries. Register it:

```bash
# Claude Code
claude mcp add zotero zotio-mcp -e ZOTERO_API_KEY=<your-key>
```

For Claude Desktop, every [release](https://github.com/OrgMentem/zotio/releases) ships per-platform [MCPB](https://github.com/modelcontextprotocol/mcpb) bundles — download the `.mcpb` for your platform, double-click it, and Claude Desktop walks you through the install.

The `ZOTERO_API_KEY` is optional for read-only local-desktop use; set it to enable writes and reach group libraries. Full details in [MCP server](mcp-server.md).

## Verify

```bash
zotio version
zotio doctor      # config, credentials, connectivity, cache freshness, writability
```

Next: gate a paper or thesis repo on library health in [CI](ci.md).
