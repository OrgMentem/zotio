# zotio field report — 2026-08-22 — papio round 2

Source: papio (`/Users/ellis/@dev/papio`), a Go daemon that drives `zotio` as a
subprocess. Reported against installed `zotio 0.19.0` and committed `132435c`.
Every claim below was re-verified in this repo before any code changed.

How this report labels claims:

- **VERIFIED** — the cited file and line range was read in this repo, or in
  `zotero/zotero` / `zotero/dataserver` upstream, or the command was run and its
  output is quoted.
- **INFERRED** — a plausible reading with no citation. Tagged inline.
- **NOT ESTABLISHED** — searched for and not found. Stated plainly so the next
  investigator does not repeat the search.

The two code findings were reproduced without any library: `HOME` pointed at a
temporary directory and `ZOTERO_BASE_URL` at a closed port, so nothing was
reachable. Registry reads (CrossRef, DataCite) are public and read-only.

The design assessment was FIRST written from source alone, with no library
written to. It was then confirmed by a live probe on the operator's own library,
at their explicit instruction — see "Measured live" below. That probe created
three objects and trashed them; it modified nothing that existed beforehand.

Related registers: `dev/field-report-2026-08-22-papio.md` (round 1) and
`dev/field-report-2026-08-22-papio-arxiv.md`.

---

## Finding 1 — a DOI was truncated at a parenthesis. RESOLVED

**The report was right, and the root cause is one line.**

`import_scan.go` decoded staged filenames with a four-entry replacer that
handled `%2F` and the two Unicode division slashes, and nothing else. `%` is
outside `doiScanRE`'s character class
(`10\.\d{4,9}/[A-Za-z0-9._;()/:\-]+`, VERIFIED `import_scan.go:35`), so a
surviving `%28` ended the match early:

| staged filename | matched | outcome |
|---|---|---|
| `10.47205%2Fjdss.2023%284-ii%2934.pdf` | `10.47205/jdss.2023` | 404 at both registries |

VERIFIED live, before the fix, with the built binary:

```
"identifier": "10.47205/jdss.2023",
"note": "resolving DOI metadata: fetching CrossRef metadata: HTTP 404 ...;
         DataCite: HTTP 404 ..."
```

The registry was never the problem. VERIFIED by direct request:

```
GET api.crossref.org/works/10.47205/jdss.2023(4-ii)34  -> 200
    "Extrinsic Motivation And Students’ Academic Achievement: A Correlational Study"
GET api.crossref.org/works/10.47205/jdss.2023          -> 404
```

Parentheses are legal in a DOI suffix, and `doiScanRE` already accepted them —
the character class contains `()`. Only the decode step was short.

**Fix.** `decodeIdentifierFilename` percent-decodes the whole name with
`url.PathUnescape`, keeping the Unicode-slash substitutions. Two properties are
deliberate:

- **An invalid escape falls back to the old narrow `%2F` decoding.** A name
  containing `%zz` is not percent-encoded after all. Decoding it strictly would
  return the name unusable and lose a DOI that used to be found — a regression
  the first draft of this fix actually had, caught by probing
  `10.1000%2Fbad%zz` before committing.
- **The trailing-closer trim is now balance-aware** (`trimUnbalancedClosers`).
  The old `strings.TrimRight(doi, ")]}")` existed for prose like
  `(see 10.1000/foo)`, but applied to `10.1000/ends(x)` it produced the
  unbalanced, non-existent `10.1000/ends(x`. A closer that has a matching
  opener inside the match belongs to the DOI and is kept.

**VERIFIED after the fix**, same binary, same staging directory:

```
"identifier": "10.47205/jdss.2023(4-ii)34",
"status": "resolved",
"title": "Extrinsic Motivation And Students’ Academic Achievement: A Correlational Study"
```

---

## Finding 2 — an arXiv ID in a filename yielded no identifier. RESOLVED,
## and the report's framing needs one correction

