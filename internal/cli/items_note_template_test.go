// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/store"
)

func TestYearFromDate(t *testing.T) {
	tests := []struct {
		name string
		date string
		want string
	}{
		{name: "ISO date", date: "2026-07-15", want: "2026"},
		{name: "month then year", date: "July 2026", want: "2026"},
		{name: "day month then year", date: "15 July 2026", want: "2026"},
		{name: "circa year", date: "c. 1997", want: "1997"},
		{name: "no date", date: "n.d.", want: ""},
		{name: "empty", date: "", want: ""},
		{name: "implausible four digit number", date: "page 8421", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := yearFromDate(tt.date); got != tt.want {
				t.Errorf("yearFromDate(%q) = %q, want %q", tt.date, got, tt.want)
			}
		})
	}
}

// Zotero pins a citekey colon-tight ("Citation Key:tight2026") while zotio's
// own importer writes the spaced form. The note template read only the spaced
// spelling and silently rendered an empty cite_key for every Zotero-pinned
// item. Both spellings are asserted here so the fix cannot be a swap of one
// narrow parse for another.
func TestNoteTemplateRendersBothExtraCiteKeySpellings(t *testing.T) {
	tests := []struct {
		name  string
		extra string
		want  string
	}{
		{name: "colon-tight", extra: "Citation Key:tight2026", want: "tight2026"},
		{name: "spaced", extra: "Citation Key: spaced2026", want: "spaced2026"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{
				"data": map[string]any{
					"title":    "A Study of Foos",
					"itemType": "journalArticle",
					"date":     "2026",
					"extra":    "some prior note\n" + tc.extra,
				},
			})
			if err != nil {
				t.Fatalf("marshal item: %v", err)
			}
			meta, err := noteMetadataFromItem(raw)
			if err != nil {
				t.Fatalf("noteMetadataFromItem: %v", err)
			}
			rendered := renderStandardNoteTemplate(meta, nil, false, time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
			wantLine := `cite_key: "` + tc.want + `"`
			if !strings.Contains(rendered, wantLine) {
				t.Fatalf("note template for Extra %q missing %q; rendered:\n%s", tc.extra, wantLine, rendered)
			}
		})
	}
}

