# Obsidian / Logseq vault

`zotio vault` keeps a Markdown vault in step with Zotero — **one note per item**, with `zotero://` backlinks baked in, and an opt-in reverse channel for your own notes. It's the batch, idempotent counterpart to cite-as-you-write plugins: keep those for live writing, let `zotio` own bulk upkeep and note round-trip.

<div class="zotio-diagram-wrap">
--8<-- "docs/assets/vault-roundtrip.svg"
</div>

!!! tip "Two paradigms, by design"
    Live/in-editor plugins (via Better BibTeX RPC) are for writing *right now*. `zotio` is for bulk, headless, version-durable upkeep over the native local API. They write to the same vault without fighting — `zotio` owns its fenced blocks and managed keys, you own everything else.

## Configure once

Set defaults in `~/.config/zotio/config.toml` instead of passing `--vault` every run:

```toml
[vault]
root = "~/Vaults/dev"   # ~ is expanded
notes_dir = "Zotero"    # notes land in <root>/<notes_dir>
format = "obsidian"     # or "logseq"
```

`--vault` / `--format` flags still override the config.

## `vault sync` — Zotero → vault

Materialize or refresh one Markdown note per item.

```bash
zotio sync                    # refresh the local mirror first
zotio vault sync --dry-run    # preview would_create / would_update / unchanged
zotio vault sync --yes        # write notes (uses [vault] config)
```

JSON reports use `preview: true` for every preview mode. `dry_run: true` means you explicitly passed `--dry-run`.

- **Identity from the citekey.** Filenames key off `data.citationKey` (native in Zotero 7+), falling back to a non-blank `Citation Key:` line in the item's Extra field, then to the item key (the result is sanitized for use as a filename) — lining up with your `[@citekey]` cites.
- **Backlinks baked in.** Frontmatter carries `zotero://select/...`; each annotation links to `zotero://open-pdf/...?annotation=<key>`.
- **Tags, venue, and related items.** Frontmatter also carries `zotero_tags` (the item's manual and automatic Zotero tags, de-duplicated and sorted), `venue` (`publicationTitle`, else `bookTitle`, else `proceedingsTitle`; the key is left out when the item has none), and `related` (the item's `dc:relation` neighbours in both directions, de-duplicated and sorted). A related item that has a note in the same sync run appears as a `[[note name]]` wikilink; any other related item appears as a `zotero://select/...` link, so a narrower `--scope` turns wikilinks into select links. In a Logseq vault the properties are `zotero-tags::`, `venue::`, and `related::`. The tool never writes `tags`: that key stays yours, and `zotero_tags` is separate so a hand-edited `tags` list is never overwritten.
- **Idempotent, non-clobbering.** Re-runs update only the managed frontmatter keys and the fenced title, abstract, and annotations blocks. Prose outside those fences, your `## Notes` region, and extra frontmatter are preserved verbatim; edits inside a fenced block are overwritten at the next sync.
- **Scoped.** `--collection`, `--tag`, `--item-type`, `--limit` reuse the local query planner. `--scope` selects a cohort with the shared grammar (`collection:KEY`, `tag:NAME`, `item:KEY`, `query:TEXT`, `saved-search:KEY`), and arms joined with ` AND ` select only the items in every arm, for example `--scope 'collection:ABCD1234 AND tag:include'`. `--scope` cannot be combined with `--item-type`, and it is refused with `--collection` or `--tag` when they select a different cohort than `--scope`; a `--collection` or `--tag` that names one of the AND arms agrees with the scope.

## `vault push` / `vault pull` — note write-back

`sync` is one-way. `push` and `pull` add the **opt-in reverse direction** for the user-owned `## Notes` region only — `push` mirrors it to a single tool-owned Zotero child note, `pull` folds remote edits back. Neither touches bibliographic fields.

```bash
zotio vault push --dry-run    # preview Obsidian → Zotero
zotio vault push --yes        # publish notes to a managed Zotero child note
zotio vault pull --yes        # fold edits made in the Zotero app back in
```