**The symptom is real. The stated contrast is a measurement artifact.**

The report contrasts `arxiv-2301.08745.pdf` (no identifier) with
`arxiv-2605.10930.pdf` (resolves), and concludes one of two paths fails to
derive the identifier. VERIFIED: with both files staged and **no DOI in either
body**, both fail identically:

```
arxiv-2301.08745.pdf   classification unidentified, no identifier
arxiv-2605.10930.pdf   classification unidentified, no identifier
```

VERIFIED that the difference was file content, not filename: writing the arXiv
DOI into the body of `arxiv-2605.10930.pdf` and changing nothing else resolves
it to `10.48550/arXiv.2605.10930` with its real title. The sibling case
succeeded through the PDF **content** scan (`extractPDFDOI`'s second and third
stages), not through its name.

So there was never a working filename path and a broken one. There was **no
filename path for arXiv IDs at all**: `extractPDFDOI` looked for DOIs only, and
neither existing arXiv pattern matches a hyphen — `arxivURLPattern` requires
`arxiv.org/abs/`, `arxivExtraPattern` requires a literal `arxiv:` (VERIFIED
`items_preprint_check.go:50-51`).

**Fix.** `arxivSelfDOIFromFilename` derives the paper's DataCite self-DOI,
`10.48550/arXiv.<id>`, from the name. Returning a DOI rather than a bare arXiv
ID means the existing pipeline does the rest unchanged — DataCite resolution
from the round-1 fix, and the arXiv field mapping in `import_datacite.go`. No
second resolution path was added.

The `arxiv` token is required. A bare `2301.08745.pdf` yields nothing:
inventing an identifier from an arbitrary number is worse than reporting none.
A real DOI in the name still wins, since DOI matching runs first.

**VERIFIED after the fix**, live against DataCite:

| staged filename | identifier | item |
|---|---|---|
| `arxiv-2301.08745.pdf` | `10.48550/arXiv.2301.08745` | preprint, "Is ChatGPT A Good Translator? Yes With GPT-4 As The Engine", 6 creators |
| `arxiv-2605.10930v2.pdf` | `10.48550/arXiv.2605.10930` | preprint, 3 creators, version suffix dropped |

Both carry `archiveID: arXiv:<id>`, `repository: arXiv` and `extra: arXiv: <id>`,
identical to an arXiv-ID import.

### The empty note, fixed alongside

The report asked for this and was right to. An `unidentified` entry became
`status: unresolved` with **no note at all**, which reads as "the registry has
no such record" rather than "nothing was extracted from this file".

The cause is worth recording: a `"no identifier"` note already existed, but it
sat under `if entry.Action == "create"`, and `create` is only assigned for
classification `new`, which is only reached **after** the empty-DOI case has
already returned `unidentified` (VERIFIED `import_scan.go:264-278`,
`import_manifest.go:56`). The branch was unreachable. It has been removed and
replaced with a note on the path that actually runs:

```
no DOI or arXiv ID found in the filename or the PDF's content;
import apply hands this file to Zotero's PDF recognizer
```

The second clause is not decoration: `recognize` entries are still actionable,
`import apply` routes them to Zotero's own recognizer (VERIFIED
`import_apply.go:355-362`). An unresolved entry is not a dead end and should not
read like one.

---

## Design question — attaching a file to an item that already exists

**Verdict: the route works. Step 2, the step the report flagged as the open
question, is VERIFIED as supported by Zotero's own server source. The blocker
lies elsewhere, and the sketched three-step shape is right for a reason the
report did not give.**

### Step 2 is accepted, and it moves no bytes

Re-parenting an existing attachment is permitted, with no restriction against
changing one valid parent to another:

- `updateFromJSON` validates the item and calls
  `$item->setSourceKey($json->parentItem)` (VERIFIED `zotero/dataserver`
  `model/Items.inc.php:1655-1700`).