func TestNoteMetadataFromItem(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want itemNoteMetadata
	}{
		{
			name: "wrapped every field populated",
			raw:  `{"data":{"title":"The Title","DOI":"10.1000/test","abstractNote":"  An abstract.  ","extra":"Some notes\nCitation Key: testKey2026\nMore","date":"2026-07-15","creators":[{"lastName":"Smith","firstName":"Jane"},{"name":"Institute of Testing"}]}}`,
			want: itemNoteMetadata{
				Title:    "The Title",
				Authors:  []string{"Smith, Jane", "Institute of Testing"},
				Year:     "2026",
				DOI:      "10.1000/test",
				Abstract: "An abstract.",
				CiteKey:  "testKey2026",
			},
		},
		{
			name: "unwrapped same fields",
			raw:  `{"title":"The Title","DOI":"10.1000/test","abstractNote":"An abstract.","extra":"Citation Key: testKey2026","date":"2026-07-15","creators":[{"lastName":"Smith","firstName":"Jane"}]}`,
			want: itemNoteMetadata{
				Title:    "The Title",
				Authors:  []string{"Smith, Jane"},
				Year:     "2026",
				DOI:      "10.1000/test",
				Abstract: "An abstract.",
				CiteKey:  "testKey2026",
			},
		},
		{
			name: "missing title",
			raw:  `{"data":{"DOI":"10.1","abstractNote":"abs","extra":"Citation Key: k1","date":"2026-03-01","creators":[]}}`,
			want: itemNoteMetadata{
				Title:    "",
				Authors:  nil,
				Year:     "2026",
				DOI:      "10.1",
				Abstract: "abs",
				CiteKey:  "k1",
			},
		},
		{
			name: "missing abstract",
			raw:  `{"data":{"title":"t","DOI":"10.1","extra":"","date":"2026-03-01","creators":[]}}`,
			want: itemNoteMetadata{
				Title:    "t",
				Authors:  nil,
				Year:     "2026",
				DOI:      "10.1",
				Abstract: "",
				CiteKey:  "",
			},
		},
		{
			name: "missing date",
			raw:  `{"data":{"title":"t","DOI":"10.1","abstractNote":"abs","extra":"","date":"","creators":[]}}`,
			want: itemNoteMetadata{
				Title:    "t",
				Authors:  nil,
				Year:     "",
				DOI:      "10.1",
				Abstract: "abs",
				CiteKey:  "",
			},
		},
		{
			name: "absent creators",
			raw:  `{"data":{"title":"t","DOI":"10.1","abstractNote":"abs","extra":"","date":"2026"}}`,
			want: itemNoteMetadata{
				Title:    "t",
				Authors:  nil,
				Year:     "2026",
				DOI:      "10.1",
				Abstract: "abs",
				CiteKey:  "",
			},
		},
		{
			name: "creator list mixing two-field and single-field",
			raw:  `{"data":{"title":"t","creators":[{"lastName":"Smith","firstName":"Jane"},{"name":"Institute of Testing"},{"lastName":"Doe","firstName":""}]}}`,
			want: itemNoteMetadata{
				Title:    "t",
				Authors:  []string{"Smith, Jane", "Institute of Testing", "Doe"},
				Year:     "",
				DOI:      "",
				Abstract: "",
				CiteKey:  "",
			},
		},
		{
			name: "creator with only firstName",
			raw:  `{"data":{"title":"t","creators":[{"firstName":"Plato"}]}}`,
			want: itemNoteMetadata{
				Title:    "t",
				Authors:  []string{"Plato"},
				Year:     "",
				DOI:      "",
				Abstract: "",
				CiteKey:  "",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := noteMetadataFromItem(json.RawMessage([]byte(tc.raw)))
			if err != nil {
				t.Fatalf("noteMetadataFromItem error = %v, want nil", err)
			}
			if got.Title != tc.want.Title {
				t.Fatalf("Title = %q, want %q", got.Title, tc.want.Title)
			}
			if got.Year != tc.want.Year {
				t.Fatalf("Year = %q, want %q", got.Year, tc.want.Year)
			}
			if got.DOI != tc.want.DOI {
				t.Fatalf("DOI = %q, want %q", got.DOI, tc.want.DOI)
			}
			if got.Abstract != tc.want.Abstract {
				t.Fatalf("Abstract = %q, want %q", got.Abstract, tc.want.Abstract)
			}
			if got.CiteKey != tc.want.CiteKey {
				t.Fatalf("CiteKey = %q, want %q", got.CiteKey, tc.want.CiteKey)
			}
			if len(got.Authors) != len(tc.want.Authors) {
				t.Fatalf("Authors = %v, want %v", got.Authors, tc.want.Authors)
			}
			for i := range tc.want.Authors {
				if got.Authors[i] != tc.want.Authors[i] {
					t.Fatalf("Authors[%d] = %q, want %q", i, got.Authors[i], tc.want.Authors[i])
				}
			}
		})
	}

	t.Run("invalid json", func(t *testing.T) {
		if _, err := noteMetadataFromItem(json.RawMessage([]byte(`{invalid}`))); err == nil {
			t.Fatalf("noteMetadataFromItem with invalid JSON error = nil, want error")
		}
	})

	t.Run("abstract is trimmed", func(t *testing.T) {
		raw := `{"data":{"abstractNote":"  spaced  "}}`
		got, err := noteMetadataFromItem(json.RawMessage([]byte(raw)))
		if err != nil {
			t.Fatalf("noteMetadataFromItem error = %v", err)
		}
		if got.Abstract != "spaced" {
			t.Fatalf("Abstract = %q, want %q", got.Abstract, "spaced")
		}
	})

	t.Run("caps authors at three", func(t *testing.T) {
		raw := `{"data":{"creators":[{"lastName":"A"},{"lastName":"B"},{"lastName":"C"},{"lastName":"D"}]}}`
		got, err := noteMetadataFromItem(json.RawMessage([]byte(raw)))
		if err != nil {
			t.Fatalf("noteMetadataFromItem error = %v", err)
		}
		if len(got.Authors) != 3 {
			t.Fatalf("Authors len = %d, want 3", len(got.Authors))
		}
	})
}

