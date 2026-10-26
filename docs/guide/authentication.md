# Authentication

`zotio` is keyless for the common case and only needs a Zotero Web API key when a write can't go through the local desktop connector, or for the few reads that only the Web API serves, such as `items bibliography`.

## Reads — no key for local reads

Local reads go to your Zotero desktop app at `localhost:23119`. No API key is required for these reads while Zotero is running. Web-only reads, such as every `items bibliography` format, need a key and fail without one. Enable the local API once:

**Zotero → Settings → Advanced → "Allow other applications to communicate with Zotero."**

## Connector creates

Creating new items in a personal library can run without a key through the local desktop connector (the channel the browser "Save to Zotero" button uses). The commands whose connector route needs only a running Zotero desktop are `items create`, the `import` commands (`import doi`, `import url`, `import file`, `import pmid`, `import arxiv`, `import isbn`), and `import apply --via connector`. `items new` always needs a Web API key, because it fetches the `/items/new` template from the Web API, which the local API does not serve. `attachments add` needs a key on every route, including `--via connector`, because the route finishes with a Web API write that moves the attachment to its parent.

## Editing writes — key required

Editing writes route to the Zotero Web API and need a key:

- `items update` / `delete` / `move`, `items enrich`
- `tags` mutations, `collections` create/update/move/delete
- `vault push` / `pull` / `resolve`, most of `import apply`

Configure it once:

```bash
printf %s "$ZOTERO_API_KEY" | zotio auth set-token --stdin     # or export ZOTERO_API_KEY=<key>
```

`auth set-token` saves immediately unless you pass `--dry-run`; it does not require `--yes`.
The key lives in `credentials.toml` in the data directory, separate from the config file.
An agentcookie-managed store keeps the key in its config file instead.

Without a key, an apply that needs a Web API mutation fails with a precondition error (exit 9).
The error names `ZOTERO_API_KEY` and `zotio auth set-token`.
Previews that read the local plane and applies that need no mutation can still succeed without a key.
Commands that need Web API reads, including `vault push`, still require a key for those reads during preview.

Generate a key at <https://www.zotero.org/settings/keys>. The first Web API write prints a one-time stderr notice naming the target. A key is also required to read **group libraries** or to read live from the Web API while the desktop app is **closed**; reads from an already synced local mirror (`--data-source local`) need no key.

!!! info "Why writes route to the cloud"
    Zotero's local API is currently GET-only, so mutations are auto-routed to the Web API (which syncs back down to your desktop). The [capabilities reference](../reference/capabilities.md) lists the write target and requirement for every command, and [Safe-by-default writes](../concepts/write-safety.md) covers the mutation engine.

## Check writability

```bash
zotio auth status       # show the credential source; this does not verify the key
zotio doctor           # report Web API writability and desktop connector reachability
zotio auth logout      # preview removal of stored credentials
zotio auth logout --yes # remove stored credentials
```

`auth logout` clears stored credentials; it does not revoke the key at Zotero or unset an environment variable.
Use Zotero's key settings to revoke a key.

The `doctor` report uses these `writes:` values:

- `available (auto-routed to Web API; reads stay local)`
- `unavailable (local API is read-only; set ZOTERO_API_KEY to write via the Web API)`
- `available (Web API)`
- `needs ZOTERO_API_KEY (Web API)`

These values describe Web API writes. The separate connector check describes desktop connector availability.
