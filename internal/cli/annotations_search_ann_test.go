// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// annRunLiveSearch runs `annotations search` against the live fake and
// returns the result keys in output order.
func annRunLiveSearch(t *testing.T, serverURL string, args ...string) []string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_BASE_URL", serverURL+"/users/0")

	cmd := newAnnotationsSearchCmd(&rootFlags{asJSON: true, dataSource: "live", noCache: true})
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("annotations search %v: %v", args, err)
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
	return keys
}

// Zotero's default quick-search mode (titleCreatorYear) never matches
// annotation text, and qmode=everything also returns annotations whose
// attachment full text matches. The live path must ask for everything and
// keep only annotations whose own text, comment or tags match every term.
func TestAnnAnnotationsSearchLiveMatchesAnnotationOwnFields(t *testing.T) {
	const fixture = `[
		{"key":"ANNTEXT","version":1,"data":{"key":"ANNTEXT","itemType":"annotation","parentItem":"PDF1","annotationText":"Institutional Trust model results","tags":[]}},
		{"key":"ANNCOMM","version":1,"data":{"key":"ANNCOMM","itemType":"annotation","parentItem":"PDF1","annotationText":"unrelated","annotationComment":"about trust","tags":[]}},
		{"key":"ANNTAG","version":1,"data":{"key":"ANNTAG","itemType":"annotation","parentItem":"PDF1","annotationText":"unrelated","tags":[{"tag":"trust"}]}},
		{"key":"ANNFULL","version":1,"data":{"key":"ANNFULL","itemType":"annotation","parentItem":"PDF2","annotationText":"unrelated passage","tags":[]}},
		{"key":"ANNSPLIT","version":1,"data":{"key":"ANNSPLIT","itemType":"annotation","parentItem":"PDF1","annotationText":"trust in the model","tags":[]}}
	]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		if r.URL.Path != "/users/0/items" || q.Get("itemType") != "annotation" {
			t.Errorf("unexpected request %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		// titleCreatorYear (the default) never looks at annotation text.
		if q.Get("qmode") != "everything" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(fixture))
	}))
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{"word matches text, comment and tag case-insensitively", "TRUST", "ANNTEXT,ANNCOMM,ANNTAG,ANNSPLIT"},
		{"every word must match", "trust model", "ANNTEXT,ANNSPLIT"},
		{"quoted phrase matches whole", `"trust model"`, "ANNTEXT"},
		{"full-text-only hit is dropped", "methodology", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(annRunLiveSearch(t, srv.URL, tc.query, "--limit", "0"), ",")
			if got != tc.want {
				t.Fatalf("query %q: keys = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

// --color filters client-side, so a capped fetch dropped matching
// highlights that sat past the cap. The live path must page until the API is
// exhausted (or --limit color matches are found).
func TestAnnAnnotationsSearchLiveColorPagesPastFetchCap(t *testing.T) {
	const total = 250
	anns := make([]string, 0, total)
	for i := range total {
		color := "#ff6666"
		if i >= 200 && i < 205 {
			color = "#ffd400"
		}
		anns = append(anns, fmt.Sprintf(`{"key":"A%03d","version":1,"data":{"key":"A%03d","itemType":"annotation","parentItem":"PDF","annotationText":"needle","annotationColor":%q}}`, i, i, color))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		if r.URL.Path != "/users/0/items" || q.Get("itemType") != "annotation" {
			t.Errorf("unexpected request %s", r.URL.String())
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		start, _ := strconv.Atoi(q.Get("start"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 || limit > 100 {
			limit = 100
		}
		start = min(start, total)
		end := min(start+limit, total)
		_, _ = w.Write([]byte("[" + strings.Join(anns[start:end], ",") + "]"))
	}))
	t.Cleanup(srv.Close)

	got := strings.Join(annRunLiveSearch(t, srv.URL, "needle", "--color", "yellow", "--limit", "3"), ",")
	if got != "A200,A201,A202" {
		t.Fatalf("keys = %q, want the first three yellow highlights past the first 100 hits", got)
	}
}
