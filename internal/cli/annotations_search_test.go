// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"strings"
	"testing"
)

func TestFilterAnnotationSummaries(t *testing.T) {
	base := []annotationSummary{
		{Key: "A1", Text: "hello world", Comment: "", Color: "#ffd400"},
		{Key: "A2", Text: "other", Comment: "Hello there", Color: "#ff6666"},
		{Key: "A3", Text: "HELLO", Comment: "", Color: "#ffd400"},
		{Key: "A4", Text: "bye", Comment: "", Color: "#5fb236"},
		{Key: "A5", Text: "yellow thing", Comment: "", Color: "#2ea8e5"},
	}

	for _, tc := range []struct {
		name        string
		annotations []annotationSummary
		query       string
		color       string
		limit       int
		wantKeys    []string
		wantNonNil  bool
	}{
		{
			name:        "empty query matches everything",
			annotations: base,
			query:       "",
			color:       "",
			limit:       0,
			wantKeys:    []string{"A1", "A2", "A3", "A4", "A5"},
		},
		{
			name:        "empty query with spaces matches everything",
			annotations: base,
			query:       "   ",
			color:       "",
			limit:       0,
			wantKeys:    []string{"A1", "A2", "A3", "A4", "A5"},
		},
		{
			name:        "non-empty query filters on text case-insensitive",
			annotations: base,
			query:       "hello",
			color:       "",
			limit:       0,
			wantKeys:    []string{"A1", "A2", "A3"},
		},
		{
			name:        "query case-insensitive uppercase",
			annotations: base,
			query:       "HELLO",
			color:       "",
			limit:       0,
			wantKeys:    []string{"A1", "A2", "A3"},
		},
		{
			name:        "query matches comment not only text",
			annotations: base,
			query:       "there",
			color:       "",
			limit:       0,
			wantKeys:    []string{"A2"},
		},
		{
			name:        "colour filter by name resolves to hex yellow",
			annotations: base,
			query:       "",
			color:       "yellow",
			limit:       0,
			wantKeys:    []string{"A1", "A3"},
		},
		{
			name:        "colour filter case-insensitive name",
			annotations: base,
			query:       "",
			color:       "YELLOW",
			limit:       0,
			wantKeys:    []string{"A1", "A3"},
		},
		{
			name:        "colour filter by hex directly",
			annotations: base,
			query:       "",
			color:       "#ffd400",
			limit:       0,
			wantKeys:    []string{"A1", "A3"},
		},
		{
			name:        "unknown colour name matches nothing",
			annotations: base,
			query:       "",
			color:       "mauve",
			limit:       0,
			wantKeys:    []string{},
			wantNonNil:  true,
		},
		{
			name:        "limit truncates keeps leading items in order",
			annotations: base,
			query:       "",
			color:       "",
			limit:       2,
			wantKeys:    []string{"A1", "A2"},
		},
		{
			name: "colour and limit combined limit after filter",
			annotations: []annotationSummary{
				{Key: "R1", Color: "#ff6666", Text: "a"},
				{Key: "R2", Color: "#ff6666", Text: "b"},
				{Key: "Y1", Color: "#ffd400", Text: "c"},
				{Key: "Y2", Color: "#ffd400", Text: "d"},
				{Key: "Y3", Color: "#ffd400", Text: "e"},
			},
			query:    "",
			color:    "yellow",
			limit:    1,
			wantKeys: []string{"Y1"},
		},
		{
			name: "colour and limit with text filter",
			annotations: []annotationSummary{
				{Key: "R1", Color: "#ff6666", Text: "hello"},
				{Key: "Y1", Color: "#ffd400", Text: "hello"},
				{Key: "Y2", Color: "#ffd400", Text: "hello"},
				{Key: "Y3", Color: "#ffd400", Text: "other"},
			},
			query:    "hello",
			color:    "yellow",
			limit:    1,
			wantKeys: []string{"Y1"},
		},
		{
			name:        "filter matches nothing returns empty non-nil slice",
			annotations: base,
			query:       "zzz-no-match",
			color:       "",
			limit:       0,
			wantKeys:    []string{},
			wantNonNil:  true,
		},
		{
			name:        "limit with no match still non-nil",
			annotations: base,
			query:       "zzz-no-match",
			color:       "yellow",
			limit:       5,
			wantKeys:    []string{},
			wantNonNil:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := filterAnnotationSummaries(tc.annotations, tc.query, tc.color, tc.limit)
			if tc.wantNonNil && got == nil {
				t.Fatalf("filterAnnotationSummaries(%q,%q,%d) = nil, want non-nil empty slice", tc.query, tc.color, tc.limit)
			}
			if len(got) != len(tc.wantKeys) {
				t.Fatalf("filterAnnotationSummaries(%q,%q,%d) keys = %v, want %v", tc.query, tc.color, tc.limit, keysOf(got), tc.wantKeys)
			}
			for i, want := range tc.wantKeys {
				if got[i].Key != want {
					t.Fatalf("filterAnnotationSummaries(%q,%q,%d) [%d] = %q, want %q (got keys %v)", tc.query, tc.color, tc.limit, i, got[i].Key, want, keysOf(got))
				}
			}
			// Verify the source returns an empty non-nil slice, not nil, when nothing matches.
			if len(tc.wantKeys) == 0 && got == nil {
				t.Fatalf("filterAnnotationSummaries(%q,%q,%d) = nil, want empty non-nil slice", tc.query, tc.color, tc.limit)
			}
		})
	}
}

