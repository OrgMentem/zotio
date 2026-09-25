// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/store"
)

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

func TestAnnotationsSearchReadsLocalStoreWithLocalProvenance(t *testing.T) {
	seedAnnotationSearchStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"PAPER1","version":1,"data":{"key":"PAPER1","itemType":"journalArticle","title":"Needle Paper"}}`),
		json.RawMessage(`{"key":"PDF1","version":1,"data":{"key":"PDF1","itemType":"attachment","parentItem":"PAPER1"}}`),
		json.RawMessage(`{"key":"ANN1","version":1,"data":{"key":"ANN1","itemType":"annotation","parentItem":"PDF1","annotationText":"Local needle passage","annotationComment":"kept","annotationColor":"#ffd400"}}`),
		json.RawMessage(`{"key":"ANN2","version":1,"data":{"key":"ANN2","itemType":"annotation","parentItem":"PDF2","annotationText":"Unrelated passage","annotationColor":"#ff6666"}}`),
	})

	for _, dataSource := range []string{"auto", "local"} {
		t.Run(dataSource, func(t *testing.T) {
			cmd := newAnnotationsSearchCmd(&rootFlags{asJSON: true, dataSource: dataSource})
			cmd.SetArgs([]string{"needle"})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("annotations search with %s source: %v", dataSource, err)
			}

			var env struct {
				Results []annotationSummary `json:"results"`
				Meta    DataProvenance      `json:"meta"`
			}
			if err := json.Unmarshal(out.Bytes(), &env); err != nil {
				t.Fatalf("decode annotations search envelope: %v; output=%q", err, out.String())
			}
			if len(env.Results) != 1 || env.Results[0].Key != "ANN1" || env.Results[0].Text != "Local needle passage" ||
				env.Results[0].ItemKey != "PAPER1" || env.Results[0].ItemTitle != "Needle Paper" {
				t.Fatalf("results = %+v, want only local annotation ANN1", env.Results)
			}
			if env.Meta.Source != "local" || env.Meta.ResourceType != "annotations" {
				t.Fatalf("provenance = %+v, want local annotations", env.Meta)
			}
		})
	}
}

func TestAnnotationsSearchLocalWithoutMirrorHasSyncGuidance(t *testing.T) {
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	cmd := newAnnotationsSearchCmd(&rootFlags{dataSource: "local"})
	cmd.SetArgs([]string{"needle"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("annotations search --data-source local without a mirror succeeded")
	}
	if !strings.Contains(err.Error(), "zotio sync") {
		t.Fatalf("missing-mirror error = %q, want zotio sync guidance", err)
	}
}

func TestAnnotationsSearchRejectsRefreshWithLocalSource(t *testing.T) {
	cmd := newAnnotationsSearchCmd(&rootFlags{dataSource: "local"})
	cmd.SetArgs([]string{"needle", "--refresh"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("annotations search accepted --refresh with --data-source local")
	}
	if !strings.Contains(err.Error(), "--refresh cannot be used with --data-source local") {
		t.Fatalf("conflict error = %q", err)
	}
	if got := ExitCode(err); got != 2 {
		t.Fatalf("conflict exit code = %d, want 2", got)
	}
}

func seedAnnotationSearchStore(t *testing.T, items []json.RawMessage) {
	t.Helper()
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open annotation search store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		_ = db.Close()
		t.Fatalf("seed annotation search items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close annotation search store: %v", err)
	}
}
