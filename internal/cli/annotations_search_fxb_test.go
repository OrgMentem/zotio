// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import "testing"

// Zotero matches annotation text and comments in normalizeForSearch form, so
// the live own-field filter must too: a hit Zotero returned for "seance" on
// "séance" was dropped while the filter compared lower-cased raw text.
func TestFxbAnnotationOwnFieldMatchUsesZoteroNormalization(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		text    string
		comment string
		want    bool
	}{
		{name: "accent stripped from field", query: "seance", text: "A séance in Paris", want: true},
		{name: "accent stripped from query", query: "Séance", text: "a seance in paris", want: true},
		{name: "decomposed accent", query: "seance", text: "A se\u0301ance", want: true},
		{name: "ligature in field", query: "fish", text: "the \ufb01sh market", want: true},
		{name: "non-decomposing letter", query: "soren", comment: "Søren's reading", want: true},
		{name: "curly apostrophe in field", query: "children's", text: "children\u2019s books", want: true},
		{name: "curly quotes in query", query: "\u2018children\u2019s\u2019", text: "'children's'", want: true},
		{name: "en dash in field", query: "1990-2000", comment: "covers 1990\u20132000", want: true},
		{name: "formatting tags stripped", query: `"homo sapiens"`, text: "<i>Homo</i> <i>sapiens</i> remains", want: true},
		{name: "every term required", query: "seance london", text: "A séance in Paris", want: false},
		{name: "different word", query: "science", text: "A séance in Paris", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := map[string]any{"data": map[string]any{
				"itemType":          "annotation",
				"annotationText":    tc.text,
				"annotationComment": tc.comment,
			}}
			if got := annotationMatchesTerms(item, annotationQueryTerms(tc.query)); got != tc.want {
				t.Fatalf("annotationMatchesTerms(text=%q comment=%q, query=%q) = %v, want %v", tc.text, tc.comment, tc.query, got, tc.want)
			}
		})
	}
}
