# zotio field report — 2026-08-22 — papio — arXiv DOIs and DataCite

Source: papio (`/Users/ellis/@dev/papio`), a Go daemon that drives `zotio` as a
subprocess. Reported against installed `zotio 0.19.0` and against committed
`HEAD`. Both findings were re-verified in this repo before any code changed;
this file records what was confirmed, what was corrected, and what was left
alone.

Label discipline: **VERIFIED** means measured or read here, with the command or
the file and line range given. **CORRECTED** means the report's claim did not
survive re-verification as stated. Nothing below is inferred without saying so.

## Finding 1 — `import resolve` could not resolve arXiv DOIs — VERIFIED, FIXED

The report's registry claim is exactly right. Re-measured live on 2026-08-22:

```
GET https://api.crossref.org/works/10.48550/arXiv.2605.10930  -> HTTP 404
GET https://api.datacite.org/dois/10.48550/arXiv.2605.10930   -> HTTP 200
    state "findable", title "Evaluating the False Trust Engendered by LLM
    Explanations", creators Palod / Biswas / Kambhampati,
    types.resourceTypeGeneral "Preprint", url https://arxiv.org/abs/2605.10930
GET https://doi.org/10.48550/arXiv.2605.10930                 -> 302 to
    https://arxiv.org/abs/2605.10930
```

So the DOI was well-formed, registered, and resolvable, and only the chosen
registry was wrong.

### The gap was an inconsistency inside this repo, not a missing feature

zotio already knew this. `items preprint-check` deliberately skips CrossRef for
arXiv self-DOIs, and its test says so in as many words:
`internal/cli/items_preprint_check_test.go:148-159` asserts "CrossRef should not
be queried for arXiv DataCite self DOI". The prefix constant
`arxivSelfDOIPrefix` has been in `internal/cli/items_preprint_check.go:37` all
along. The import path simply never learned it, so two DOI resolution paths in
one binary disagreed about who owns a prefix.

### Scope was wider than reported

The report named `import resolve`. Re-verification found the same defect on
every command that turned a DOI into an item, because they all funnelled
through one CrossRef-only helper:

| command | call site before | blocked on an arXiv DOI |
|---|---|---|
| `import resolve <dir>` | `import_resolve.go:96` | yes |
| `import resolve <manifest>` | `import_resolve.go:118` | yes |
| `import doi` | `import_doi.go:68` | yes |
| `import discover` | `import_discover.go:260` | yes |
| `import scan --resolve` | `import_scan.go:271` | title only |

`import scan --resolve` used a *second* CrossRef-only function
(`crossRefItemFromDOI`), which is why the directory path resolved the item but
still showed no title for it.

### Fix shape: fallback, not prefix routing

The report suggested either. Fallback was chosen and the reasoning is recorded
at the top of `internal/cli/import_datacite.go`: a prefix table fixes arXiv
alone, leaves every other DataCite registrant (Zenodo, Dryad, figshare, OSF,
institutional repositories) failing identically, and goes stale. Falling back
costs one extra request only on the miss path.

Three properties worth knowing as a consumer:

- **A CrossRef outage is not a fallback trigger.** Only HTTP 404/410 — "no such
  record" — routes to DataCite. A timeout or 5xx is returned as-is, because
  CrossRef may well own the DOI and simply be unavailable; retrying at a
  registry that does not own it would convert a truthful transient error into a
  misleading permanent one.
- **When both registries miss, the error names both.** The old single-registry
  message is precisely what sent papio looking for a malformed DOI.
- **arXiv self-DOIs get the arXiv fields.** `archiveID`, `repository` and
  `extra` match `arxivItemFromEntry`, so the same paper imported by DOI and by
  arXiv ID produces the same item. The requested DOI spelling is preserved:
  DataCite normalises the suffix to lower case, and echoing the caller's
  spelling keeps a manifest entry's `identifier` equal to its item's `DOI`.

### Verified after the change

