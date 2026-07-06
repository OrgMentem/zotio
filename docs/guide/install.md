# Install

`zotio` comes in three pieces you can install independently: the **CLI** (the engine — everything runs through it), the **agent skill** (drives the CLI inside coding agents), and the **MCP server** (exposes the CLI to MCP hosts). Most people want the CLI; add the skill or MCP server for your agent of choice.

## 1. The CLI — `zotio`

One-shot install of the binary (and the agent skill alongside it):

```bash
npx -y @mvanhorn/printing-press install zotero             # CLI + skill
npx -y @mvanhorn/printing-press install zotero --cli-only  # CLI only
```

Or grab a pre-built binary for your platform from the [latest release](https://github.com/mvanhorn/printing-press-library/releases/tag/zotero-current). On macOS, clear the Gatekeeper quarantine (`xattr -d com.apple.quarantine <binary>`); on Unix, mark it executable (`chmod +x <binary>`). Verify with `zotio --version`.

!!! tip "Enable the local API"
    Reads and keyless item creation talk to your Zotero desktop app at `localhost:23119`. Turn it on once in Zotero: **Settings → Advanced → "Allow other applications to communicate with Zotero."**

## 2. The agent skill

A focused skill that teaches a coding agent to drive the CLI. It auto-installs the CLI on first invocation.

- **Claude Code:** `npx skills add mvanhorn/printing-press-library/cli-skills/zotio -g`
- **Hermes (CLI):** `hermes skills install mvanhorn/printing-press-library/cli-skills/zotio --force`
- **Hermes (in a chat):** `/skills install mvanhorn/printing-press-library/cli-skills/zotio --force`
- **OpenClaw:** tell your agent — *"Install the zotio skill from `https://github.com/mvanhorn/printing-press-library/tree/main/cli-skills/zotio`. The skill defines how its required CLI can be installed."*

See [Use in a coding agent](agent-skill.md) for how to drive it.

## 3. The MCP server — `zotio-mcp`

A separate binary that exposes the CLI to MCP hosts. Install it from the published public-library entry or a pre-built release, then register it:

```bash
# Claude Code
claude mcp add zotero zotio-mcp -e ZOTERO_API_KEY=<your-key>
```

For Claude Desktop, `zotio` ships an [MCPB](https://github.com/modelcontextprotocol/mcpb) manifest (`manifest.json`). When published, download the per-platform `.mcpb` from the [latest release](https://github.com/mvanhorn/printing-press-library/releases/tag/zotero-current), double-click it, and Claude Desktop walks you through the install.

The `ZOTERO_API_KEY` is optional for read-only local-desktop use; set it to enable writes and reach group libraries. Full details in [MCP server](mcp-server.md).

## Verify

```bash
zotio --version
zotio doctor      # config, credentials, connectivity, cache freshness, writability
```