func TestRenderNoteTemplate(t *testing.T) {
	fixed := time.Date(2026, 8, 22, 10, 30, 0, 0, time.UTC)
	meta := itemNoteMetadata{
		Title:    "A Study of Foos",
		Authors:  []string{"Smith, Jane", "Doe, John"},
		Year:     "2026",
		DOI:      "10.1000/foo",
		Abstract: "The abstract.",
		CiteKey:  "smith2026",
	}

	tests := []struct {
		name     string
		obsidian bool
		want     string
	}{
		{
			name:     "standard",
			obsidian: false,
			want: `---
title: "A Study of Foos"
authors: ["Smith, Jane", "Doe, John"]
year: "2026"
doi: "10.1000/foo"
cite_key: "smith2026"
tags: []
date_read: 2026-08-22
---

## Abstract

The abstract.

## Key Points

-

## Annotations

<!-- zotio:annotations (auto-generated; edits here are overwritten on sync) -->
<!-- /zotio:annotations -->

## Notes
`,
		},
		{
			name:     "obsidian wikilinks authors",
			obsidian: true,
			want: `---
title: "A Study of Foos"
authors: ["[[Smith, Jane]]", "[[Doe, John]]"]
year: "2026"
doi: "10.1000/foo"
cite_key: "smith2026"
tags: []
date_read: 2026-08-22
---

## Abstract

The abstract.

## Key Points

-

## Annotations

<!-- zotio:annotations (auto-generated; edits here are overwritten on sync) -->
<!-- /zotio:annotations -->

## Notes
`,
		},
		{
			name:     "empty year omits quotes",
			obsidian: false,
			want: `---
title: "A Study of Foos"
authors: ["Smith, Jane", "Doe, John"]
year:
doi: "10.1000/foo"
cite_key: "smith2026"
tags: []
date_read: 2026-08-22
---

## Abstract

The abstract.

## Key Points

-

## Annotations

<!-- zotio:annotations (auto-generated; edits here are overwritten on sync) -->
<!-- /zotio:annotations -->

## Notes
`,
		},
		{
			name:     "empty abstract placeholder",
			obsidian: false,
			want: `---
title: "A Study of Foos"
authors: ["Smith, Jane", "Doe, John"]
year: "2026"
doi: "10.1000/foo"
cite_key: "smith2026"
tags: []
date_read: 2026-08-22
---

## Abstract

(no abstract)

## Key Points

-

## Annotations

<!-- zotio:annotations (auto-generated; edits here are overwritten on sync) -->
<!-- /zotio:annotations -->

## Notes
`,
		},
	}

	// Table covers each fixture; empty-year and empty-abstract check edge
	// quoting/placeholder behaviour.
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := meta
			if tc.name == "empty year omits quotes" {
				m.Year = ""
			}
			if tc.name == "empty abstract placeholder" {
				m.Abstract = ""
			}
			got := renderStandardNoteTemplate(m, nil, tc.obsidian, fixed)
			if got != tc.want {
				t.Fatalf("renderStandardNoteTemplate %s mismatch\n got:\n%s\nwant:\n%s", tc.name, got, tc.want)
			}
			// Front matter delimiters.
			if !strings.HasPrefix(got, "---\n") {
				t.Fatalf("output does not start with front matter delimiter ---, got %q", got[:20])
			}
			if !strings.Contains(got, "\n---\n\n") {
				t.Fatalf("output missing closing front matter delimiter")
			}
			// One key per line and quoting: check that title and doi are quoted.
			if !strings.Contains(got, "title: \"A Study of Foos\"") {
				t.Fatalf("front matter title not quoted, got %q", got)
			}
			// Deterministic date.
			if !strings.Contains(got, "date_read: 2026-08-22") {
				t.Fatalf("date_read not rendered from fixed time, got %q", got)
			}
		})
	}

	// Explicit difference between variants: obsidian wraps each author in [[ ]].
	t.Run("difference between standard and obsidian", func(t *testing.T) {
		standard := renderStandardNoteTemplate(meta, nil, false, fixed)
		obsidian := renderStandardNoteTemplate(meta, nil, true, fixed)
		if standard == obsidian {
			t.Fatalf("standard and obsidian outputs are equal, want different wikilink wrapping")
		}
		if strings.Contains(standard, "[[Smith, Jane]]") {
			t.Fatalf("standard output contains wikilink %q, want plain authors", "[[Smith, Jane]]")
		}
		if !strings.Contains(obsidian, "[[Smith, Jane]]") || !strings.Contains(obsidian, "[[Doe, John]]") {
			t.Fatalf("obsidian output missing wikilink-wrapped authors, got %q", obsidian)
		}
		// Wikilink format is [[author]].
		if !strings.Contains(obsidian, `["[[Smith, Jane]]", "[[Doe, John]]"]`) {
			t.Fatalf("obsidian wikilink format wrong, got %q", obsidian)
		}
	})

	t.Run("authors yaml is quoted", func(t *testing.T) {
		got := renderStandardNoteTemplate(meta, nil, false, fixed)
		// Authors array must use yamlStringArray quoting.
		if !strings.Contains(got, `authors: ["Smith, Jane", "Doe, John"]`) {
			t.Fatalf("authors line not quoted YAML array, got %q", got)
		}
	})
}

