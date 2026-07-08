# CI for your bibliography

Gate a paper, thesis, or review repository on the health of its Zotero library — and wear the result as a live badge.

The building blocks are all in the CLI: `library health` exits `11` when findings meet your severity bar (`--fail-on`), `12` when the local mirror is stale (`--require-fresh`), and `9` when a required precondition is missing — and `--badge` emits [shields.io endpoint](https://shields.io/badges/endpoint-badge) JSON. CI is just those exit codes wired to a workflow.

## The GitHub Action

[`zotio: bibliography health for Zotero`](https://github.com/marketplace/actions/zotio-bibliography-health-for-zotero) packages install → sync → gate:

```yaml
# .github/workflows/bibliography.yml
name: Bibliography health

on:
  pull_request:
  push:
    branches: [main]
  schedule:
    - cron: "0 6 * * *"   # your library drifts outside git — check daily

permissions:
  contents: read

jobs:
  bibliography:
    runs-on: ubuntu-latest
    steps:
      - uses: OrgMentem/zotio-action@v1
        with:
          api-key: ${{ secrets.ZOTERO_API_KEY }}
          for: citation
          fail-on: high
          check-retractions: "true"
          badge-path: badge.json
```

**Why the cron matters:** a normal CI gate runs when the repo changes — but your Zotero library changes when you import a paper, with no push and no PR. The schedule catches library drift (and freshly issued retractions) within a day instead of at submission time. The action also writes a verdict table to the job's step summary and exposes `exit-code`/`message`/`color` outputs for downstream steps.

Create a Zotero API key with **read access** at [zotero.org/settings/keys](https://www.zotero.org/settings/keys) and store it as the `ZOTERO_API_KEY` repository secret — CI runners have no Zotero desktop, so the Web API key is required there. The action verifies the key up front, masks it in logs, and resolves your user ID automatically.

`check-retractions: "true"` extends the gate to **retracted papers**, via Crossref's Retraction Watch data.

## Gate on what changed

Legacy libraries often have standing findings that take time to unwind. An absolute `fail-on: high` gate is useful for greenfield repositories, but on older libraries it trains everyone to ignore a permanently red banner while new problems slip in beside known debt.

Use a cached baseline instead: the first run records today's library, and later runs fail only when the PR or overnight library drift introduces new high-or-worse findings.

```yaml
      - uses: OrgMentem/zotio-action@v1
        with:
          api-key: ${{ secrets.ZOTERO_API_KEY }}
          for: citation
          fail-on: none
          fail-on-new: high
          baseline: "true"
```

With the default cache enabled, that recipe is quiet when nothing changed and loud when a new finding appears; the badge reads `no new findings` in bright green even if the legacy library still has old findings below the baseline. The action can also post the sticky PR comment, open a deduplicated issue on new retractions, check a manuscript via `manuscript`, publish a shields endpoint to a gist, and persist the zotio store cache between runs.


## Gate the manuscript too

The library gate catches bad references; `items bibcheck` catches references your manuscript uses that the library can't back:

```yaml
      - run: zotio items bibcheck thesis.tex --fail-on-unknown   # exit 11 on unknown/ambiguous citekeys
```

![zotio items bibcheck resolving manuscript citekeys](../assets/demos/bibcheck.gif)

## Publish the badge

`badge-path` writes shields endpoint JSON — even when the gate fails, so the badge never lies. Publish it anywhere shields can reach (GitHub Pages, a gist, any static host) and embed:

```markdown
![bibliography](https://img.shields.io/endpoint?url=https://<you>.github.io/<repo>/badge.json)
```

This site does exactly that: the zotio README badge is regenerated from the maintainer's real library on every docs deploy.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Gate passed. |
| `9` | Setup/precondition failure (missing key, unmet check requirement). |
| `11` | Quality gate failed — findings at or above `fail-on`. |
| `12` | Freshness gate failed (`--require-fresh`). |

## Without the action

Any CI system works — the action is convenience, not magic:

```bash
brew install orgmentem/tap/zotio          # or download a release binary
export ZOTERO_API_KEY=...                 # and ZOTERO_BASE_URL=https://api.zotero.org/users/<id>
zotio sync
baseline="${HOME}/.local/share/zotio/health-baseline.json"
zotio library health --for citation \
  --fail-on none \
  --baseline "$baseline" \
  --write-baseline "$baseline" \
  --fail-on-new high \
  --report health-report.json \
  --badge > badge.json
```
