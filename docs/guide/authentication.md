# Authentication

`zotio` is keyless for the common case and only needs a Zotero Web API key when a write can't go through the local desktop connector.

## Reads — no key

Reads go to your Zotero desktop app at `localhost:23119`. No API key is required while Zotero is running. Enable the local API once:

**Zotero → Settings → Advanced → "Allow other applications to communicate with Zotero."**

## Creating items — no key

Creating items and saving attachments also works keyless — those go through the same local desktop connector (the channel the browser "Save to Zotero" button uses).

## Editing writes — key required

Editing writes route to the Zotero Web API and need a key:

- `items update` / `delete` / `move`, `items enrich`
- `tags` mutations, `collections` create/update/move/delete
- `vault push` / `pull` / `resolve`, most of `import apply`

Configure it once:

```bash
printf %s "$ZOTERO_API_KEY" | zotio auth set-token --stdin     # or export ZOTERO_API_KEY=<key>
```

Generate a key at <https://www.zotero.org/settings/keys>. The first Web API write prints a one-time stderr notice naming the target. A key is also required to read **group libraries** or to read while the desktop app is **closed**.

!!! info "Why writes route to the cloud"
    Zotero's local API is currently GET-only, so mutations are auto-routed to the Web API (which syncs back down to your desktop). The [capabilities reference](../reference/capabilities.md) lists the write target and requirement for every command, and [Safe-by-default writes](../concepts/write-safety.md) covers the mutation engine.

## Check writability

```bash
zotio doctor      # reports a writes: line — available or read-only
```
