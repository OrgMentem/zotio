// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// journal undo for `items enrich`: the provenance line an enrich write appends
// to Extra has to come back out when the run is undone.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"zotio/internal/mutation"
)

// enrichUndoPlane is a stateful stand-in for the Zotero write plane. It holds
// one item, K1, enforces the If-Unmodified-Since-Version precondition, and
// folds every PATCH body into the item, so a test can apply a run, undo it, and
// compare the item's bytes before and after.
type enrichUndoPlane struct {
	srv *httptest.Server

	mu      sync.Mutex
	version int
	data    map[string]any
	patches []map[string]any
}

// newEnrichUndoPlane isolates HOME (the journal lives under it) and points the
// CLI at a plane whose item K1 carries extra as its Extra field.
func newEnrichUndoPlane(t *testing.T, extra string) *enrichUndoPlane {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_API_KEY", "")
	p := &enrichUndoPlane{version: 40, data: map[string]any{
		"key": "K1", "itemType": "journalArticle",
		"DOI": "", "abstractNote": "", "volume": "", "pages": "",
		"extra": extra,
		"tags":  []any{map[string]any{"tag": "typed-by-hand"}},
	}}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	t.Setenv("ZOTERO_BASE_URL", p.srv.URL)
	return p
}

func (p *enrichUndoPlane) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.URL.Path != "/items/K1" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Last-Modified-Version", strconv.Itoa(p.version))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"key": "K1", "version": p.version, "data": p.data})
	case http.MethodPatch:
		if r.Header.Get("If-Unmodified-Since-Version") != strconv.Itoa(p.version) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p.patches = append(p.patches, body)
		for k, v := range body {
			p.data[k] = v
		}
		p.version++
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (p *enrichUndoPlane) field(name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, _ := p.data[name].(string)
	return s
}

// edit stands in for the user changing the item after the run.
func (p *enrichUndoPlane) edit(name string, value any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.data[name] = value
	p.version++
}

func (p *enrichUndoPlane) tagNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var names []string
	for _, raw := range p.data["tags"].([]any) {
		names = append(names, raw.(map[string]any)["tag"].(string))
	}
	slices.Sort(names)
	return names
}

func (p *enrichUndoPlane) patchCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.patches)
}

func (p *enrichUndoPlane) lastPatch() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.patches[len(p.patches)-1]
}

// applyEnrichJournaled applies proposals through the same planning path
// `items enrich --yes` uses, with the journal recorder installed, and returns
// the run id the journal assigned.
func applyEnrichJournaled(t *testing.T, plane *enrichUndoPlane, proposals ...enrichProposal) string {
	t.Helper()
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })
	c := newEnrichWriteClient(t, plane.srv.URL)
	ops := enrichPlannedOps(context.Background(), proposals, c, &rootFlags{}, http.DefaultClient)
	env, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "items.enrich", ops)
	if err != nil || !env.OK || env.Result == nil || env.Result.Summary.Applied != len(proposals) {
		t.Fatalf("enrich apply: err=%v env=%+v, want %d applied", err, env, len(proposals))
	}
	journal, _ := env.Journal.(map[string]any)
	runID, _ := journal["run_id"].(string)
	if runID == "" {
		t.Fatalf("enrich apply recorded no journal run: journal=%v", env.Journal)
	}
	return runID
}

func undoJournalRun(t *testing.T, runID string) (mutation.Envelope, error) {
	t.Helper()
	cmd := newJournalCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"undo", runID})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	var env mutation.Envelope
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode undo envelope: %v; out=%s", decodeErr, out.String())
		}
	}
	return env, err
}

func mustUndoJournalRun(t *testing.T, runID string) mutation.Envelope {
	t.Helper()
	env, err := undoJournalRun(t, runID)
	if err != nil {
		t.Fatalf("undo %s: %v; env=%+v", runID, err, env)
	}
	return env
}

func enrichUndoDOIProposal() enrichProposal {
	return enrichProposal{
		Key: "K1", Category: "missing_doi", Action: enrichActionPatch,
		Source: "CrossRef", Fields: map[string]any{"DOI": "10.1/attention"},
	}
}