Unit fixtures are trimmed copies of the live responses
(`internal/cli/import_datacite_test.go`). Each test was confirmed to fail with
the fallback disabled, reproducing the reported symptom verbatim:
`status = "unresolved" note = "fetching CrossRef metadata: HTTP 404: Resource
not found."`

Live, against the real registries, with the operator's library untouched:

```
import resolve, 10.48550/arXiv.2605.10930 -> resolved, preprint,
  "Evaluating the False Trust Engendered by LLM Explanations",
  archiveID arXiv:2605.10930, 3 creators
import resolve, 10.1518/hfes.46.1.50_30392 -> resolved, journalArticle,
  "Trust in Automation: Designing for Appropriate Reliance"   (CrossRef, no regression)
import resolve, 10.5061/dryad.8515 -> resolved, dataset, publisher Dryad   (generality)
import resolve, 10.9999/definitely-not-a-real-doi -> unresolved,
  "resolving DOI metadata: fetching CrossRef metadata: HTTP 404 ...; DataCite: HTTP 404 ..."
import doi 10.48550/arXiv.1706.03762 -> preview only, abstract resolved from DataCite
```

### Deliberately not changed

`import translate` / `import url` still resolve a URL-embedded DOI through
CrossRef only (`internal/cli/import_translate.go:87-96`). That path already
falls back to the page's own embedded metadata when CrossRef misses, so an
arXiv URL still yields a typed item and nothing is blocked. Unifying it would
also change a documented fallback order and an asserted error string, so it is
left as a known, non-blocking narrowing rather than folded into this fix.

`items enrich --missing-abstract` likewise stays CrossRef-first with
OpenAlex/Semantic Scholar fallbacks; it is not registry-blocked.

## Finding 2 — "the working tree does not compile" — CORRECTED

The specific symptom no longer exists. `internal/cli/items_bibcheck.go:200` is
`return out` inside `parseLatexCitationOccurrences`, `go build ./...` is clean at
`HEAD`, and the file's last change is commit `153320a`. The report measured
uncommitted work that has since been committed.

The report's underlying warning was still sound, just not for the named reason.
At the time this work started, `go vet ./...` failed on an *uncommitted test*
file (`internal/cli/channel_workflow_test.go`, an unused `testing` import left
behind when its bodies moved to `analytics_test.go`), and during this session
four more uncommitted test files became non-compiling as a concurrent agent
worked on them: `items_move_test.go` (duplicate
`writePlaneTestPatchBodyCollections`), `searches_materialize_test.go` (unused
`srv`), `items_tags_write_test.go` (unused `fmt`), `tags_rename_test.go` (unused
`bytes`). None are this change's files.

So the accurate form of the finding is: **`go build` is clean at HEAD, but
`go test ./internal/cli/...` cannot compile while that concurrent test work is
in flight.** This change was therefore verified in a separate `git worktree` at
`HEAD` carrying only its own files, where `gofmt`, `go vet`, `golangci-lint`,
the full `internal/cli` suite, and `go test -race ./...` are all green.

Seven pre-existing `golangci-lint` findings exist at `HEAD` in files this change
does not touch (`items_enrich_test.go`, `journal_test.go`,
`internal/client/client_test.go`, `internal/cliutil/{paths_test,probe,ratelimit}.go`).
They are not introduced here and are not fixed here.

## Context — the read-envelope break papio absorbed

Acknowledged, no action taken, and the report's own conclusion is endorsed:
`results_array_test.go` pins the convention correctly. The rollout has since
been completed for the remaining record reads and for `analytics`, and a
separate defect found while smoke-testing it — `--compact` emptying rows whose
fields the allow-list does not name, which reduced `--agent journal list` to ten
empty objects — is fixed in the same unreleased block.

The report's closing observation is the useful one for future shape changes: a
consumer decoding into a slice **fails silently**, it does not error loudly.
That is an argument for pinning shapes in tests on the producer side, which is
what `results_array_test.go` now does for every record read.
