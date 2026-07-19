# Install

`zotio` comes in three pieces you can install independently: the **CLI** (the engine — everything runs through it), the **agent skill** (drives the CLI inside coding agents), and the **MCP server** (exposes the CLI to MCP hosts). Most people want the CLI; add the skill or MCP server for your agent of choice.

!!! tip "Try it before wiring anything"
    `zotio demo` seeds a sandboxed sample library (no Zotero desktop, no API key); `ZOTIO_DEMO=1 zotio <command>` runs any command against it. When you're convinced, `zotio init` sets up the real thing.

## 1. The CLI — `zotio`

=== "macOS"

    **Homebrew** — installs both `zotio` and the `zotio-mcp` MCP server; `brew upgrade` tracks new releases:

    ```bash
    brew install orgmentem/tap/zotio
    ```

=== "Linux"

    **Homebrew** (Linuxbrew) — installs both `zotio` and `zotio-mcp`; `brew upgrade` tracks new releases:

    ```bash
    brew install orgmentem/tap/zotio
    ```

    **Distro packages** — every [GitHub release](https://github.com/OrgMentem/zotio/releases) ships `.deb`, `.rpm`, and `.apk` for amd64/arm64, each bundling both `zotio` and `zotio-mcp`. Download the file for your arch, then:

    ```bash
    # Debian / Ubuntu
    sudo dpkg -i zotio_<version>_linux_amd64.deb

    # Fedora / RHEL / openSUSE
    sudo rpm -i zotio_<version>_linux_amd64.rpm

    # Alpine
    sudo apk add --allow-untrusted zotio_<version>_linux_amd64.apk
    ```

=== "Windows"

    **Scoop** — installs both `zotio` and `zotio-mcp`; `scoop update zotio` tracks new releases:

    ```powershell
    scoop bucket add orgmentem https://github.com/OrgMentem/scoop-bucket
    scoop install zotio
    ```

    !!! note "WinGet is on the way"
        A `winget install OrgMentem.zotio` manifest is pending review in `microsoft/winget-pkgs`. Until it lands, use Scoop or a prebuilt archive.

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