func TestNoteTemplateSanitizesAbstractManagedFenceMarkers(t *testing.T) {
	fixed := time.Date(2026, 8, 22, 10, 30, 0, 0, time.UTC)
	meta := itemNoteMetadata{
		Title:    "A Study of Foos",
		Abstract: "Operator text\n" + vaultAnnBegin + "\nMore operator text",
	}

	for _, tc := range []struct {
		name   string
		render func() string
	}{
		{name: "standard", render: func() string {
			return renderStandardNoteTemplate(meta, nil, false, fixed)
		}},
		{name: "logseq", render: func() string {
			return renderLogseqNoteTemplate(meta, nil, fixed)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.render()
			annotationsHeading := strings.Index(got, "## Annotations")
			firstFence := strings.Index(got, vaultAnnBegin)
			if firstFence < annotationsHeading {
				t.Fatalf("abstract opened an annotations region before the labelled section:\n%s", got)
			}
			if count := strings.Count(got, vaultAnnBegin); count != 1 {
				t.Fatalf("begin fence count = %d, want only the managed annotations fence:\n%s", count, got)
			}
			if !strings.Contains(got, "[zotio annotations marker removed]") {
				t.Fatalf("abstract fence marker was not neutralized:\n%s", got)
			}
		})
	}
}

func TestRenderLogseqNoteTemplate(t *testing.T) {
	fixed := time.Date(2026, 8, 22, 10, 30, 0, 0, time.UTC)

	tests := []struct {
		name string
		meta itemNoteMetadata
		want string
	}{
		{
			name: "with authors",
			meta: itemNoteMetadata{
				Title:    "A Study of Foos",
				Authors:  []string{"Smith, Jane", "Doe, John"},
				Year:     "2026",
				DOI:      "10.1000/foo",
				Abstract: "The abstract.",
				CiteKey:  "smith2026",
			},
			want: `- title:: A Study of Foos
- authors:: [[Smith, Jane]], [[Doe, John]]
- year:: 2026
- doi:: 10.1000/foo
- cite_key:: smith2026
- tags::
- date_read:: 2026-08-22
- ## Abstract
  - The abstract.
- ## Key Points
  - 
- ## Annotations
  <!-- zotio:annotations (auto-generated; edits here are overwritten on sync) -->
  <!-- /zotio:annotations -->
- ## Notes
  - 
`,
		},
		{
			name: "empty author list",
			meta: itemNoteMetadata{
				Title:    "A Study of Foos",
				Authors:  nil,
				Year:     "2026",
				DOI:      "10.1000/foo",
				Abstract: "The abstract.",
				CiteKey:  "smith2026",
			},
			want: `- title:: A Study of Foos
- authors::
- year:: 2026
- doi:: 10.1000/foo
- cite_key:: smith2026
- tags::
- date_read:: 2026-08-22
- ## Abstract
  - The abstract.
- ## Key Points
  - 
- ## Annotations
  <!-- zotio:annotations (auto-generated; edits here are overwritten on sync) -->
  <!-- /zotio:annotations -->
- ## Notes
  - 
`,
		},
		{
			name: "empty abstract placeholder",
			meta: itemNoteMetadata{
				Title:    "T",
				Authors:  []string{"Smith, Jane"},
				Year:     "2026",
				DOI:      "10.1",
				Abstract: "",
				CiteKey:  "k",
			},
			want: `- title:: T
- authors:: [[Smith, Jane]]
- year:: 2026
- doi:: 10.1
- cite_key:: k
- tags::
- date_read:: 2026-08-22
- ## Abstract
  - (no abstract)
- ## Key Points
  - 
- ## Annotations
  <!-- zotio:annotations (auto-generated; edits here are overwritten on sync) -->
  <!-- /zotio:annotations -->
- ## Notes
  - 
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := renderLogseqNoteTemplate(tc.meta, nil, fixed)
			if got != tc.want {
				t.Fatalf("renderLogseqNoteTemplate %s mismatch\n got:\n%s\nwant:\n%s", tc.name, got, tc.want)
			}
			// Logseq bullet syntax: top-level bullets start with "- " and nested with "  - ".
			if !strings.HasPrefix(got, "- title::") {
				t.Fatalf("logseq output does not start with \"- title::\", got %q", got[:20])
			}
			if !strings.Contains(got, "- ## Abstract\n  - ") {
				t.Fatalf("logseq abstract bullet syntax wrong, got %q", got)
			}
			if !strings.Contains(got, "- ## Key Points\n  - ") {
				t.Fatalf("logseq key points bullet syntax wrong, got %q", got)
			}
			if !strings.Contains(got, "date_read:: 2026-08-22") {
				t.Fatalf("logseq date_read not from fixed time, got %q", got)
			}
		})
	}

	t.Run("authors are wikilinked", func(t *testing.T) {
		meta := itemNoteMetadata{
			Title:    "T",
			Authors:  []string{"Smith, Jane"},
			Year:     "2026",
			DOI:      "",
			Abstract: "abs",
			CiteKey:  "",
		}
		got := renderLogseqNoteTemplate(meta, nil, fixed)
		if !strings.Contains(got, "- authors:: [[Smith, Jane]]") {
			t.Fatalf("logseq authors not wikilinked [[author]], got %q", got)
		}
	})

	t.Run("empty authors has no trailing space value", func(t *testing.T) {
		meta := itemNoteMetadata{
			Title:    "T",
			Authors:  []string{},
			Year:     "2026",
			Abstract: "abs",
		}
		got := renderLogseqNoteTemplate(meta, nil, fixed)
		if !strings.Contains(got, "- authors::\n") {
			t.Fatalf("empty authors should emit \"- authors::\\n\", got %q", got)
		}
	})
}

func TestRenderLogseqNoteTemplateNestsAnnotationsUnderHeading(t *testing.T) {
	fixed := time.Date(2026, 8, 22, 10, 30, 0, 0, time.UTC)
	got := renderLogseqNoteTemplate(itemNoteMetadata{Title: "T", Abstract: "abs"}, []annotationSummary{{
		Key:        "ANN1",
		ParentItem: "PDF1",
		Text:       "A highlight",
		Comment:    "Reader note",
		Page:       "4",
	}}, fixed)

	for _, want := range []string{
		"- ## Annotations\n  " + vaultAnnBegin,
		"\n  - A highlight ",
		"\n    - Note: Reader note",
		"\n  " + vaultAnnEnd + "\n- ## Notes",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Logseq annotation block missing child indentation %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\n- A highlight ") {
		t.Fatalf("Logseq annotation rendered as a top-level sibling:\n%s", got)
	}
}

func TestNoteTemplateRequiresSyncedStore(t *testing.T) {
	root, _, out, _ := newPreflightTestRoot(t)
	noteTemplate := mustFindPreflightCommand(t, root, "items", "note-template")
	runExecuted := false
	noteTemplate.RunE = func(cmd *cobra.Command, args []string) error {
		runExecuted = true
		return nil
	}

	root.SetArgs([]string{"--json", "items", "note-template", "ITEM1"})
	err := root.Execute()
	if err == nil {
		t.Fatal("items note-template without a synced store succeeded, want precondition error")
	}
	if runExecuted {
		t.Fatal("items note-template RunE executed after synced_store preflight failed")
	}
	if got := ExitCode(err); got != 9 {
		t.Fatalf("exit code = %d, want 9; err=%v", got, err)
	}

	var env preconditionUnmetEnvelope
	if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
		t.Fatalf("decode precondition envelope: %v; output=%q", decodeErr, out.String())
	}
	if env.Kind != "precondition_unmet" || env.Capability != "items note-template" || env.Precondition != preconditionSyncedStore {
		t.Fatalf("envelope = %+v, want items note-template / synced_store refusal", env)
	}
	if len(env.Remediation) == 0 || !strings.Contains(strings.Join(env.Remediation, " "), "zotio sync") {
		t.Fatalf("remediation = %v, want zotio sync guidance", env.Remediation)
	}
}

func TestNoteTemplateRendersMirroredAnnotations(t *testing.T) {
	seedNoteTemplateMirror(t, []json.RawMessage{
		json.RawMessage(`{"key":"PAPER1","version":1,"data":{"key":"PAPER1","itemType":"journalArticle","title":"Annotated Paper","date":"2026"}}`),
		json.RawMessage(`{"key":"PDF1","version":1,"data":{"key":"PDF1","itemType":"attachment","parentItem":"PAPER1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"ANN12","version":1,"data":{"key":"ANN12","itemType":"annotation","parentItem":"PDF1","annotationType":"highlight","annotationText":"A later highlight","annotationPageLabel":"12","dateAdded":"2026-02-01T00:00:00Z"}}`),
		json.RawMessage(`{"key":"ANN3","version":1,"data":{"key":"ANN3","itemType":"annotation","parentItem":"PDF1","annotationType":"highlight","annotationText":"An earlier highlight","annotationPageLabel":"3","dateAdded":"2026-01-01T00:00:00Z"}}`),
	})

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "standard", args: []string{"PAPER1"}},
		{name: "logseq", args: []string{"PAPER1", "--format", "logseq"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runNoteTemplateTestCommand(t, tc.args...)
			section := noteTemplateAnnotationSection(t, out)
			for _, want := range []string{
				"An earlier highlight",
				"[p. 3]",
				"A later highlight",
				"[p. 12]",
				vaultAnnBegin,
				vaultAnnEnd,
			} {
				if !strings.Contains(section, want) {
					t.Errorf("Annotations section missing %q:\n%s", want, section)
				}
			}
			if strings.Index(section, "An earlier highlight") > strings.Index(section, "A later highlight") {
				t.Errorf("annotations are not sorted by page:\n%s", section)
			}
		})
	}
}

