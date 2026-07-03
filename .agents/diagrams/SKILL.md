---
name: diagrams
description: Design system and authoring workflow for the zotio README explainer SVGs in docs/assets/ (architecture, library-health, import-flow, write-safety, vault-roundtrip). Use when creating, editing, reviewing, or regenerating any of these diagrams, when a diagram has a visual defect (text/pill overflow, label collision, arrowhead not meeting a box, commentary that looks like a node), or after a Printing Press reprint may have touched README.md / docs/assets. Covers the shared palette, the node-vs-annotation visual grammar, the standalone-SVG constraints for GitHub, and the render-and-measure verification method.
license: Apache-2.0
compatibility: Assumes a headless browser tool for rendering SVGs and python3 for XML validation.
metadata:
  author: enieuwy
  scope: docs/assets/*.svg
---

# zotio README diagrams — design system & workflow

Five hand-authored SVGs explain zotio in `README.md`. They are **hand-crafted marketing assets**, not generated output — treat them as intentional local content (see "Reprint caution" below). This skill is the memory of how they are built and how to keep them consistent.

## The diagrams

| File | Depicts | Source of truth |
|---|---|---|
| `architecture.svg` | Hybrid routing: reads local (no key), writes split by intent | `internal/cli/write_routing.go`, `create_route.go`; AGENTS.md "Zotero API Surface" |
| `library-health.svg` | `library health` scope → preset → composed checks → CI gate (exit 0/11/12) | `internal/cli/library_health.go` |
| `import-flow.svg` | Reviewable import: scan → resolve → apply over an editable manifest | `internal/cli/import*.go` |
| `write-safety.svg` | Preview-first mutation envelope: command → plan → gates → apply → journal/undo | `internal/mutation/`, `internal/cli/mutate.go` |
| `vault-roundtrip.svg` | Conflict-safe Obsidian/Logseq ⇄ Zotero round-trip | `internal/cli/vault*.go` |

## Visual grammar (the core decision)

In a node-link diagram the **filled rounded-rect (border, often a drop shadow) IS the signifier for "a component/step."** Do not overload it. Every text element falls in exactly one of these registers:

1. **Node** — a real component/step. Solid fill + 1.5–2px border + optional soft drop shadow, `rx≈10–14`. This is the ONLY thing that gets the filled-card treatment.
2. **Edge label** — labels a transition between nodes (`reads`, `writes`, `new items`, `replay → mirror`, `vault sync/push/pull`, `yes`/`no`). Small text; a tiny opaque bg chip only where it crosses a line for legibility. Part of the graph — keep it.
3. **Node-bound badge** — an attribute sitting ON a node (`READ-ONLY`, `PREVIEW-FIRST`, `LIVE`, and bound property chips like `stable finding keys`). A small chip touching/inside its parent card reads as "a property of this node." Legitimate.
4. **Commentary (annotation register)** — the author's voice: theses, guarantees, caveats, side-notes. MUST look distinct from nodes:
   - NO drop shadow, NO solid fill (`fill="none"`), NO solid node border.
   - Optional 1px **dashed** hairline `#cbd5e1` (or same-hue low-opacity) only where a boundary aids grouping.
   - Lead with a `▸ ` marker in `#94a3b8` (or the region accent). Muted/accent text, lighter weight.
5. **Soft callout** — a *deliberately prominent* takeaway (e.g. vault "prose is never clobbered"). Light tint (e.g. `#eefaf3`), no shadow, soft/low-opacity hairline, keep any meaningful icon. Noticeable but clearly not a flow node.

Rule of thumb: if it describes the system rather than *being* the system, it must not wear the node costume.

## Palette (shared across all five — use these exact hex)

- Canvas bg `#fbfcfe` · title `#1e2a44` · body `#33415c` · muted `#64748b`
- READ / local (blue): fill `#e6f0fb` · stroke `#2c6cb0` · text `#173a5e`
- WRITE / cloud (coral): fill `#fde2e2` · stroke `#c0392b` · text `#7a1f16`
- EXTERNAL (amber): fill `#fdf3d8` · stroke `#b8860b` · text `#6b4e0a`
- LOCAL-only / desktop (grey): fill `#eef1f5` · stroke `#64748b` · text `#33415c`
- SAFE / accent (green): fill `#d6f5e3` · stroke `#0aa06e` · text `#0a5f43`
- Brand / arrows (indigo): `#4f46e5` · default connector line `#94a3b8` · annotation hairline `#cbd5e1`

Semantic, not decorative: the desktop connector is **red** because creating an item is a *write*, even though it's local/keyless — locality is a separate axis from read/write.

## Hard constraints (GitHub-safe standalone SVG)

- Valid XML; root `<svg xmlns="http://www.w3.org/2000/svg" viewBox=… width=… height=…>`.
- Draw an opaque background `<rect>` covering the whole viewBox (never rely on page bg; must work in light AND dark GitHub themes).
- Inline styling only: one inline `<style>` block + inline attributes. Font stacks: `ui-sans-serif, -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif` and `ui-monospace, SFMono-Regular, Menlo, monospace`.
- **Forbidden** (GitHub strips them): `<script>`, `<image>`, `<foreignObject>`, `@import`, external fonts, and any `href`/`src`/`xlink:href` pointing at `http(s):`. The only allowed `http` is the `xmlns` namespace literal.
- Every label fits inside its shape with padding; arrowheads land ON the target box edge (use a `<marker>`); no connector line passes through label text.

## Verification — MEASURE, never eyeball

This is the hard-won lesson: eyeballing coordinates fails repeatedly at 1–3px tolerances. Prove every fix.

1. **Render** at `deviceScaleFactor` 2–3, viewport ~1000px, via a headless browser on the `file://` URL. Cache-bust with `?v=N` after every edit so you never view a stale render.
2. **Measure** in the DOM, don't guess:
   - `getBBox()` for each `<text>` vs its container `<rect>` → confirm ≥ ~6px padding, no overflow.
   - point-in-polygon for text inside a `<polygon>` diamond.
   - `getPointAtLength()` sampling of each flow path → confirm the endpoint lands on the target box edge and no sample falls within ~6px of a label rect (catches line-through-text).
3. **Validate**: `python3 -c "import xml.dom.minidom; xml.dom.minidom.parse('docs/assets/<file>.svg')"` and grep that none of `<script` / `<image` / `@import` / `href|src="http` are present (only `xmlns`).
4. Take a final full-diagram screenshot to check overall balance.

Rough text metrics for estimating width: bold uppercase sans ≈ 7px/char @ 11px; monospace ≈ 6.6px/char @ 11px; regular ≈ 6px/char.

## Factual grounding (don't reintroduce old errors)

- **Routing** (`architecture.svg`): item **creation** (+ attachments/PDFs) prefers the **local desktop connector** (`localhost:23119`, no key) — but only on a personal library with the desktop running; group libraries and a closed desktop fall back to the Web API. **All other mutations** (edits, deletes, enrich, tags, moves, `collections` create/update) go to the **Zotero Web API** (key required). **Replay-to-mirror is edits-only** — creates reconcile on the next `sync`. Never draw "all writes → cloud."
- **Rejected overclaims** — do NOT put these back in README or diagrams (unsupported by code): "every Zotero feature in the terminal", "adds 18 features", "no other tool offers", "reading-list = oldest-unread sorted with abstract preview" (it is a `to-read` tag queue with add→start→done).

## Workflow

- Edit one SVG → render → measure → validate → commit **only that file** (the repo may carry unrelated uncommitted work from other sessions; never stage it).
- For a multi-diagram pass, delegate **one agent per file in parallel** (they never collide) and have each render-measure-verify before committing its single file.
- Keep sibling cards uniform in height; when you move a box, re-anchor every connector that touches it.

## Reprint caution

`zotio` is a generated Printing Press CLI. `/printing-press-reprint` (or a regen) can overwrite `README.md` and does not know `docs/assets/*.svg` are intentional. Record the README rewrite + these SVGs in `.printing-press-patches.json` so a reprint reconciles instead of clobbering. Related decisions: the agent skill was renamed `pp-zotero` → `zotio`; the license stays Apache-2.0 (generator default).
