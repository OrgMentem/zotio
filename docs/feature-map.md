# zotero-pp-cli — feature map

Grouping = backend / data routing. **`*` = added or changed in the write-safety platform work.**
Core architecture: **reads stay local; writes auto-route to the Zotero Web API** (hybrid routing —
the version read happens locally, the write goes to the cloud, and only with an `api_key`).

```mermaid
flowchart TB
  CLI(["zotero-pp-cli"])

  subgraph DB["Local SQLite mirror - needs sync (READ)"]
    audit["items audit"]
    dupd["items duplicates (detect)"]
    misspdf["items missing-pdf"]
    stale["items stale / unfiled / venues"]
    summ["items summarize = synthesis"]
    rlv["reading-list (view)"]
    taud["tags audit / inventory"]
    stats["library stats"]
    srch["search (local fallback)"]
  end

  subgraph LAPI["Local Zotero API :23119 (READ)"]
    iget["items get / list / children"]
    itl["items tags list"]
    cget["collections get / items / list"]
    sget["searches get / list / run"]
    ann["annotations / fulltext / file"]
  end

  subgraph WEB["Zotero Web API (WRITE - auto-routed; preview by default, --yes to apply)"]
    icrud["items create / update / delete / trash / restore"]
    imove["items move (+ --from / bulk) *"]
    itw["items tags add / remove *"]
    tren["tags rename"]
    tfix["tags audit fix *"]
    enr["items enrich (apply)"]
    dres["items duplicates resolve *"]
    rls["reading-list add / start / done *"]
    smat["searches materialize *"]
    ccrud["collections create / update / delete"]
    imp["import doi / url / file"]
  end

  subgraph EXT["External APIs (READ)"]
    cr["CrossRef"]
    oa["OpenAlex"]
    up["Unpaywall"]
  end

  subgraph LOCAL["Local only - files / desktop / introspection"]
    bun["collections bundle *"]
    wf["workflow run *"]
    vault["vault sync"]
    note["items note-template"]
    open["items open (desktop)"]
    intro["doctor / agent-context / which / version"]
  end

  subgraph GLOB["Global schema endpoints"]
    sch["schema item-types / fields / drift"]
  end

  CLI --> DB
  CLI --> LAPI
  CLI --> WEB
  CLI --> LOCAL
  CLI --> GLOB

  enr -. "lookups" .-> EXT
  imp -. "lookups" .-> EXT
  WEB -. "hybrid: reads version locally, writes to cloud" .-> LAPI
  wf -. "dispatches commands in-process" .-> CLI

  classDef new fill:#d6f5e3,stroke:#0a7,stroke-width:2px,color:#000;
  classDef write fill:#fde2e2,stroke:#c0392b,color:#000;
  classDef read fill:#e6f0fb,stroke:#2c6cb0,color:#000;
  classDef ext fill:#fdf3d8,stroke:#b8860b,color:#000;
  classDef local fill:#eeeeee,stroke:#666666,color:#000;

  class audit,dupd,misspdf,stale,summ,rlv,taud,stats,srch,iget,itl,cget,sget,ann read;
  class icrud,tren,enr,ccrud,imp write;
  class imove,itw,tfix,dres,rls,smat,bun,wf new;
  class cr,oa,up ext;
  class vault,note,open,intro,sch local;
```

## Legend

| Group | Backend | Direction |
|---|---|---|
| **Local SQLite mirror** | synced `data.db` (`sync` first) | read |
| **Local Zotero API** | `localhost:23119` desktop API (local-store fallback) | read |
| **Zotero Web API** | cloud — auto-routed for all mutations | **write** (preview → `--yes`) |
| **External APIs** | CrossRef / OpenAlex / Unpaywall (+ import services) | read |
| **Local only** | files, desktop launch, in-process dispatch, introspection | local |
| **Global schema** | un-prefixed `/itemTypes` etc. | read |

Color coding: **green = new (`*`)**, **red = write**, **blue = read**, **yellow = external**, **grey = local-only**.

**Write-safety platform (`*` + the `WEB` group):** one mutation state machine (`apply = --yes && !--dry-run`,
`--dry-run` wins), split plan/result JSON envelope, write gates (`--max-changes` 500 / 50 under `--agent`,
`--allow-destructive` only for truly-irreversible ops), `--keys-from` bulk selection, fail-fast bulk, and
per-command MCP safety annotations. `--agent` no longer implies `--yes`.

**Validated by:** `gofmt` / `go build` / `go vet` / `go test` (unit tests over `httptest` mock Zotero + real local
SQLite), plus CLI smoke (registration, `workflow run` exit codes, `agent-context`/MCP mirror). **Not** validated
against a live Zotero Web API (offline; mocks only).