// Every enrich category that writes provenance, driven through apply and undo
// against Extra values that are empty, hold one prior line, and hold several.
// Undo must leave Extra byte-identical to what it was before the apply, and the
// tags or fields back as they were, in ONE version-guarded PATCH.
func TestJournalUndoRestoresEnrichProvenanceExtraByteForByte(t *testing.T) {
	categories := []struct {
		name     string
		proposal enrichProposal
		fields   map[string]string // field -> value the apply writes into a blank field
		tags     []string          // tags the apply adds
	}{
		{
			name:     "missing_doi",
			proposal: enrichUndoDOIProposal(),
			fields:   map[string]string{"DOI": "10.1/attention"},
		},
		{
			name: "missing_abstract",
			proposal: enrichProposal{
				Key: "K1", Category: "missing_abstract", Action: enrichActionPatch,
				Source: "CrossRef", Fields: map[string]any{"abstractNote": "An abstract."},
			},
			fields: map[string]string{"abstractNote": "An abstract."},
		},
		{
			name: "missing_citation",
			proposal: enrichProposal{
				Key: "K1", Category: "missing_citation", Action: enrichActionPatch,
				Source: "CrossRef", Fields: map[string]any{"volume": "7", "pages": "1-9"},
			},
			fields: map[string]string{"volume": "7", "pages": "1-9"},
		},
		{
			name: "missing_subjects",
			proposal: enrichProposal{
				Key: "K1", Category: "missing_subjects", Action: enrichActionTag,
				Source: "OpenAlex", Tags: []string{"concept/Biology", "concept/Computer science"},
			},
			tags: []string{"concept/Biology", "concept/Computer science"},
		},
		{
			name: "missing_mesh",
			proposal: enrichProposal{
				Key: "K1", Category: "missing_mesh", Action: enrichActionTag,
				Source: "PubMed (PMID 34942586)", Tags: []string{"mesh/Humans", "mesh/Neoplasms"},
			},
			tags: []string{"mesh/Humans", "mesh/Neoplasms"},
		},
	}
	extras := []struct{ name, extra string }{
		{"empty", ""},
		{"one prior line", "Citation Key: smith2020"},
		{"several prior lines", "Citation Key: smith2020\nPMID: 34942586\nlive user note"},
	}
	for _, category := range categories {
		for _, tc := range extras {
			t.Run(category.name+"/"+tc.name, func(t *testing.T) {
				plane := newEnrichUndoPlane(t, tc.extra)
				runID := applyEnrichJournaled(t, plane, category.proposal)

				applied := plane.field("extra")
				lines := strings.Split(applied, "\n")
				if !strings.HasPrefix(lines[len(lines)-1], "zotio: ") || !strings.HasPrefix(applied, tc.extra) {
					t.Fatalf("applied extra = %q, want %q followed by a zotio provenance line", applied, tc.extra)
				}
				for field, want := range category.fields {
					if got := plane.field(field); got != want {
						t.Fatalf("applied %s = %q, want %q", field, got, want)
					}
				}
				if len(category.tags) > 0 {
					wantTags := slices.Sorted(slices.Values(append([]string{"typed-by-hand"}, category.tags...)))
					if got := plane.tagNames(); !slices.Equal(got, wantTags) {
						t.Fatalf("applied tags = %v, want %v: the proposed tags added", got, wantTags)
					}
				}

				env := mustUndoJournalRun(t, runID)
				if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 {
					t.Fatalf("undo env = %+v, want one applied reversal", env)
				}
				if got := plane.field("extra"); got != tc.extra {
					t.Fatalf("extra after undo = %q, want the pre-apply %q", got, tc.extra)
				}
				for field := range category.fields {
					if got := plane.field(field); got != "" {
						t.Fatalf("%s after undo = %q, want it blank again", field, got)
					}
				}
				if got := plane.tagNames(); !slices.Equal(got, []string{"typed-by-hand"}) {
					t.Fatalf("tags after undo = %v, want only the hand-typed tag", got)
				}
				if plane.patchCount() != 2 {
					t.Fatalf("PATCH count = %d, want 2 (the apply and ONE undo write)", plane.patchCount())
				}
			})
		}
	}
}

// The user keeps working on the item after the run. Undo removes the one
// provenance line and nothing else, wherever the user's lines now sit.
func TestJournalUndoEnrichRemovesOnlyTheProvenanceLineAfterUserEdits(t *testing.T) {
	const pre = "Citation Key: smith2020"
	plane := newEnrichUndoPlane(t, pre)
	runID := applyEnrichJournaled(t, plane, enrichUndoDOIProposal())

	plane.edit("extra", "top note\n"+plane.field("extra")+"\nbottom note")
	mustUndoJournalRun(t, runID)

	if got, want := plane.field("extra"), "top note\n"+pre+"\nbottom note"; got != want {
		t.Fatalf("extra after undo = %q, want %q", got, want)
	}
	if got := plane.field("DOI"); got != "" {
		t.Fatalf("DOI after undo = %q, want it blank again", got)
	}
}

