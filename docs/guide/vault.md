# Obsidian / Logseq vault

`zotio vault` keeps a Markdown vault in step with Zotero — **one note per item**, with `zotero://` backlinks baked in, and an opt-in reverse channel for your own notes. It's the batch, idempotent counterpart to cite-as-you-write plugins: keep those for live writing, let `zotio` own bulk upkeep and note round-trip.

![conflict-safe vault round-trip](../assets/vault-roundtrip.svg)

!!! tip "Two paradigms, by design"
    Live/in-editor plugins (via Better BibTeX RPC) are for writing *right now*. `zotio` is for bulk, headless, version-durable upkeep over the native local API. They write to the same vault without fighting — `zotio` owns its fenced blocks and managed keys, you own everything else.

## Configure once

Set defaults in `~/.config/zotio/config.toml` instead of passing `--out` every run:

```toml
[vault]
root = "~/Vaults/dev"   # ~ is expanded
notes_dir = "Zotero"    # notes land in <root>/<notes_dir>
format = "obsidian"     # or "logseq"
```

`--out` / `--format` flags still override the config.

## `vault sync` — Zotero → vault

Materialize or refresh one Markdown note per item.

```bash
zotio sync                    # refresh the local mirror first
zotio vault sync --dry-run    # preview created / updated / unchanged
zotio vault sync              # write notes (uses [vault] config)
```

- **Identity from the citekey.** Filenames key off `data.citationKey` (native in Zotero 7+), falling back to the item key — lining up with your `[@citekey]` cites.
- **Backlinks baked in.** Frontmatter carries `zotero://select/...`; each annotation links to `zotero://open-pdf/...?annotation=<key>`.
- **Idempotent, non-clobbering.** Re-runs update only the managed frontmatter keys and a fenced annotations block. Your prose and extra frontmatter are preserved verbatim.
- **Scoped.** `--collection`, `--tag`, `--item-type`, `--limit` reuse the local query planner.

## `vault push` / `vault pull` — note write-back

`sync` is one-way. `push` and `pull` add the **opt-in reverse direction** for the user-owned `## Notes` region only — `push` mirrors it to a single tool-owned Zotero child note, `pull` folds remote edits back. Neither touches bibliographic fields.

```bash
zotio vault push --dry-run    # preview Obsidian → Zotero
zotio vault push              # publish notes to a managed Zotero child note
zotio vault pull              # fold edits made in the Zotero app back in
```

- **Reads local, writes cloud.** `push` auto-routes to `api.zotero.org` (needs a key — see [Authentication](authentication.md)). Personal library by default; `--group` prints a one-time visibility warning.
- **One managed child note per item.** First push creates it (with a deterministic write token so a retry can't duplicate it); later pushes PATCH the same note. Its first line is `Obsidian notes — <citekey>` so its origin is obvious in Zotero.
- **Verbatim renderer.** Markdown is reproduced with everything HTML-escaped and nothing interpreted — wikilinks, tables, callouts, and code survive exactly.

!!! warning "Conflicts are never auto-merged"
    A note is pushed only when its `## Notes` region changed *and* the remote note hasn't diverged. If both sides changed, nothing is overwritten — a conflict file lands under `_vault-zotero-conflicts/` and is reported.

## Recover from conflicts

```bash
zotio vault conflicts                       # list unresolved artifacts
zotio vault resolve <citekey> --keep-vault  # republish vault copy over remote
zotio vault resolve <citekey> --keep-remote # pull remote over the vault region
zotio vault resolve <citekey> --recreate    # re-create a note deleted in Zotero
```

## Note format contract

`vault sync` establishes the structure `push` relies on, so run `sync` first.

- **Stable identity.** `zotero_key` / `zotero_library` in frontmatter mean renaming a file or changing a citekey never duplicates or orphans a note.
- **Managed vs user regions.** The tool owns the frontmatter keys and the title/abstract/annotations fences. You own the region between `<!-- zotio:notes-begin -->` and `<!-- zotio:notes-end -->` under `## Notes` — that, and only that, is what `push` sends.
- **Hidden sync state.** Push records its baseline in a single `<!-- zotio:state {...} -->` comment, keeping Obsidian Properties clean.
- **Safe writes.** Vault files are written atomically and only when the on-disk bytes still match what was read — a concurrent Obsidian/iCloud edit is reported (`file_busy`), never clobbered.

## Round-trip in practice

```bash
zotio sync                                    # refresh the local mirror
zotio vault sync                              # Zotero → vault
# ... write under "## Notes" in Obsidian ...
zotio vault push --dry-run                    # preview Obsidian → Zotero
zotio vault push                              # publish
zotio vault pull                              # fold app-side edits back in
zotio vault conflicts                         # if anything conflicted
zotio vault resolve <citekey> --keep-vault    # ...or --keep-remote
```

!!! note "Mental model"
    Obsidian is the editing surface; the Zotero child note is a managed mirror. `pull` folds remote edits back on a clean fast-forward; simultaneous edits on both sides surface as a conflict, never an auto-merge.

See the full command surface in the [command reference](../reference/commands.md#zotio-vault), and the design positioning (why the CLI owns batch, plugins own live) in the repo at `notes/obsidian-positioning.md`.
