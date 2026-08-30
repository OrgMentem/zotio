# zotio field report — 2026-08-30 — vault push read the version map from a stale cache

The fourth report of the day, and the one that closes the last write path with no live evidence at
all. `vault push` was marked NOT ATTEMPTED in all three earlier reports for the same reason each
time: no vault is configured on this machine.

Source: this repo at `6e22eba` plus the fix this report produced. Target: the operator's real
personal library (`users/5847066`), writes routing to `api.zotero.org`. Vault: a **throwaway**
directory at `/tmp/zb-vault` holding one note, deliberately not the operator's Obsidian vault.

Every claim is labelled **OBSERVED** — measured against that library, with `curl` as an independent
oracle.

---

## The defect

`fetchNoteVersions` fetched the batch key-to-version map through `c.Get`, the **cached** read path:

```go
params := map[string]string{"itemKey": …, "format": "versions"}
data, err := c.Get("/items", params)
```

`internal/client/client.go` states the rule for the version-bearing reads outright — *"Bypasses the
read cache so the caller always observes a live value"* — and `getNote`, twenty-five lines above in
the same file, obeys it with `GetWithVersion`. Same file, same concern, two different answers. A
batch read simply had no version-bearing helper to reach for, so the cached one got used.

Absence from that map is how `vault push` decides a note was deleted remotely
(`vault_push.go:232`), and every present version becomes an
`If-Unmodified-Since-Version` precondition. Caching it for five minutes makes both wrong.

## Reproduced live, A/B on the same library — OBSERVED

The two binaries differ by one line. The library state, the vault file, and the on-disk cache entry
were identical for both runs.

| step | action | observed |
| --- | --- | --- |
| 1 | pre-fix binary, no-op push | `1 unchanged`; **1 cache entry written** |
| 2 | `DELETE /items/8NNXQI4G` by `curl`, version 15153 | `HTTP 204`; the note then reads `HTTP 404` |
| 3 | pre-fix binary, push again | **`1 unchanged`** |
| 4 | fixed binary, same cache, same deleted note | **`1 remote_deleted`** |

Step 3 is the defect. The managed note was **confirmed gone by an independent oracle**, and the tool
reported the vault note as synchronised, exit 0. Silent divergence: the operator is told everything
is in order while the Zotero side of the pair no longer exists.

Step 4 is the fix, on byte-identical state:

```
Pushed notes from /tmp/zb-vault: 1 remote_deleted
  [remote_deleted] zbProbe2026.md — child note missing remotely;
  run vault resolve 'zbProbe2026' --recreate to re-create
Error: vault push: 1 note failures; results incomplete
```

Correct classification, an actionable next step naming the exact command, and a non-zero exit so a
scripted caller notices.

**Why the cache was genuinely live, not assumed.** `newClient` sets `c.NoCache = f.noCache`, false
by default, so `vault push` caches like any other command. Step 1 wrote exactly one cache entry
under `~/.cache/zotio`; after the fix that entry is never created, because the version map no longer
goes through the cached path. The empty cache directory is the fix working, not the cache being
disabled — which is why this had to be measured A/B rather than by inspecting one binary.

**Why a mutation does not save you.** A write invalidates the whole cache
(`invalidateCache` does `os.RemoveAll(cacheDir)`), so the stale window opens precisely on the runs
that write nothing — the idempotent no-op re-runs that a vault workflow performs most often. The
remote change in step 2 came from outside zotio, as a real one would: another device syncing, or
Zotero itself.

## The fix

`fetchNoteVersions` now reads through `GetWithVersion` and discards the header version, which
describes the whole library rather than any one note. The doc comment states why the bypass is
mandatory, so the next reader does not tidy it back to `Get`.

Four tests. `TestFetchNoteVersionsObservesLiveVersions` is the regression test and is
negative-controlled: reverting the one line fails it with *"the version map was served from the
response cache, so a remote edit is invisible to the push diff"*, and the server is asked once
instead of twice. Every other `vault push` test sets `NoCache = true`, which is exactly why this
went unnoticed; the new tests enable the cache and isolate it with `t.Setenv("HOME", …)`.

## Revert detector — PASSED

| field | baseline | final |
| --- | --- | --- |
| items | 3224 | 3224 |
| trash rows | 39 | 39 |
| `key_tags_sha256` | `96ea73e870d92c0e…` | `96ea73e870d92c0e…` |
| library version | 15152 | 15154 |

One note created and permanently deleted; only `library_version` moved. A search of every live item
for the probe citekey returns nothing.

## Not closed by this run

* **The conflict path.** This exercised create, no-op, and `remote_deleted`. A divergent remote body
  producing a conflict artifact under `_vault-zotero-conflicts/` was not exercised live.
* **A real Obsidian vault.** The rehearsal used a throwaway directory with one note. Behavior
  against a populated vault with `[vault].root` configured is unmeasured, and pushing into the
  operator's own notes needs their explicit go-ahead rather than being inferred from a batch
  instruction.
* **`vault pull` and `vault audit`.** Untouched by this fix and still unverified live.