// The user rewrote Extra and the line is gone. Undo still reverses the field,
// leaves Extra alone, and does not send Extra at all.
func TestJournalUndoEnrichLeavesExtraWhenTheLineIsGone(t *testing.T) {
	plane := newEnrichUndoPlane(t, "Citation Key: smith2020")
	runID := applyEnrichJournaled(t, plane, enrichUndoDOIProposal())

	plane.edit("extra", "rewritten by hand")
	env := mustUndoJournalRun(t, runID)

	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("undo env = %+v, want the DOI reversal applied despite the missing line", env)
	}
	if got := plane.field("extra"); got != "rewritten by hand" {
		t.Fatalf("extra after undo = %q, want the user's rewrite untouched", got)
	}
	if _, sent := plane.lastPatch()["extra"]; sent {
		t.Fatalf("undo PATCH = %v, want no extra key when the line is gone", plane.lastPatch())
	}
	if got := plane.field("DOI"); got != "" {
		t.Fatalf("DOI after undo = %q, want it blank again", got)
	}
}

// Nothing is left to reverse once the user has already removed the tags and the
// line: the undo is a no-op, not a failure and not a write.
func TestJournalUndoEnrichIsANoOpWhenAlreadyReversedByHand(t *testing.T) {
	plane := newEnrichUndoPlane(t, "")
	runID := applyEnrichJournaled(t, plane, enrichProposal{
		Key: "K1", Category: "missing_mesh", Action: enrichActionTag,
		Source: "PubMed (PMID 1)", Tags: []string{"mesh/Humans"},
	})
	plane.edit("tags", []any{})
	plane.edit("extra", "")
	before := plane.patchCount()

	env := mustUndoJournalRun(t, runID)
	if !env.OK || env.Result == nil || env.Result.Summary.NoOp != 1 {
		t.Fatalf("undo env = %+v, want one no-op", env)
	}
	if plane.patchCount() != before {
		t.Fatalf("PATCH count = %d, want %d: nothing to write", plane.patchCount(), before)
	}
}

// Apply read the live item, not the mirror. A field the mirror thought blank
// but the live item already held goes back to the live value, and a field the
// user changed after the run keeps the user's value.
func TestJournalUndoEnrichRestoresLivePriorFieldValue(t *testing.T) {
	t.Run("restores what the live item held", func(t *testing.T) {
		plane := newEnrichUndoPlane(t, "")
		plane.data["DOI"] = "10.9/live-prior"
		runID := applyEnrichJournaled(t, plane, enrichUndoDOIProposal())
		if got := plane.field("DOI"); got != "10.1/attention" {
			t.Fatalf("applied DOI = %q, want the proposed DOI", got)
		}

		mustUndoJournalRun(t, runID)
		if got := plane.field("DOI"); got != "10.9/live-prior" {
			t.Fatalf("DOI after undo = %q, want the value the live item held before the run", got)
		}
		if got := plane.field("extra"); got != "" {
			t.Fatalf("extra after undo = %q, want the pre-apply empty Extra", got)
		}
	})
	t.Run("keeps a value the user changed since", func(t *testing.T) {
		plane := newEnrichUndoPlane(t, "Citation Key: smith2020")
		runID := applyEnrichJournaled(t, plane, enrichUndoDOIProposal())
		plane.edit("DOI", "10.5/user-corrected")

		mustUndoJournalRun(t, runID)
		if got := plane.field("DOI"); got != "10.5/user-corrected" {
			t.Fatalf("DOI after undo = %q, want the user's correction kept", got)
		}
		if got := plane.field("extra"); got != "Citation Key: smith2020" {
			t.Fatalf("extra after undo = %q, want the provenance line removed all the same", got)
		}
	})
}

// A term already on the live item was not added by the run, so undo must not
// take it away. The tags undo removes are the ones the write actually added.
func TestJournalUndoEnrichRemovesOnlyTheTagsTheRunAdded(t *testing.T) {
	plane := newEnrichUndoPlane(t, "Citation Key: smith2020")
	plane.data["tags"] = []any{
		map[string]any{"tag": "typed-by-hand"},
		map[string]any{"tag": "mesh/Humans", "type": float64(1)},
	}
	runID := applyEnrichJournaled(t, plane, enrichProposal{
		Key: "K1", Category: "missing_mesh", Action: enrichActionTag,
		Source: "PubMed (PMID 1)", Tags: []string{"mesh/Humans", "mesh/Neoplasms"},
	})
	if !strings.Contains(plane.field("extra"), "MeSH tags (1)") {
		t.Fatalf("applied extra = %q, want provenance crediting the one added term", plane.field("extra"))
	}

	mustUndoJournalRun(t, runID)
	if got := plane.tagNames(); !slices.Equal(got, []string{"mesh/Humans", "typed-by-hand"}) {
		t.Fatalf("tags after undo = %v, want the term the run did not add kept", got)
	}
	if got := plane.field("extra"); got != "Citation Key: smith2020" {
		t.Fatalf("extra after undo = %q, want the pre-apply Extra", got)
	}
}

