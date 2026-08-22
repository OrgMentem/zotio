// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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

<!-- Export annotations with: zotio items annotations <itemKey> -->

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

<!-- Export annotations with: zotio items annotations <itemKey> -->

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

<!-- Export annotations with: zotio items annotations <itemKey> -->

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

<!-- Export annotations with: zotio items annotations <itemKey> -->

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
			got := renderStandardNoteTemplate(m, tc.obsidian, fixed)
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
		standard := renderStandardNoteTemplate(meta, false, fixed)
		obsidian := renderStandardNoteTemplate(meta, true, fixed)
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
		got := renderStandardNoteTemplate(meta, false, fixed)
		// Authors array must use yamlStringArray quoting.
		if !strings.Contains(got, `authors: ["Smith, Jane", "Doe, John"]`) {
			t.Fatalf("authors line not quoted YAML array, got %q", got)
		}
	})
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
  - Export annotations with: zotio items annotations <itemKey>
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
  - Export annotations with: zotio items annotations <itemKey>
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
  - Export annotations with: zotio items annotations <itemKey>
- ## Notes
  - 
`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := renderLogseqNoteTemplate(tc.meta, fixed)
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
		got := renderLogseqNoteTemplate(meta, fixed)
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
		got := renderLogseqNoteTemplate(meta, fixed)
		if !strings.Contains(got, "- authors::\n") {
			t.Fatalf("empty authors should emit \"- authors::\\n\", got %q", got)
		}
	})
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
