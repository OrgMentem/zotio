# Install

`zotio` comes in three pieces you can install independently: the **CLI** (the engine — everything runs through it), the **agent skill** (drives the CLI inside coding agents), and the **MCP server** (exposes the CLI to MCP hosts). Most people want the CLI; add the skill or MCP server for your agent of choice.

## 1. The CLI — `zotio`

**Homebrew (macOS / Linux):**

```bash
brew install orgmentem/tap/zotio
```

This installs both `zotio` and the `zotio-mcp` MCP server; `brew upgrade` tracks new releases.

**Prebuilt binaries:** every [GitHub release](https://github.com/OrgMentem/zotio/releases) ships archives for macOS, Linux, and Windows (amd64/arm64) with cosign-signed checksums and SBOMs. Unpack and put `zotio` on your `PATH`; on macOS clear the Gatekeeper quarantine (`xattr -d com.apple.quarantine zotio`), on Unix `chmod +x zotio`.

**From source:**

```bash
git clone https://github.com/OrgMentem/zotio && cd zotio
go build -o zotio ./cmd/zotio
```

Then let the CLI walk you through setup — Zotero detection, the local-API toggle, an optional Web API key, first sync, and a health check:

```bash
zotio init
```

!!! tip "Enable the local API"
    Reads and keyless item creation talk to your Zotero desktop app at `localhost:23119`. Turn it on once in Zotero: **Settings → Advanced → "Allow other applications to communicate with Zotero."** (`zotio init` walks you through this.)

## 2. The agent skill

A focused skill — bundled in the repo as [`SKILL.md`](https://github.com/OrgMentem/zotio/blob/main/SKILL.md) — that teaches a coding agent to drive the CLI directly (the most efficient path; no MCP server in the middle).

- **Claude Code:** copy `SKILL.md` into `~/.claude/skills/zotio/SKILL.md` (or your project's `.claude/skills/zotio/`).
- **Any other agent:** point it at the raw file — `https://raw.githubusercontent.com/OrgMentem/zotio/main/SKILL.md` — or paste it into your agent's skill store.

See [Use in a coding agent](agent-skill.md) for how to drive it.

## 3. The MCP server — `zotio-mcp`

`zotio-mcp` ships alongside the CLI — the Homebrew formula and every release archive include both binaries. Register it:

```bash
# Claude Code
claude mcp add zotero zotio-mcp -e ZOTERO_API_KEY=<your-key>
```

For Claude Desktop, `zotio` ships an [MCPB](https://github.com/modelcontextprotocol/mcpb) manifest (`manifest.json`) — the standard one-click extension format; `.mcpb` bundles are produced at publish time.

The `ZOTERO_API_KEY` is optional for read-only local-desktop use; set it to enable writes and reach group libraries. Full details in [MCP server](mcp-server.md).

## Verify

```bash
zotio version
zotio doctor      # config, credentials, connectivity, cache freshness, writability
```