// A same-day re-run finds its own provenance line already in Extra and adds
// nothing. That line belongs to the earlier run, so undoing the later one must
// leave it in place.
func TestJournalUndoEnrichKeepsAProvenanceLineAnEarlierRunWrote(t *testing.T) {
	p := enrichUndoDOIProposal()
	pre := "Citation Key: smith2020\n" + enrichProvenanceLine(&p)
	plane := newEnrichUndoPlane(t, pre)
	runID := applyEnrichJournaled(t, plane, p)
	if got := plane.field("extra"); got != pre {
		t.Fatalf("applied extra = %q, want %q unchanged: the line was already there", got, pre)
	}

	entry, err := mutation.ReadEntry(helpersTestJournalDir(t), runID)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	for _, change := range entry.Ops[0].Revert {
		if change.Field == extraLineField {
			t.Fatalf("revert = %+v, want no Extra line recorded for a line this run did not add", entry.Ops[0].Revert)
		}
	}

	mustUndoJournalRun(t, runID)
	if got := plane.field("extra"); got != pre {
		t.Fatalf("extra after undo = %q, want the earlier run's line kept in %q", got, pre)
	}
	if got := plane.field("DOI"); got != "" {
		t.Fatalf("DOI after undo = %q, want it blank again", got)
	}
}

// Entries journaled before this change carry no recorded line. Their tags still
// undo exactly as before, Extra is left alone, and a field overwrite is still
// refused because its prior value was never captured.
func TestJournalUndoEnrichLegacyEntriesBehaveAsBefore(t *testing.T) {
	const legacyExtra = "Citation Key: smith2020\nzotio: MeSH tags (2) added via PubMed (PMID 1) on 2026-10-12"
	entryFor := func(runID string, ops ...mutation.JournalOp) mutation.JournalEntry {
		return mutation.JournalEntry{
			SchemaVersion: mutation.JournalSchemaVersion, RunID: runID, Operation: "items.enrich", Mode: "apply", OK: true,
			Timestamp: time.Now(), Summary: mutation.ResultSummary{Attempted: len(ops), Applied: len(ops)}, Ops: ops,
		}
	}

	t.Run("tags", func(t *testing.T) {
		plane := newEnrichUndoPlane(t, legacyExtra)
		plane.data["tags"] = []any{
			map[string]any{"tag": "typed-by-hand"},
			map[string]any{"tag": "mesh/Humans", "type": float64(1)},
			map[string]any{"tag": "mesh/Neoplasms", "type": float64(1)},
		}
		legacy := entryFor("legacy-mesh", mutation.JournalOp{
			ID: "missing_mesh:K1", Key: "K1", Kind: "missing_mesh", Status: "applied",
			Changes: []mutation.Change{
				{Field: "tags", Add: "mesh/Humans", TagType: 1},
				{Field: "tags", Add: "mesh/Neoplasms", TagType: 1},
			},
		})
		if err := mutation.WriteEntry(helpersTestJournalDir(t), legacy); err != nil {
			t.Fatalf("seed journal: %v", err)
		}

		env := mustUndoJournalRun(t, "legacy-mesh")
		if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 {
			t.Fatalf("undo env = %+v, want one applied reversal", env)
		}
		if got := plane.tagNames(); !slices.Equal(got, []string{"typed-by-hand"}) {
			t.Fatalf("tags after undo = %v, want only the hand-typed tag", got)
		}
		if got := plane.field("extra"); got != legacyExtra {
			t.Fatalf("extra after undo = %q, want it untouched: the entry recorded no line", got)
		}
		if _, sent := plane.lastPatch()["extra"]; sent {
			t.Fatalf("undo PATCH = %v, want no extra key for a legacy entry", plane.lastPatch())
		}
	})

	t.Run("field overwrite is still refused", func(t *testing.T) {
		plane := newEnrichUndoPlane(t, legacyExtra)
		plane.data["DOI"] = "10.1/attention"
		legacy := entryFor("legacy-doi", mutation.JournalOp{
			ID: "missing_doi:K1", Key: "K1", Kind: "missing_doi", Status: "applied",
			Changes: []mutation.Change{{Field: "DOI", Add: "10.1/attention"}},
		})
		if err := mutation.WriteEntry(helpersTestJournalDir(t), legacy); err != nil {
			t.Fatalf("seed journal: %v", err)
		}

		env, err := undoJournalRun(t, "legacy-doi")
		if err == nil {
			t.Fatalf("undo err = nil, want a refusal: env=%+v", env)
		}
		if plane.patchCount() != 0 || plane.field("DOI") != "10.1/attention" {
			t.Fatalf("a refused legacy field overwrite wrote: patches=%d DOI=%q", plane.patchCount(), plane.field("DOI"))
		}
	})
}