- `validateJSONItem` models both child-to-top-level (`parentItem: false`) and
  top-level-to-child, with no old/new parent equality check (VERIFIED
  `model/Items.inc.php:2008-2031`).
- Attachment save validation checks only the **target** parent — exists, not
  self, must be a regular item. It does not prohibit a change (VERIFIED
  `model/Item.inc.php:1421-1453`, `2000-2032`).
- An explicit re-parent path exists server-side:
  `UPDATE item{$Type}s SET sourceItemID=?` logging old and new source (VERIFIED
  `model/Item.inc.php:2150-2240`).

Critically for a **WebDAV** library, re-parenting cannot strand the file,
because every storage name derives from the attachment's own key and never from
its parent:

- local storage directory: `getStorageDirectory` → `item.key` (VERIFIED
  `zotero/zotero` `chrome/content/zotero/xpcom/attachments.js:2773-2805`)
- upload ZIP: `item.key + '.zip'` (VERIFIED
  `chrome/content/zotero/storage/storageUtilities.js:36-41`)
- WebDAV remote pair: `_getItemURI` → `item.key + '.zip'`,
  `_getItemPropertyURI` → `item.key + '.prop'`, with the `.zip`→`.prop` mapping
  done by extension only (VERIFIED
  `chrome/content/zotero/xpcom/storage/webdav.js:1618-1632`, `1641-1646`)

A parent change writes `sourceItemID`; it does not change `item.key`. So no
local path and neither remote WebDAV name moves. INFERRED, from those three
facts together: the bytes survive a re-parent untouched.

### The sketched three-step shape is correct, for a non-obvious reason

The obvious simplification is to skip the temporary parent: save a **standalone**
top-level attachment, then re-parent it, since `validateJSONItem` explicitly
supports top-level-to-child. That is worse. `POST /connector/saveStandaloneAttachment`
asks Zotero to **recognize** the file into a parent item when it can (VERIFIED
`internal/connector/connector.go:208-243`), so Zotero may create a parent of its
own choosing, and the temporary parent reappears — now outside zotio's control.

`POST /connector/saveAttachment` against a same-session parent involves no
recognizer (VERIFIED `internal/connector/connector.go:168-205`). Deliberately
creating a minimal temporary parent and attaching to it is therefore the route
with the fewest moving parts, which is what the report proposed.

### The real blocker: the connector never returns the created key

Step 2 needs the new attachment's Zotero key. No connector call returns one:

- `SaveAttachment` returns only 201 Created (VERIFIED `connector.go:177-205`).
- `SaveStandaloneAttachment` returns only `canRecognize bool` (VERIFIED
  `connector.go:210,234-242`).
- `GetRecognizedItem` returns only title and item type (VERIFIED
  `connector.go:246-253`).

zotio already faces this for item creates and solves it heuristically:
`confirmConnectorCreate` → `findRecentlyAddedItemKey(title, itemType, createdAfter)`,
documented as best-effort, and it explicitly refuses to guess when more than one
recently added item matches (VERIFIED `create_route.go:229-267`, `276-287`).

An attachment-flavoured equivalent is implementable and would inherit that
ambiguity. The consequence is worse here than for a create: the failure mode of
a wrong match is **attaching a PDF to the wrong paper**, silently. Any
implementation needs the `matched > 1` refusal that `confirmConnectorCreate`
already has, not a best guess.

### Two further constraints

- **Ordering.** Re-parent first, then trash the temporary parent. Trash sets
  only the deleted-item marker and this source shows no server-side child
  cascade (VERIFIED `model/Item.inc.php:1323-1332`); a hard delete nulls
  `sourceItemID` via `ON DELETE SET NULL` rather than deleting children
  (VERIFIED `misc/shard.sql:412-414`). The desktop client is a separate code
  path; it was measured rather than read — see the probe below.