func TestNoteTemplateWithoutAnnotationsRendersEmptyManagedSection(t *testing.T) {
	seedNoteTemplateMirror(t, []json.RawMessage{
		json.RawMessage(`{"key":"EMPTY1","version":1,"data":{"key":"EMPTY1","itemType":"journalArticle","title":"Paper Without Annotations","date":"2026"}}`),
	})

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "standard", args: []string{"EMPTY1"}},
		{name: "logseq", args: []string{"EMPTY1", "--format", "logseq"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runNoteTemplateTestCommand(t, tc.args...)
			section := noteTemplateAnnotationSection(t, out)
			if !strings.Contains(section, "## Annotations") {
				t.Fatalf("output missing Annotations section:\n%s", out)
			}
			if strings.Contains(out, "annotations export") || strings.Contains(out, "Export annotations") {
				t.Fatalf("output contains the obsolete annotations export instruction:\n%s", out)
			}
			want := vaultAnnBegin + "\n" + vaultAnnEnd
			if tc.name == "logseq" {
				want = "  " + vaultAnnBegin + "\n  " + vaultAnnEnd
			}
			if !strings.Contains(section, want) {
				t.Fatalf("empty Annotations section does not contain an empty managed region:\n%s", section)
			}
			if strings.Contains(section, "_No annotations._") {
				t.Fatalf("empty Annotations section contains placeholder text:\n%s", section)
			}
		})
	}
}