func TestRemoveExtraLine(t *testing.T) {
	const line = "zotio: DOI added via CrossRef on 2026-10-12"
	cases := []struct {
		name, extra, want string
		removed           bool
	}{
		{"only line", line, "", true},
		{"last line", "a\n" + line, "a", true},
		{"first line", line + "\nb", "b", true},
		{"middle line", "a\n" + line + "\nb", "a\nb", true},
		{"keeps a trailing blank the user left", "a\n" + line + "\n", "a\n", true},
		{"removes only the first copy", line + "\nb\n" + line, "b\n" + line, true},
		{"absent", "a\nb", "a\nb", false},
		{"only a substring of another line", "see " + line, "see " + line, false},
		{"empty extra", "", "", false},
		// An editor that rewrites Extra gives every line a CRLF terminator.
		// The recorded line has none, and the other lines keep theirs.
		{"crlf middle line", "a\r\n" + line + "\r\nb", "a\r\nb", true},
		{"crlf first line", line + "\r\nb", "b", true},
		{"crlf last line takes the crlf before it", "a\r\n" + line, "a", true},
		{"crlf only line", line + "\r", "", true},
		{"crlf keeps a trailing terminator the user left", "a\r\n" + line + "\r\n", "a\r\n", true},
		{"mixed: only the last line has an lf before it", "a\r\nb\n" + line, "a\r\nb", true},
		{"crlf absent", "a\r\nb\r\n", "a\r\nb\r\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, removed := removeExtraLine(tc.extra, line)
			if got != tc.want || removed != tc.removed {
				t.Fatalf("removeExtraLine(%q) = (%q, %v), want (%q, %v)", tc.extra, got, removed, tc.want, tc.removed)
			}
		})
	}
	if got, removed := removeExtraLine("a\n\nb", ""); got != "a\n\nb" || removed {
		t.Fatalf("an empty line must never match a blank line: got (%q, %v)", got, removed)
	}
}

// removeExtraLine is the exact inverse of the append for every Extra the API
// can hold, which is trimmed: Zotero strips surrounding whitespace on save.
func TestRemoveExtraLineInvertsEnrichProvenanceExtra(t *testing.T) {
	for _, pre := range []string{"", "x", "a\nb", "a\n\nb", "a: b\nc: d"} {
		p := enrichUndoDOIProposal()
		p.extra = pre
		next, added := enrichProvenanceExtra(&p)
		if added == "" {
			t.Fatalf("pre=%q: no line reported added to %q", pre, next)
		}
		if got, removed := removeExtraLine(next, added); !removed || got != pre {
			t.Fatalf("pre=%q: undoing %q gave (%q, %v)", pre, next, got, removed)
		}
	}
}

func TestEnrichProvenanceExtraReportsNoLineWhenAlreadyPresent(t *testing.T) {
	p := enrichUndoDOIProposal()
	p.extra = "a\n" + enrichProvenanceLine(&p)
	next, added := enrichProvenanceExtra(&p)
	if next != p.extra || added != "" {
		t.Fatalf("enrichProvenanceExtra = (%q, %q), want the Extra unchanged and no line added", next, added)
	}
}

// An editor that rewrote Extra with CRLF still shows the line a same-day
// re-run would write, so no second copy is appended and none is claimed.
func TestEnrichProvenanceExtraSeesACRLFTerminatedLine(t *testing.T) {
	p := enrichUndoDOIProposal()
	p.extra = "a\r\n" + enrichProvenanceLine(&p) + "\r\nb"
	next, added := enrichProvenanceExtra(&p)
	if next != p.extra || added != "" {
		t.Fatalf("enrichProvenanceExtra = (%q, %q), want the Extra unchanged and no line added", next, added)
	}
}

