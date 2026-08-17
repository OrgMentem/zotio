// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// searchesRunTestServer models the plane that made the removed fallback unsafe:
// /searches/{key}/items is absent, and /items ignores the unsupported
// savedSearch parameter (Zotero silently drops unknown query params) so an
// unfiltered library is returned with HTTP 200.
type searchesRunTestServer struct {
	server        *httptest.Server
	libraryItems  []string
	itemsRequests []string
}

func newSearchesRunTestServer(t *testing.T, resultsStatus int, libraryItems []string) *searchesRunTestServer {
	t.Helper()
	ts := &searchesRunTestServer{libraryItems: libraryItems}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/users/0/searches/SK" && r.Method == http.MethodGet:
			w.Header().Set("Last-Modified-Version", "42")
			_, _ = fmt.Fprint(w, `{"key":"SK","version":42,"data":{"name":"my saved search"}}`)
		case r.URL.Path == "/users/0/searches/SK/items" && r.Method == http.MethodGet:
			// The saved-search results endpoint is unavailable on this plane.
			w.WriteHeader(resultsStatus)
			_, _ = fmt.Fprint(w, `{"message":"No endpoint found"}`)
		case r.URL.Path == "/users/0/items" && r.Method == http.MethodGet:
			// Ignores savedSearch entirely and answers with the whole library.
			ts.itemsRequests = append(ts.itemsRequests, r.URL.RawQuery)
			items := make([]string, 0, len(ts.libraryItems))
			for _, key := range ts.libraryItems {
				items = append(items, fmt.Sprintf(`{"key":%q,"version":42,"data":{"title":%q}}`, key, key))
			}
			w.Header().Set("Last-Modified-Version", "42")
			_, _ = fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func runSearchesRunTestCmd(t *testing.T, srv *searchesRunTestServer, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newSearchesRunCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// TestSearchesRunReportsUnavailableInsteadOfWholeLibrary is the regression guard
// for the removed q=""+savedSearch fallback. Returning the library here would be
// the worst possible failure: the caller cannot distinguish it from a real
// saved-search cohort, so agents would audit, export, or mutate every item.
func TestSearchesRunReportsUnavailableInsteadOfWholeLibrary(t *testing.T) {
	library := []string{"AAAA1111", "BBBB2222", "CCCC3333", "DDDD4444"}
	srv := newSearchesRunTestServer(t, http.StatusNotFound, library)

	out, errOut, err := runSearchesRunTestCmd(t, srv, "SK")
	if err != nil {
		t.Fatalf("searches run returned error: %v (stderr %q)", err, errOut)
	}

	var fallback searchesRunFallback
	if decodeErr := json.Unmarshal([]byte(out), &fallback); decodeErr != nil {
		t.Fatalf("output is not an unavailable envelope; decode %q: %v", out, decodeErr)
	}
	if fallback.ResultsAvailable {
		t.Fatalf("results_available = true, want false; envelope %q", out)
	}
	if !strings.Contains(fallback.Message, "unavailable") {
		t.Fatalf("message does not report unavailability: %q", fallback.Message)
	}

	// The whole point: no broad /items request may be made at all.
	if len(srv.itemsRequests) != 0 {
		t.Fatalf("searches run fell back to a broad /items query: %v", srv.itemsRequests)
	}
	// And no library item may appear anywhere in the output.
	for _, key := range library {
		if strings.Contains(out, key) {
			t.Fatalf("unfiltered library item %s leaked into searches run output: %q", key, out)
		}
	}
	// The saved search itself is still reported so the caller can see what failed.
	if len(fallback.Search) == 0 || !strings.Contains(string(fallback.Search), "my saved search") {
		t.Fatalf("saved search metadata missing from envelope: %q", out)
	}
	if !strings.Contains(errOut, "warning:") {
		t.Fatalf("expected a loud stderr warning, got %q", errOut)
	}
}

// TestSearchesRunReportsUnavailableOnEmptyResults covers the other trigger for
// the old fallback: the endpoint exists but returns an empty page. An empty
// saved search is a legitimate answer, so it must not become a library dump.
func TestSearchesRunReportsUnavailableOnEmptyResults(t *testing.T) {
	srv := newSearchesRunTestServer(t, http.StatusOK, []string{"AAAA1111", "BBBB2222"})
	srv.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/0/searches/SK":
			_, _ = fmt.Fprint(w, `{"key":"SK","version":42,"data":{"name":"empty search"}}`)
		case "/users/0/searches/SK/items":
			_, _ = fmt.Fprint(w, `[]`)
		case "/users/0/items":
			srv.itemsRequests = append(srv.itemsRequests, r.URL.RawQuery)
			_, _ = fmt.Fprint(w, `[{"key":"AAAA1111","version":42,"data":{"title":"leaked"}}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	out, _, err := runSearchesRunTestCmd(t, srv, "SK")
	if err != nil {
		t.Fatalf("searches run returned error: %v", err)
	}
	if len(srv.itemsRequests) != 0 {
		t.Fatalf("empty saved search fell back to a broad /items query: %v", srv.itemsRequests)
	}
	if strings.Contains(out, "leaked") {
		t.Fatalf("library item leaked into output for an empty saved search: %q", out)
	}
}
