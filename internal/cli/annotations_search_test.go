// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"zotio/internal/store"
)

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

// A global LIMIT used to drop in-collection hits when a higher-ranked hit sat
// outside the collection; the scope must filter before the limit.
func TestAnnotationsSearchLocalScopeFiltersBeforeLimit(t *testing.T) {
	seedAnnotationSearchStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"INP","version":1,"data":{"key":"INP","itemType":"journalArticle","title":"In","collections":["COLL"]}}`),
		json.RawMessage(`{"key":"OUTP","version":1,"data":{"key":"OUTP","itemType":"journalArticle","title":"Out"}}`),
		json.RawMessage(`{"key":"INPDF","version":1,"data":{"key":"INPDF","itemType":"attachment","parentItem":"INP"}}`),
		json.RawMessage(`{"key":"OUTPDF","version":1,"data":{"key":"OUTPDF","itemType":"attachment","parentItem":"OUTP"}}`),
		json.RawMessage(`{"key":"OUTA","version":1,"data":{"key":"OUTA","itemType":"annotation","parentItem":"OUTPDF","annotationText":"needle needle needle needle"}}`),
		json.RawMessage(`{"key":"INA1","version":1,"data":{"key":"INA1","itemType":"annotation","parentItem":"INPDF","annotationText":"needle needle among a few words"}}`),
		json.RawMessage(`{"key":"INA2","version":1,"data":{"key":"INA2","itemType":"annotation","parentItem":"INPDF","annotationText":"a long passage with one needle among many other words here"}}`),
	})
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"needle", "--scope", "collection:COLL", "--limit", "1"}, "INA1"},
		{[]string{"needle", "--scope", "collection:COLL"}, "INA1,INA2"},
		{[]string{"needle", "--scope", "collection:EMPTY"}, ""},
	} {
		cmd := newAnnotationsSearchCmd(&rootFlags{asJSON: true, dataSource: "local"})
		cmd.SetArgs(tc.args)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		var env struct {
			Results []annotationSummary `json:"results"`
		}
		if err := json.Unmarshal(out.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v; output=%q", err, out.String())
		}
		keys := make([]string, 0, len(env.Results))
		for _, r := range env.Results {
			keys = append(keys, r.Key)
		}
		if got := strings.Join(keys, ","); got != tc.want {
			t.Fatalf("%v: keys = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// The live API cannot filter annotations by grand-parent, so the command
// resolves attachment parents in itemKey batches of 50 and filters before
// --limit.
func TestAnnotationsSearchLiveScopeFiltersByAttachmentParent(t *testing.T) {
	seedAnnotationSearchStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"INP","version":1,"data":{"key":"INP","itemType":"journalArticle","title":"In","collections":["COLL"]}}`),
	})
	const n = 60
	anns := make([]string, 0, n)
	for i := range n {
		anns = append(anns, fmt.Sprintf(`{"key":"A%02d","version":1,"data":{"key":"A%02d","itemType":"annotation","parentItem":"PDF%02d","annotationText":"needle","annotationColor":"#ffd400"}}`, i, i, i))
	}
	var mu sync.Mutex
	lookups := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/users/0/items" && q.Get("itemType") == "annotation":
			_, _ = w.Write([]byte("[" + strings.Join(anns, ",") + "]"))
		case r.URL.Path == "/users/0/items" && q.Get("itemKey") != "":
			mu.Lock()
			lookups++
			mu.Unlock()
			keys := strings.Split(q.Get("itemKey"), ",")
			if len(keys) > 50 {
				t.Errorf("itemKey batch of %d keys, want <= 50", len(keys))
			}
			atts := make([]string, 0, len(keys))
			for _, key := range keys {
				// Every tenth attachment belongs to the in-scope item.
				parent := "OUTP"
				var i int
				if _, err := fmt.Sscanf(key, "PDF%d", &i); err == nil && i%10 == 0 {
					parent = "INP"
				}
				atts = append(atts, fmt.Sprintf(`{"key":%q,"data":{"key":%q,"itemType":"attachment","parentItem":%q}}`, key, key, parent))
			}
			_, _ = w.Write([]byte("[" + strings.Join(atts, ",") + "]"))
		default:
			t.Errorf("unexpected request %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	cmd := newAnnotationsSearchCmd(&rootFlags{asJSON: true, dataSource: "live", noCache: true})
	cmd.SetArgs([]string{"needle", "--scope", "collection:COLL", "--limit", "5"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("live scoped search: %v", err)
	}
	var env struct {
		Results []annotationSummary `json:"results"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v; output=%q", err, out.String())
	}
	keys := make([]string, 0, len(env.Results))
	for _, r := range env.Results {
		keys = append(keys, r.Key)
	}
	if got := strings.Join(keys, ","); got != "A00,A10,A20,A30,A40" {
		t.Fatalf("keys = %q, want the first five in-scope annotations", got)
	}
	if lookups > (n+49)/50 {
		t.Fatalf("attachment lookups = %d, want <= %d", lookups, (n+49)/50)
	}
}

// A standalone attachment has no parentItem, so it owns its annotations: a
// scope naming it, directly or as a collection member, must keep them on
// both the local and the live path.
func TestAnnotationsSearchScopeKeepsStandaloneAttachmentAnnotations(t *testing.T) {
	seedAnnotationSearchStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"SOLOPDF","version":1,"data":{"key":"SOLOPDF","itemType":"attachment","title":"Solo PDF","collections":["COLL"]}}`),
		json.RawMessage(`{"key":"SOLOA","version":1,"data":{"key":"SOLOA","itemType":"annotation","parentItem":"SOLOPDF","annotationText":"needle in a standalone pdf"}}`),
	})
	var mu sync.Mutex
	searches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/users/0/items" && q.Get("itemType") == "annotation":
			mu.Lock()
			searches++
			mu.Unlock()
			_, _ = w.Write([]byte(`[{"key":"SOLOA","version":1,"data":{"key":"SOLOA","itemType":"annotation","parentItem":"SOLOPDF","annotationText":"needle in a standalone pdf"}}]`))
		case r.URL.Path == "/users/0/items" && q.Get("itemKey") == "SOLOPDF":
			// A standalone attachment carries no parentItem field.
			_, _ = w.Write([]byte(`[{"key":"SOLOPDF","version":1,"data":{"key":"SOLOPDF","itemType":"attachment","title":"Solo PDF","collections":["COLL"]}}]`))
		default:
			t.Errorf("unexpected request %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	for _, tc := range []struct {
		source string
		args   []string
	}{
		{"local", []string{"needle", "--scope", "item:SOLOPDF"}},
		{"local", []string{"needle", "--scope", "collection:COLL"}},
		{"live", []string{"needle", "--scope", "item:SOLOPDF"}},
		{"live", []string{"needle", "--scope", "collection:COLL"}},
	} {
		cmd := newAnnotationsSearchCmd(&rootFlags{asJSON: true, dataSource: tc.source, noCache: true})
		cmd.SetArgs(tc.args)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%s %v: %v", tc.source, tc.args, err)
		}
		var env struct {
			Results []annotationSummary `json:"results"`
		}
		if err := json.Unmarshal(out.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v; output=%q", err, out.String())
		}
		keys := make([]string, 0, len(env.Results))
		for _, r := range env.Results {
			keys = append(keys, r.Key)
		}
		if got := strings.Join(keys, ","); got != "SOLOA" {
			t.Fatalf("%s %v: keys = %q, want SOLOA", tc.source, tc.args, got)
		}
	}
	// The local cases must not reach the API.
	if searches != 2 {
		t.Fatalf("live annotation searches = %d, want 2", searches)
	}
}

func TestAnnotationsSearchLiveScopeWithoutMirrorFails(t *testing.T) {
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	cmd := newAnnotationsSearchCmd(&rootFlags{dataSource: "auto"})
	cmd.SetArgs([]string{"needle", "--refresh", "--scope", "collection:COLL"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "zotio sync") {
		t.Fatalf("scoped --refresh without a mirror: err = %v, want zotio sync guidance", err)
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