// The mirror replays an undo's Extra-line removal onto its cached row, and an
// Extra the mirror holds without the line is a successful no-op, not a reason
// to skip the rest of the item's write-through.
func TestApplyChangeToItemDataRemovesAnExtraLine(t *testing.T) {
	const line = "zotio: DOI added via CrossRef on 2026-10-12"
	remove := mutation.Change{Field: extraLineField, Remove: line}

	data := map[string]any{"extra": "a\n" + line + "\nb"}
	if !applyChangeToItemData(data, remove) || data["extra"] != "a\nb" {
		t.Fatalf("replay = %v, want the line removed", data["extra"])
	}
	data = map[string]any{"extra": "a\nb"}
	if !applyChangeToItemData(data, remove) || data["extra"] != "a\nb" {
		t.Fatalf("replay on an Extra without the line = %v, want it unchanged and handled", data["extra"])
	}
	for _, c := range []mutation.Change{
		{Field: extraLineField, Add: line},
		{Field: extraLineField, Remove: ""},
		{Field: extraLineField},
	} {
		if applyChangeToItemData(map[string]any{"extra": line}, c) {
			t.Fatalf("change %+v replayed; only a removal of a named line is replayable", c)
		}
	}
	data = map[string]any{"extra": "a\r\n" + line + "\r\nb"}
	if !applyChangeToItemData(data, remove) || data["extra"] != "a\r\nb" {
		t.Fatalf("replay on a CRLF Extra = %q, want the line removed with its own terminator", data["extra"])
	}
}

// What apply records is what the journal shows: the planned changes stay free
// of Extra (the mirror must not replay a stale value), and the inverse carries
// the field's prior value and the exact line.
func TestEnrichOpRecordsItsRevertWhileApplying(t *testing.T) {
	plane := newEnrichUndoPlane(t, "Citation Key: smith2020")
	runID := applyEnrichJournaled(t, plane, enrichUndoDOIProposal())

	entry, err := mutation.ReadEntry(helpersTestJournalDir(t), runID)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	op := entry.Ops[0]
	for _, change := range op.Changes {
		if change.Field == "extra" || change.Field == extraLineField {
			t.Fatalf("planned changes = %+v, want Extra kept out of the replayable plan", op.Changes)
		}
	}
	if len(op.Revert) != 2 {
		t.Fatalf("revert = %+v, want a DOI restore and an Extra line removal", op.Revert)
	}
	if got := op.Revert[0]; got.Field != "DOI" || got.Add != "" || got.Remove != "10.1/attention" {
		t.Fatalf("revert[0] = %+v, want DOI restored to blank while it still holds the written value", got)
	}
	wantLine := plane.field("extra")[len("Citation Key: smith2020\n"):]
	if got := op.Revert[1]; got.Field != extraLineField || got.Remove != wantLine {
		t.Fatalf("revert[1] = %+v, want removal of exactly the appended line %q", got, wantLine)
	}
}

// latestJournalRunID reads the run id from the journal rather than the
// envelope: an enrich run that skipped candidates reports `skipped` in
// env.Journal, which hides the run id the recorder put there.
func latestJournalRunID(t *testing.T, operation string) string {
	t.Helper()
	entries, err := mutation.ListEntries(helpersTestJournalDir(t))
	if err != nil || len(entries) == 0 || entries[0].Operation != operation {
		t.Fatalf("journal entries = %+v err=%v, want the newest to be an %s run", entries, err, operation)
	}
	return entries[0].RunID
}

// The two real commands end to end: `items enrich --yes` writes the field and
// the provenance line, `journal undo <run>` takes both back out. The mirror's
// own Extra is deliberately stale; the live Extra is the one that must come
// back byte for byte.
func TestItemsEnrichMissingDOIThenJournalUndoRestoresLiveExtra(t *testing.T) {
	const liveExtra = "Citation Key: smith2020\nlive user note"
	crsrv := crossRefSearchServer(t, "Attention Is All You Need", "10.1/attention")
	withBase(t, &enrichCrossRefBase, crsrv.URL)
	plane := newEnrichUndoPlane(t, liveExtra)
	_ = seedEnrichStore(t, "stale mirror note")
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	env, err := runMeshEnrich(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--missing-doi", "--no-openalex", "--no-semantic-scholar")
	if err != nil || env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("enrich apply: err=%v env=%+v, want one applied write", err, env)
	}
	if got := plane.field("DOI"); got != "10.1/attention" || !strings.Contains(plane.field("extra"), "zotio: DOI added via CrossRef") {
		t.Fatalf("applied DOI=%q extra=%q, want the DOI and its provenance line", got, plane.field("extra"))
	}
	runID := latestJournalRunID(t, "items.enrich")

	mustUndoJournalRun(t, runID)
	if got := plane.field("extra"); got != liveExtra {
		t.Fatalf("extra after undo = %q, want the live pre-apply %q", got, liveExtra)
	}
	if got := plane.field("DOI"); got != "" {
		t.Fatalf("DOI after undo = %q, want it blank again", got)
	}
}