- **Do not loop this per paper.** Round 1 recorded roughly 78 consecutive
  one-item `--via connector` invocations leaving Zotero unresponsive with
  progress windows accumulating and no dismiss route
  (`dev/field-report-2026-08-22-papio.md`). One connector session per repaired
  paper reproduces exactly that. A bulk repair must batch within a session.

### Measured live, 2026-08-22, on the operator's own library

Run at the operator's explicit instruction with `dev/reparent-probe.sh`'s
procedure, driven step by step. Only objects created by the probe were touched:
baseline library version 14985, 1229 top-level items, **trash empty**. Full
`export snapshot` taken first.

| step | result |
|---|---|
| receiver item created | `VJ85KXIM`, v14986, top-level 1229 → 1230 |
| connector create of parent + file | applied 1/1, `via: connector`, **`key: ""`** |
| temp parent / attachment | `MZ3CCEGC` / `FV48BHNC`, `linkMode: imported_url` |
| local storage path | `~/Zotero/storage/FV48BHNC/…pdf`, **no dir under the parent key** |
| after sync | `md5 5ac56ea8…`, `mtime` set, v14989 — file registered with the store |
| **PATCH `{"parentItem":"VJ85KXIM"}`** | **HTTP 204**, v14989 → 14990 |
| after re-parent | `md5` and `mtime` **byte-identical**; storage dir unchanged |
| old parent children | 0. New parent children: 1 |
| **trash the former parent** | attachment `deleted: 0`, parent unchanged, **v still 14990** |
| desktop client after sync | agrees on every point; trash holds only the probe's items |
| end state | top-level back to 1229; nothing pre-existing modified (`?since=14985`) |

VERIFIED, and this settles the assessment's three open points:

1. **Re-parenting an already-stored, already-synced attachment is accepted.**
   HTTP 204, and the desktop adopted the change on the next sync.
2. **The bytes are never touched.** `md5` and `mtime` are identical before and
   after, the local storage directory is still named for the attachment key, and
   no directory appeared under the new parent's key. Nothing was re-uploaded.
3. **Neither the server NOR the desktop client cascades a trash to a child that
   has already been moved away.** Trashing the former parent left the attachment
   at version 14990 — it was not modified at all. This was the one thing the
   source could not answer.

Two side observations worth keeping:

- `import apply --via connector` reported `"key": ""`, confirming from the
  outside what `import_apply.go:277` says: the connector branch returns no key.
  `items create --via connector`, by contrast, DID return the real key, because
  `confirmConnectorCreate` resolves it. So the discovery gap is specific to
  attachments, and is the one piece of new code this route needs.
- The round-1 progress-window hazard reproduced on a **single** invocation:
  Zotero's window count went 1 → 2. It returned to 1 later on its own. One leak
  per call is harmless; 78 of them are what wedged the app.

NOT ESTABLISHED: the WebDAV server's own directory listing was not read. The
password lives in the OS keychain and reading it was out of scope. Evidence for
the remote side is therefore `md5`/`mtime` registration plus the source-verified
`webdav.js` naming, not a directory listing.

### Recommendation

`attachments add` on a WebDAV library is a **missing feature, not an
impossibility** — now measured, not just argued. The report's proposed fallback,
restating the refusal as a permanent property so papio stops retrying, would be
recording a wrong fact. papio should treat it as unsupported-for-now.

What implementing it actually needs, in order:

1. **Attachment-key discovery after a connector save.** The only genuinely
   missing piece. Model it on `confirmConnectorCreate`, and keep that function's
   refusal to guess when more than one candidate matches: here a wrong match
   attaches a PDF to the wrong paper, silently.
2. **A `parentItem` write.** zotio has none today; `items update` sets a title
   and `items move` changes collection membership.
3. **Re-parent strictly before trashing**, and batch within one connector
   session rather than one session per paper.

The mutation itself is proven. It is one guarded PATCH that moves no bytes.
