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
	"reflect"
	"strings"
	"testing"

	"zotio/internal/store"
)

// seedSearchesRunStore mirrors what `zotio sync` stores: the saved-search
// DEFINITION SK with the given conditions (JSON array text) and the given
// item payloads. Local evaluation reads exactly these rows.
func seedSearchesRunStore(t *testing.T, conditions string, items ...json.RawMessage) {
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
	definition := `{"key":"SK","version":42,"data":{"key":"SK","name":"mirrored search","conditions":` + conditions + `}}`
	if _, _, err := db.UpsertBatch("searches", []json.RawMessage{json.RawMessage(definition)}); err != nil {
		t.Fatalf("seed saved search: %v", err)
	}
	if len(items) > 0 {
		if _, _, err := db.UpsertBatch("items", items); err != nil {
			t.Fatalf("seed items: %v", err)
		}
	}
	if err := db.SaveSyncState("items", "", len(items)); err != nil {
		t.Fatalf("seed items sync state: %v", err)
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
	// No mirror: the auto fallback has nothing to evaluate and must refuse.
	t.Setenv("HOME", t.TempDir())
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

// searchesRunLiveResultsServer serves a definition for SK and counts reads of
// the live results endpoint; resultsStatus 0 means the endpoint answers an
// empty page, anything else is returned as the endpoint's status.
func searchesRunLiveResultsServer(t *testing.T, definition string, resultsStatus int, resultRequests *int) *searchesRunTestServer {
	t.Helper()
	srv := newSearchesRunTestServer(t, http.StatusOK, nil)
	srv.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/0/searches/SK":
			_, _ = fmt.Fprint(w, definition)
		case "/users/0/searches/SK/items":
			*resultRequests++
			if resultsStatus != 0 {
				w.WriteHeader(resultsStatus)
				return
			}
			_, _ = fmt.Fprint(w, `[{"key":"AAAA1111","version":42,"data":{"title":"live result"}}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return srv
}

// searchesRunOutputKeys decodes a searches run result array into item keys.
func searchesRunOutputKeys(t *testing.T, out string) []string {
	t.Helper()
	var items []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("output is not a JSON array of items (%v): %q", err, out)
	}
	keys := make([]string, len(items))
	for i, item := range items {
		keys[i] = item.Key
	}
	return keys
}

func searchesRunPreconditionEnvelope(t *testing.T, out string) preconditionUnmetEnvelope {
	t.Helper()
	var env preconditionUnmetEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("output is not a precondition_unmet envelope; decode %q: %v", out, err)
	}
	if env.Kind != "precondition_unmet" || env.Precondition != preconditionLiveLocalAPI {
		t.Fatalf("envelope = %+v, want precondition_unmet/live_local_api", env)
	}
	return env
}

// TestSearchesRunLocalDataSourceEvaluatesTheMirroredSearch: --data-source
// local reproduces the search from the synced definition and items, never
// reading the live results endpoint, and returns the stored item payloads in
// the item-list order (dateModified descending, key ascending).
func TestSearchesRunLocalDataSourceEvaluatesTheMirroredSearch(t *testing.T) {
	var resultRequests int
	srv := searchesRunLiveResultsServer(t, `{"key":"SK","version":42,"data":{"name":"live only search"}}`, 0, &resultRequests)
	seedSearchesRunStore(t,
		ssConds([3]string{"title", "contains", "learning"}, [3]string{"includeChildren", "true", ""}),
		ssItem("AAAA1111", "journalArticle", map[string]any{"title": "Deep Learning", "dateModified": "2023-01-01T00:00:00Z"}),
		ssItem("BBBB2222", "journalArticle", map[string]any{"title": "Learning Theory", "dateModified": "2024-01-01T00:00:00Z"}),
		ssItem("CCCC3333", "journalArticle", map[string]any{"title": "Gardening", "dateModified": "2025-01-01T00:00:00Z"}),
		ssItem("PDF00001", "attachment", map[string]any{"title": "Full text", "parentItem": "AAAA1111", "dateModified": "2022-01-01T00:00:00Z"}),
	)

	out, _, err := runSearchesRunTestCmdWithFlags(t, srv, &rootFlags{asJSON: true, dataSource: "local"}, "SK")
	if err != nil {
		t.Fatalf("searches run --data-source local: %v\noutput: %s", err, out)
	}
	if resultRequests != 0 {
		t.Fatalf("--data-source local read the live results endpoint %d time(s)", resultRequests)
	}
	if got, want := searchesRunOutputKeys(t, out), []string{"BBBB2222", "AAAA1111", "PDF00001"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	if !strings.Contains(out, `"data"`) || !strings.Contains(out, "Deep Learning") {
		t.Fatalf("local results are not the stored item payloads: %q", out)
	}
	if strings.Contains(out, "live result") {
		t.Fatalf("live results leaked into a --data-source local run: %q", out)
	}
}

// A search that matches nothing is an empty array, not a refusal.
func TestSearchesRunLocalDataSourceEmptyResultIsAnEmptyArray(t *testing.T) {
	srv := newSearchesRunTestServer(t, http.StatusOK, nil)
	seedSearchesRunStore(t,
		ssConds([3]string{"title", "contains", "nothing matches this"}),
		ssItem("AAAA1111", "journalArticle", map[string]any{"title": "Deep Learning"}),
	)
	out, _, err := runSearchesRunTestCmdWithFlags(t, srv, &rootFlags{asJSON: true, dataSource: "local"}, "SK")
	if err != nil {
		t.Fatalf("searches run --data-source local: %v\noutput: %s", err, out)
	}
	if got := strings.TrimSpace(out); got != "[]" {
		t.Fatalf("output = %q, want []", got)
	}
}

// TestSearchesRunLocalDataSourceRefusesAnUnsupportedSearch: a condition the
// evaluator cannot reproduce keeps the live_local_api refusal, names the
// condition, and never returns a partial or live result.
func TestSearchesRunLocalDataSourceRefusesAnUnsupportedSearch(t *testing.T) {
	var resultRequests int
	srv := searchesRunLiveResultsServer(t, `{"key":"SK","version":42,"data":{"name":"live only search"}}`, 0, &resultRequests)
	seedSearchesRunStore(t,
		ssConds([3]string{"title", "contains", "learning"}, [3]string{"fulltextContent", "contains", "kernel"}),
		ssItem("AAAA1111", "journalArticle", map[string]any{"title": "Deep Learning"}),
	)

	out, _, err := runSearchesRunTestCmdWithFlags(t, srv, &rootFlags{asJSON: true, dataSource: "local"}, "SK")
	if err == nil {
		t.Fatalf("searches run --data-source local succeeded; output %q", out)
	}
	assertPreconditionExitCode(t, err, 9)
	if resultRequests != 0 {
		t.Fatalf("--data-source local still read the live results endpoint %d time(s)", resultRequests)
	}
	if strings.Contains(out, "live result") || strings.Contains(out, "AAAA1111") {
		t.Fatalf("a result leaked into a refused local run: %q", out)
	}
	env := searchesRunPreconditionEnvelope(t, out)
	if !strings.Contains(env.Detail, "fulltextContent") {
		t.Fatalf("detail does not name the unsupported condition: %q", env.Detail)
	}
}

// A mirror that holds the search definition but never completed an items
// sync (a write-through cache of the definition alone) must refuse, not print
// [] as if the search ran against the library.
func TestSearchesRunLocalDataSourceRefusesWhenItemsNeverSynced(t *testing.T) {
	srv := newSearchesRunTestServer(t, http.StatusOK, nil)
	t.Setenv("HOME", t.TempDir())
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir store dir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	definition := `{"key":"SK","version":42,"data":{"key":"SK","name":"mirrored search","conditions":` + ssConds([3]string{"title", "contains", "x"}) + `}}`
	if _, _, err := db.UpsertBatch("searches", []json.RawMessage{json.RawMessage(definition)}); err != nil {
		t.Fatalf("seed saved search: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	out, _, err := runSearchesRunTestCmdWithFlags(t, srv, &rootFlags{asJSON: true, dataSource: "local"}, "SK")
	if err == nil {
		t.Fatalf("searches run against a never-synced mirror succeeded; output %q", out)
	}
	assertPreconditionExitCode(t, err, 9)
	env := searchesRunPreconditionEnvelope(t, out)
	if !strings.Contains(env.Detail, "items sync") {
		t.Fatalf("detail does not mention the missing items sync: %q", env.Detail)
	}
}

// A saved search absent from the mirror is the existing not-found error from
// the keyed definition read, not a precondition and not an empty answer.
func TestSearchesRunLocalDataSourceAbsentSearchIsNotFound(t *testing.T) {
	srv := newSearchesRunTestServer(t, http.StatusOK, nil)
	seedSearchesRunStore(t, ssConds([3]string{"title", "contains", "x"}))
	out, _, err := runSearchesRunTestCmdWithFlags(t, srv, &rootFlags{asJSON: true, dataSource: "local"}, "MISSING")
	if err == nil {
		t.Fatalf("searches run of an unmirrored search succeeded; output %q", out)
	}
	if !strings.Contains(err.Error(), "not found in local store") {
		t.Fatalf("error = %v, want the local not-found error", err)
	}
}

// In auto mode a plane that cannot execute the search (HTTP 404 on the results
// endpoint) falls back to the mirror, and still never issues a broad /items
// query.
func TestSearchesRunAutoFallsBackToLocalEvaluation(t *testing.T) {
	var resultRequests int
	definition := `{"key":"SK","version":42,"data":{"name":"my saved search","conditions":` +
		ssConds([3]string{"tag", "is", "ai"}) + `}}`
	srv := searchesRunLiveResultsServer(t, definition, http.StatusNotFound, &resultRequests)
	seedSearchesRunStore(t, ssConds([3]string{"tag", "is", "ai"}),
		ssItem("AAAA1111", "journalArticle", map[string]any{"title": "Tagged", "tags": ssTags("ai")}),
		ssItem("BBBB2222", "journalArticle", map[string]any{"title": "Untagged"}),
	)

	out, _, err := runSearchesRunTestCmdWithFlags(t, srv, &rootFlags{asJSON: true}, "SK")
	if err != nil {
		t.Fatalf("searches run: %v\noutput: %s", err, out)
	}
	if resultRequests != 1 {
		t.Fatalf("live results endpoint reads = %d, want exactly 1 attempt before the fallback", resultRequests)
	}
	if got, want := searchesRunOutputKeys(t, out), []string{"AAAA1111"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	if len(srv.itemsRequests) != 0 {
		t.Fatalf("fallback issued a broad /items query: %v", srv.itemsRequests)
	}
}

// The fallback never trades a refusal for a partial answer: an unsupported
// condition keeps the precondition and the detail covers both planes.
func TestSearchesRunAutoFallbackRefusesAnUnsupportedSearch(t *testing.T) {
	var resultRequests int
	definition := `{"key":"SK","version":42,"data":{"name":"my saved search","conditions":` +
		ssConds([3]string{"tag", "is", "ai"}, [3]string{"note", "contains", "x"}) + `}}`
	srv := searchesRunLiveResultsServer(t, definition, http.StatusNotFound, &resultRequests)
	seedSearchesRunStore(t, ssConds([3]string{"tag", "is", "ai"}),
		ssItem("AAAA1111", "journalArticle", map[string]any{"title": "Tagged", "tags": ssTags("ai")}),
	)

	out, _, err := runSearchesRunTestCmdWithFlags(t, srv, &rootFlags{asJSON: true}, "SK")
	if err == nil {
		t.Fatalf("searches run succeeded for an unsupported search; output %q", out)
	}
	assertPreconditionExitCode(t, err, 9)
	env := searchesRunPreconditionEnvelope(t, out)
	for _, want := range []string{"my saved search", "results endpoint could not be executed", "note: condition is not supported"} {
		if !strings.Contains(env.Detail, want) {
			t.Fatalf("detail %q does not mention %q", env.Detail, want)
		}
	}
	if strings.Contains(out, "AAAA1111") {
		t.Fatalf("a partial result leaked into the refusal: %q", out)
	}
}

// --data-source live forbids the mirror: an unreachable desktop is the
// precondition even though the mirror could evaluate the search.
func TestSearchesRunLiveDataSourceNeverFallsBackToLocal(t *testing.T) {
	var resultRequests int
	srv := searchesRunLiveResultsServer(t, `{"key":"SK","version":42,"data":{"name":"live only search"}}`, http.StatusNotFound, &resultRequests)
	seedSearchesRunStore(t, ssConds([3]string{"title", "contains", "learning"}),
		ssItem("AAAA1111", "journalArticle", map[string]any{"title": "Deep Learning"}),
	)

	out, _, err := runSearchesRunTestCmdWithFlags(t, srv, &rootFlags{asJSON: true, dataSource: "live"}, "SK")
	if err == nil {
		t.Fatalf("searches run --data-source live succeeded; output %q", out)
	}
	assertPreconditionExitCode(t, err, 9)
	if strings.Contains(out, "AAAA1111") {
		t.Fatalf("the mirror answered a --data-source live run: %q", out)
	}

	// An unreachable desktop (definition read fails) is the same refusal.
	srv.server.Close()
	out, _, err = runSearchesRunTestCmdWithFlags(t, srv, &rootFlags{asJSON: true, dataSource: "live"}, "SK")
	if err == nil {
		t.Fatalf("searches run --data-source live succeeded against a closed server; output %q", out)
	}
	assertPreconditionExitCode(t, err, 9)
	searchesRunPreconditionEnvelope(t, out)
}