func TestItemsEnrichMissingMeshThenJournalUndoRestoresLiveExtra(t *testing.T) {
	const liveExtra = "Citation Key: smith2020"
	stub := newPubMedStub(t)
	stub.efetch["111"] = meshArticleXML("111", "", "Humans", "Neoplasms")
	plane := newEnrichUndoPlane(t, liveExtra)
	seedMeshStore(t,
		`{"key":"K1","version":4,"data":{"key":"K1","itemType":"journalArticle","title":"T","PMID":"111","tags":[]}}`)
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	env, err := runMeshEnrich(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--missing-mesh")
	if err != nil || env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("enrich apply: err=%v env=%+v, want one applied write", err, env)
	}
	if got := plane.tagNames(); !slices.Equal(got, []string{"mesh/Humans", "mesh/Neoplasms", "typed-by-hand"}) {
		t.Fatalf("applied tags = %v, want both MeSH terms added", got)
	}
	runID := latestJournalRunID(t, "items.enrich")

	mustUndoJournalRun(t, runID)
	if got := plane.field("extra"); got != liveExtra {
		t.Fatalf("extra after undo = %q, want the live pre-apply %q: the MeSH provenance line must go", got, liveExtra)
	}
	if got := plane.tagNames(); !slices.Equal(got, []string{"typed-by-hand"}) {
		t.Fatalf("tags after undo = %v, want only the hand-typed tag", got)
	}
}

// An applied run that also skipped a candidate must still print its run_id:
// the skipped list is merged into the journal object, not written over it.
func TestItemsEnrichAppliedRunWithSkipsKeepsRunID(t *testing.T) {
	stub := newPubMedStub(t)
	stub.efetch["111"] = meshArticleXML("111", "", "Humans")
	stub.efetch["222"] = meshArticleXML("222", "")
	_ = newEnrichUndoPlane(t, "")
	seedMeshStore(t,
		`{"key":"K1","version":4,"data":{"key":"K1","itemType":"journalArticle","title":"T","PMID":"111","tags":[]}}`,
		`{"key":"K2","version":4,"data":{"key":"K2","itemType":"journalArticle","title":"U","PMID":"222","tags":[]}}`)
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	env, err := runMeshEnrich(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--missing-mesh")
	if err != nil || env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("enrich apply: err=%v env=%+v, want one applied write", err, env)
	}
	journal, _ := env.Journal.(map[string]any)
	if runID, _ := journal["run_id"].(string); runID == "" {
		t.Fatalf("journal = %v, want the applied run_id kept next to the skips", env.Journal)
	}
	if skipped, _ := journal["skipped"].([]any); len(skipped) != 1 {
		t.Fatalf("journal = %v, want the one skipped candidate", env.Journal)
	}
}

// The undo's PATCH skips a field restore when the user has corrected the field
// since. What the undo reports as effective is exactly what the PATCH carried,
// because the mirror replays and records that, not the plan.
func TestApplyUndoMembershipReportsOnlyTheChangesItSent(t *testing.T) {
	const line = "zotio: DOI added via CrossRef on 2026-10-12"
	plane := newEnrichUndoPlane(t, "Citation Key: smith2020\n"+line)
	plane.data["DOI"] = "10.5/user-corrected"
	plane.data["tags"] = []any{
		map[string]any{"tag": "typed-by-hand"},
		map[string]any{"tag": "mesh/Humans", "type": float64(1)},
	}
	c := newEnrichWriteClient(t, plane.srv.URL)
	changes := []mutation.Change{
		{Field: "tags", Remove: "mesh/Humans", TagType: 1},
		{Field: "tags", Remove: "mesh/Gone", TagType: 1},
		{Field: "DOI", Add: "", Remove: "10.1/attention"},
		{Field: extraLineField, Remove: line},
	}

	status, detail, effective, err := applyUndoMembership(c, "/items/K1", changes)
	if err != nil || status != "applied" {
		t.Fatalf("status=%q detail=%v err=%v, want applied", status, detail, err)
	}
	want := []mutation.Change{
		{Field: "tags", Remove: "mesh/Humans", TagType: 1},
		{Field: extraLineField, Remove: line},
	}
	if len(effective) != len(want) || effective[0] != want[0] || effective[1] != want[1] {
		t.Fatalf("effective = %+v, want only the tag removal and the Extra line that were sent: %+v", effective, want)
	}
	if _, sent := plane.lastPatch()["DOI"]; sent {
		t.Fatalf("undo PATCH = %v, want the user's corrected DOI left out", plane.lastPatch())
	}
	if got := plane.field("DOI"); got != "10.5/user-corrected" {
		t.Fatalf("DOI after undo = %q, want the user's correction kept", got)
	}
}