- **Vault reads local, note reads and writes cloud.** Vault files and ordinary library reads are local. `push` resolves the Web write target (`api.zotero.org`, needs a key — see [Authentication](authentication.md)) and reads the managed note's version and content from it in both preview and apply, so a preview also needs the key and network. Personal library by default; `--group` prints a one-time visibility warning.
- **One managed child note per item.** First push creates it (with a deterministic write token so a retry can't duplicate it); later pushes PATCH the same note. Its first line is `Obsidian notes — <citekey>` so its origin is obvious in Zotero.
- **Verbatim renderer.** Markdown is reproduced as uninterpreted text with everything HTML-escaped — wikilinks, tables, callouts, and code are not rendered or rewritten. Blocks are split on blank lines, so runs of empty or whitespace-only lines collapse to a single paragraph break, including inside fenced code.

!!! warning "Conflicts are never auto-merged"
    A note is pushed only when its `## Notes` region changed *and* the remote note hasn't diverged. If both sides changed, nothing is overwritten — a conflict file lands under `_vault-zotero-conflicts/` and is reported. A preview reports it as `would_conflict`; conflicts count as issues and exit 1 in preview and apply alike.

## Audit before and after a round-trip

`vault audit` checks managed notes without changing files or Zotero:

```bash
zotio library health --for vault
zotio vault audit
zotio vault audit --vault /path/to/vault/Zotero
```

The audit reports five issue codes in `counts` and `findings[].evidence.issue`.
Each issue has a corresponding finding kind:

| Issue code | Finding kind | Meaning |
| --- | --- | --- |
| `orphaned` | `vault_orphan` | The note's Zotero item no longer exists in the local store. |
| `stale` | `vault_stale` | The note has fallen behind the local store. |
| `needs_notes_boundary` | `vault_needs_notes_boundary` | The managed notes markers are absent or invalid. |
| `upgradeable` | `vault_upgradeable` | The note has a recognized Zotero selection link but no `zotero_key` frontmatter. |
| `unparseable_state` | `vault_issue` | The audit cannot parse the saved state comment. |

Refresh the mirror with `zotio sync` before comparing notes with current Zotero data.
Use `library health --for vault` to find missing or duplicate citekeys before they cause note identity problems.
Run `vault audit` again after the round-trip.

## Recover from conflicts

```bash
zotio vault conflicts                       # list unresolved artifacts
zotio vault resolve <citekey> --keep-vault --allow-destructive --yes  # republish vault copy over remote
zotio vault resolve <citekey> --keep-remote --allow-destructive --yes # pull remote over the vault region
zotio vault resolve <citekey> --recreate --yes    # re-create a note deleted in Zotero
```

`--keep-vault` and `--keep-remote` overwrite one side, so they need `--allow-destructive` to apply. Without it the preview prints `ok:false` with `destructive_opt_in_required` and exits non-zero. Previews also evaluate `--max-changes` like apply.

## Note format contract

`vault sync` establishes the structure `push` relies on, so run `sync` first.

- **Stable identity.** `zotero_key` / `zotero_library` in frontmatter mean renaming a file or changing a citekey never duplicates or orphans a note.
- **Managed vs user regions.** The tool owns the frontmatter keys (`title`, `authors`, `year`, `itemType`, `DOI`, `url`, `citekey`, `zotero_key`, `zotero`, `collections`, `collection_names`, `zotero_tags`, `venue`, `related`, `zotero_library`) and the title/abstract/annotations fences; a Logseq vault gets the equivalent `name::` properties (for example `zotero-key::`, `zotero-tags::`). A managed key is rewritten on every sync, and a managed key with nothing to show (an item with no venue) is removed rather than left stale. You own the region between `<!-- zotio:notes-begin -->` and `<!-- zotio:notes-end -->` under `## Notes` — that, and only that, is what `push` sends. Any other key, including `tags`, is yours and survives sync.
- **Hidden sync state.** Push records its baseline in a single `<!-- zotio:state {...} -->` comment, keeping Obsidian Properties clean.
- **Safe writes.** Vault files are written atomically and only when the on-disk bytes still match what was read — a concurrent Obsidian/iCloud edit detected at that check is reported (`file_busy`) instead of overwritten. Publication is an atomic rename, but the filesystem has no portable compare-and-swap, so an edit that lands between the final check and the rename can still be replaced. If you need strict preservation, avoid editing the vault while a write runs.

## Round-trip in practice

```bash
zotio sync                                    # refresh the local mirror
zotio vault sync --yes                         # Zotero → vault
# ... write under "## Notes" in Obsidian ...
zotio vault push --dry-run                    # preview Obsidian → Zotero
zotio vault push --yes                        # publish
zotio vault pull --yes                        # fold app-side edits back in
zotio vault conflicts                         # if anything conflicted
zotio vault resolve <citekey> --keep-vault --allow-destructive --yes    # ...or --keep-remote
```

!!! note "Mental model"
    Obsidian is the editing surface; the Zotero child note is a managed mirror. `pull` folds remote edits back on a clean fast-forward; simultaneous edits on both sides surface as a conflict, never an auto-merge.

See the full command surface in the [command reference](../reference/commands.md#zotio-vault), and the design positioning (why the CLI owns batch, plugins own live) in the repo at `dev/obsidian-positioning.md`.
