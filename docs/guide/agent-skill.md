# Use in a coding agent

The agent skill teaches a coding agent to drive `zotio` directly — the most efficient path, with no MCP server in the middle. Install it as shown in [Install](install.md#2-the-agent-skill).

## Invoke it

In Claude Code (or another supported agent), invoke the skill with your goal:

```
/zotio export a week of highlights from my "LLM eval" collection as markdown
```

The skill drives the CLI directly, choosing commands and flags for you.

## How agents discover the surface at runtime

`zotio` is built to be introspected, so an agent never has to hard-code a command list:

```bash
zotio agent-context --pretty     # structured JSON: commands, flags, auth
zotio which "stale tickets"      # resolve a natural-language goal to a command
zotio capabilities               # read/write classification + preconditions
zotio <command> --help           # per-command help
```

Add `--agent` to any command for agent-friendly defaults — JSON output, compact fields, no color, and non-interactive prompts. Mutating commands still preview unless you pass `--yes`:

```bash
zotio items audit --agent
zotio tags audit fix --agent --yes   # apply the merge
```

## Safety for automated callers

- Reads are safe and keyless. Writes preview by default; `--agent` does **not** auto-apply them.
- `--max-changes` caps how many operations a single mutation may apply (lower under `--agent`).
- `--dry-run` shows the request without sending it.

See [Safe-by-default writes](../concepts/write-safety.md) for the full mutation contract and the [command reference](../reference/commands.md) for every flag.