func TestFetchLimitForAnnotationSearch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		color string
		want  int
	}{
		{
			name:  "no colour returns limit unchanged",
			limit: 20,
			color: "",
			want:  20,
		},
		{
			name:  "whitespace colour treated as no colour",
			limit: 20,
			color: "   ",
			want:  20,
		},
		{
			// When a colour filter is requested the API result is post-filtered
			// in memory (annotationColorMatches), so many fetched rows will be
			// discarded. The fetch limit must be widened to 100 when the caller
			// asked for fewer than 100, otherwise a limit of e.g. 10 would return
			// fewer than 10 matches even when more exist, because the colour drop
			// happens after the API paging.
			name:  "colour with limit below 100 widens to 100",
			limit: 20,
			color: "yellow",
			want:  100,
		},
		{
			name:  "colour with limit below 100 widens single digit",
			limit: 1,
			color: "red",
			want:  100,
		},
		{
			name:  "colour with limit exactly 100 unchanged",
			limit: 100,
			color: "yellow",
			want:  100,
		},
		{
			name:  "colour with limit above 100 unchanged",
			limit: 150,
			color: "yellow",
			want:  150,
		},
		{
			name:  "colour with zero limit returns zero",
			limit: 0,
			color: "yellow",
			want:  0,
		},
		{
			name:  "colour with negative limit returns zero",
			limit: -5,
			color: "yellow",
			want:  0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fetchLimitForAnnotationSearch(tc.limit, tc.color)
			if got != tc.want {
				t.Fatalf("fetchLimitForAnnotationSearch(%d,%q) = %d, want %d", tc.limit, tc.color, got, tc.want)
			}
		})
	}
}

// Verify the colour mapping the source uses: name lower-cased and trimmed to hex.
func TestAnnotationColorHexMapping(t *testing.T) {
	cases := map[string]string{
		"yellow": "#ffd400",
		"red":    "#ff6666",
		"green":  "#5fb236",
		"blue":   "#2ea8e5",
		"purple": "#a28ae5",
		"orange": "#f19837",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			got := annotationColorHex(name)
			if got != want {
				t.Fatalf("annotationColorHex(%q) = %q, want %q", name, got, want)
			}
			// Case-insensitive and trimmed: source folds with ToLower+TrimSpace.
			got2 := annotationColorHex(strings.ToUpper(" " + name + " "))
			if got2 != want {
				t.Fatalf("annotationColorHex folded %q = %q, want %q", name, got2, want)
			}
		})
	}
	// Unknown name is returned as-is (lower-cased trimmed identity is not mapped).
	t.Run("unknown", func(t *testing.T) {
		if got := annotationColorHex("mauve"); got != "mauve" {
			t.Fatalf("annotationColorHex(%q) = %q, want mauve", "mauve", got)
		}
	})
}

func TestAnnotationColorMatches(t *testing.T) {
	for _, tc := range []struct {
		name      string
		actual    string
		requested string
		want      bool
	}{
		{name: "empty requested matches anything", actual: "#ffd400", requested: "", want: true},
		{name: "exact hex match", actual: "#ffd400", requested: "#ffd400", want: true},
		{name: "name resolves to hex", actual: "#ffd400", requested: "yellow", want: true},
		{name: "name case-insensitive", actual: "#ffd400", requested: "YELLOW", want: true},
		{name: "name with spaces trimmed", actual: "#ffd400", requested: " yellow ", want: true},
		{name: "actual case-insensitive", actual: "#FFD400", requested: "yellow", want: true},
		{name: "mismatch", actual: "#ff6666", requested: "yellow", want: false},
		{name: "unknown name does not match hex", actual: "#ffd400", requested: "mauve", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := annotationColorMatches(tc.actual, tc.requested)
			if got != tc.want {
				t.Fatalf("annotationColorMatches(%q,%q) = %v, want %v", tc.actual, tc.requested, got, tc.want)
			}
		})
	}
}