// The mirror write-through for an undo replays what the undo wrote. A field the
// user corrected after the run is not restored by the PATCH, so the mirror must
// not be assigned the recorded prior value, and the pending-write marker must
// not carry it: on sync the marker would overrule the user's correction.
func TestJournalUndoMirrorReplaysOnlyWhatTheUndoSent(t *testing.T) {
	const liveExtra = "Citation Key: smith2020"
	for _, tc := range []struct {
		name            string
		userCorrectsDOI bool
		wantMirrorDOI   string
		wantMarkerDOI   bool
	}{
		{"a restored field is replayed and recorded", false, "", true},
		{"a field the user corrected is neither replayed nor recorded", true, "10.1/attention", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crsrv := crossRefSearchServer(t, "Attention Is All You Need", "10.1/attention")
			withBase(t, &enrichCrossRefBase, crsrv.URL)
			plane := newEnrichUndoPlane(t, liveExtra)
			db := seedEnrichStore(t, liveExtra)
			oldMirror := mirrorWriteThrough
			mirrorWriteThrough = applyMirrorWriteThrough
			mutationJournalRecorder = recordMutationJournal
			t.Cleanup(func() {
				mirrorWriteThrough = oldMirror
				mutationJournalRecorder = nil
			})

			env, err := runMeshEnrich(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--missing-doi", "--no-openalex", "--no-semantic-scholar")
			if err != nil || env.Result == nil || env.Result.Summary.Applied != 1 {
				t.Fatalf("enrich apply: err=%v env=%+v, want one applied write", err, env)
			}
			runID := latestJournalRunID(t, "items.enrich")
			if tc.userCorrectsDOI {
				plane.edit("DOI", "10.5/user-corrected")
			}

			undo := mustUndoJournalRun(t, runID)
			if !undo.OK || undo.Result == nil || undo.Result.Summary.Applied != 1 {
				t.Fatalf("undo env = %+v, want one applied reversal", undo)
			}

			rows, err := db.QueryRaw("SELECT json_extract(data,'$.data.DOI') AS doi FROM resources WHERE resource_type='items' AND id='K1'")
			if err != nil || len(rows) != 1 {
				t.Fatalf("read mirror row: rows=%v err=%v", rows, err)
			}
			if got := sqlStringValue(rows[0]["doi"]); got != tc.wantMirrorDOI {
				t.Fatalf("mirror DOI after undo = %q, want %q", got, tc.wantMirrorDOI)
			}
			pending, err := db.PendingWrites("items")
			if err != nil {
				t.Fatalf("read pending writes: %v", err)
			}
			mark, ok := pending["K1"]
			if !ok {
				t.Fatal("undo recorded no pending write for K1")
			}
			var changes []mutation.Change
			if err := json.Unmarshal(mark.Changes, &changes); err != nil {
				t.Fatalf("decode pending changes: %v", err)
			}
			var hasDOI, hasLine bool
			for _, change := range changes {
				hasDOI = hasDOI || change.Field == "DOI"
				hasLine = hasLine || change.Field == extraLineField
			}
			if hasDOI != tc.wantMarkerDOI || !hasLine {
				t.Fatalf("pending changes = %+v, want DOI recorded=%v and the Extra line removal recorded", changes, tc.wantMarkerDOI)
			}
		})
	}
}

// An editor that rewrote Extra with CRLF separators must not leave the
// provenance line stranded: the recorded line has no CR, and the other lines
// keep their own terminators.
func TestJournalUndoEnrichMatchesAnExtraRewrittenWithCRLF(t *testing.T) {
	const pre = "Citation Key: smith2020\nlive note"
	for _, tc := range []struct{ name, suffix, want string }{
		{"the line is last", "", "Citation Key: smith2020\r\nlive note"},
		{"the user's lines follow", "\r\ntrailing note", "Citation Key: smith2020\r\nlive note\r\ntrailing note"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plane := newEnrichUndoPlane(t, pre)
			runID := applyEnrichJournaled(t, plane, enrichUndoDOIProposal())
			plane.edit("extra", strings.ReplaceAll(plane.field("extra"), "\n", "\r\n")+tc.suffix)

			mustUndoJournalRun(t, runID)
			if got := plane.field("extra"); got != tc.want {
				t.Fatalf("extra after undo = %q, want %q", got, tc.want)
			}
			if got := plane.field("DOI"); got != "" {
				t.Fatalf("DOI after undo = %q, want it blank again", got)
			}
		})
	}
}