func seedNoteTemplateMirror(t *testing.T, items []json.RawMessage) {
	t.Helper()
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		_ = db.Close()
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func runNoteTemplateTestCommand(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newItemsNoteTemplateCmd(&rootFlags{dataSource: "local"})
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("note-template %v: %v", args, err)
	}
	return out.String()
}

func noteTemplateAnnotationSection(t *testing.T, out string) string {
	t.Helper()
	start := strings.Index(out, "## Annotations")
	if start < 0 {
		t.Fatalf("output missing Annotations section:\n%s", out)
	}
	end := strings.Index(out[start:], "## Notes")
	if end < 0 {
		t.Fatalf("output missing Notes section after Annotations:\n%s", out)
	}
	return out[start : start+end]
}

func TestYAMLStringArray(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{name: "empty nil", values: nil, want: "[]"},
		{name: "empty slice", values: []string{}, want: "[]"},
		{name: "one element", values: []string{"a"}, want: `["a"]`},
		{name: "several elements", values: []string{"a", "b", "c"}, want: `["a", "b", "c"]`},
		{name: "colon needs quoting", values: []string{"a: b"}, want: `["a: b"]`},
		{name: "quote needs quoting", values: []string{`a"b`}, want: `["a\"b"]`},
		{name: "leading dash", values: []string{"- item"}, want: `["- item"]`},
		{name: "multiple with special chars", values: []string{"a: b", `c"d`, "- e"}, want: `["a: b", "c\"d", "- e"]`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := yamlStringArray(tc.values)
			if got != tc.want {
				t.Fatalf("yamlStringArray(%v) = %q, want %q", tc.values, got, tc.want)
			}
		})
	}
}
