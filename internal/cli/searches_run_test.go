// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/store"
)

// seedSearchesRunStore mirrors what `zotio sync` stores for saved searches: the
// DEFINITION only. There is no membership row to find, which is why the result
// read cannot be served locally.
func seedSearchesRunStore(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir store dir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("searches", []json.RawMessage{
		json.RawMessage(`{"key":"SK","version":42,"data":{"key":"SK","name":"live only search","conditions":[{"condition":"title","operator":"contains","value":"x"}]}}`),
	}); err != nil {
		t.Fatalf("seed saved search: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

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
	return runSearchesRunTestCmdWithFlags(t, srv, &rootFlags{asJSON: true}, args...)
}

func runSearchesRunTestCmdWithFlags(t *testing.T, srv *searchesRunTestServer, flags *rootFlags, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newSearchesRunCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// TestSearchesRunRefusesUnexecutableSearchWithPrecondition is the regression
// guard for two bugs at once.
//
// The first is the removed q=""+savedSearch fallback: returning the library
// here would be the worst possible failure, because the caller cannot
// distinguish it from a real saved-search cohort, so agents would audit,
// export, or mutate every item.
//
// The second is silent degradation. This used to exit 0 with an
// ok-shaped envelope, so a plane that cannot execute the search was
// indistinguishable, to an exit-code-checking caller, from a search that ran
// and matched nothing.
func TestSearchesRunRefusesUnexecutableSearchWithPrecondition(t *testing.T) {
	library := []string{"AAAA1111", "BBBB2222", "CCCC3333", "DDDD4444"}
	srv := newSearchesRunTestServer(t, http.StatusNotFound, library)

	out, _, err := runSearchesRunTestCmd(t, srv, "SK")
	if err == nil {
		t.Fatalf("searches run succeeded against a plane that cannot execute the search; output %q", out)
	}
	assertPreconditionExitCode(t, err, 9)

	var env preconditionUnmetEnvelope
	if decodeErr := json.Unmarshal([]byte(out), &env); decodeErr != nil {
		t.Fatalf("output is not a precondition_unmet envelope; decode %q: %v", out, decodeErr)
	}
	if env.Kind != "precondition_unmet" || env.Precondition != preconditionLiveLocalAPI {
		t.Fatalf("envelope = %+v, want precondition_unmet/live_local_api", env)
	}
	if env.Capability != "searches run" {
		t.Fatalf("capability = %q, want searches run", env.Capability)
	}
	if len(env.Remediation) == 0 {
		t.Fatalf("refusal carries no remediation: %+v", env)
	}
	// The refusal names the search, using the mirrored/live definition, so the
	// operator does not have to map a key back to a search by hand.
	if !strings.Contains(env.Detail, "my saved search") {
		t.Fatalf("detail does not name the saved search: %q", env.Detail)
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
}

// TestSearchesRunReturnsEmptyArrayOnEmptyResults covers the other trigger for
// the old fallback: the endpoint exists but returns an empty page. An empty
// saved search is a legitimate answer, so it must neither become a library dump
// NOR be reported as an unexecutable search — a caller has to be able to tell
// "ran, 0 hits" from "could not run".
func TestSearchesRunReturnsEmptyArrayOnEmptyResults(t *testing.T) {
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

	out, errOut, err := runSearchesRunTestCmd(t, srv, "SK")
	if err != nil {
		t.Fatalf("searches run returned error: %v", err)
	}
	if len(srv.itemsRequests) != 0 {
		t.Fatalf("empty saved search fell back to a broad /items query: %v", srv.itemsRequests)
	}
	if strings.Contains(out, "leaked") {
		t.Fatalf("library item leaked into output for an empty saved search: %q", out)
	}
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("output is not a JSON array (%v); an empty search must not become a refusal: %q", err, out)
	}
	if len(items) != 0 {
		t.Fatalf("output = %q, want an empty array", out)
	}
	if strings.Contains(errOut, "precondition") {
		t.Fatalf("a working endpoint with 0 hits must not report a precondition: %q", errOut)
	}
}

// TestSearchesRunLocalDataSourceRefusesInsteadOfReadingLive is the guard for
// the other half of the same bug: --data-source local used to be ignored, so
// the command silently returned LIVE results under a local label. Result
// membership is not mirrored, so the only honest answer is a refusal.
func TestSearchesRunLocalDataSourceRefusesInsteadOfReadingLive(t *testing.T) {
	srv := newSearchesRunTestServer(t, http.StatusOK, nil)
	var resultRequests int
	srv.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/0/searches/SK":
			_, _ = fmt.Fprint(w, `{"key":"SK","version":42,"data":{"name":"live only search"}}`)
		case "/users/0/searches/SK/items":
			resultRequests++
			_, _ = fmt.Fprint(w, `[{"key":"AAAA1111","version":42,"data":{"title":"live result"}}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// A store is present but holds no saved searches, which is exactly the
	// state a synced mirror is in: sync stores definitions, never membership.
	seedSearchesRunStore(t)

	out, _, err := runSearchesRunTestCmdWithFlags(t, srv, &rootFlags{asJSON: true, dataSource: "local"}, "SK")
	if err == nil {
		t.Fatalf("searches run --data-source local succeeded; output %q", out)
	}
	assertPreconditionExitCode(t, err, 9)
	if resultRequests != 0 {
		t.Fatalf("--data-source local still read the live results endpoint %d time(s)", resultRequests)
	}
	if strings.Contains(out, "live result") {
		t.Fatalf("live results leaked into a --data-source local run: %q", out)
	}
	var env preconditionUnmetEnvelope
	if decodeErr := json.Unmarshal([]byte(out), &env); decodeErr != nil {
		t.Fatalf("output is not a precondition_unmet envelope; decode %q: %v", out, decodeErr)
	}
	if !strings.Contains(env.Detail, "definitions only") {
		t.Fatalf("detail does not explain that only definitions are mirrored: %q", env.Detail)
	}
}
