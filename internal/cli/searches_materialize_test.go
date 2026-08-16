// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

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

	"zotio/internal/mutation"
)

type searchesMaterializeTestServer struct {
	server       *httptest.Server
	searchKeys   []string
	versions     map[string]string
	collections  map[string][]string
	patchBodies  map[string]map[string]any
	patchHeaders map[string]string
	patchCounts  map[string]int
}

func newSearchesMaterializeTestServer(t *testing.T, searchKeys []string, versions map[string]string, collections map[string][]string) *searchesMaterializeTestServer {
	t.Helper()
	ts := &searchesMaterializeTestServer{
		searchKeys:   searchKeys,
		versions:     versions,
		collections:  collections,
		patchBodies:  map[string]map[string]any{},
		patchHeaders: map[string]string{},
		patchCounts:  map[string]int{},
	}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/0/searches/SK/items" && r.Method == http.MethodGet {
			w.Header().Set("Last-Modified-Version", "99")
			items := make([]map[string]any, 0, len(ts.searchKeys))
			for _, key := range ts.searchKeys {
				items = append(items, map[string]any{
					"key":     key,
					"version": ts.versions[key],
					"data": map[string]any{
						"collections": ts.collections[key],
					},
				})
			}
			_, _ = fmt.Fprint(w, searchMaterializeJSON(t, items))
			return
		}
		for key := range ts.collections {
			itemPath := "/users/0/items/" + key
			if r.URL.Path != itemPath {
				continue
			}
			switch r.Method {
			case http.MethodGet:
				version := ts.versions[key]
				w.Header().Set("Last-Modified-Version", version)
				_, _ = fmt.Fprintf(w, `{"key":%q,"version":%s,"data":{"collections":%s}}`, key, version, searchMaterializeJSON(t, ts.collections[key]))
			case http.MethodPatch:
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode patch body: %v", err)
				}
				ts.patchBodies[key] = body
				ts.patchHeaders[key] = r.Header.Get("If-Unmodified-Since-Version")
				ts.patchCounts[key]++
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			}
			return
		}
		http.Error(w, "unexpected path", http.StatusNotFound)
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func searchMaterializeJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(data)
}

func searchMaterializePatchBodyCollections(t *testing.T, body map[string]any) []string {
	t.Helper()
	rawCollections, ok := body["collections"].([]any)
	if !ok {
		t.Fatalf("PATCH body = %+v, missing collections", body)
	}
	collections := make([]string, 0, len(rawCollections))
	for _, raw := range rawCollections {
		collection, ok := raw.(string)
		if !ok {
			t.Fatalf("PATCH collection = %#v, want string", raw)
		}
		collections = append(collections, collection)
	}
	return collections
}

func runSearchesMaterializeTestCmd(t *testing.T, srv *searchesMaterializeTestServer, flags *rootFlags, args ...string) (mutation.Envelope, string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newSearchesMaterializeCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	var env mutation.Envelope
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode mutation envelope %q: %v", out.String(), decodeErr)
		}
	}
	return env, errOut.String(), err
}

func mustRunSearchesMaterializeTestCmd(t *testing.T, srv *searchesMaterializeTestServer, flags *rootFlags, args ...string) mutation.Envelope {
	t.Helper()
	env, stderr, err := runSearchesMaterializeTestCmd(t, srv, flags, args...)
	if err != nil {
		t.Fatalf("searches materialize %v: %v; stderr=%s", args, err, stderr)
	}
	return env
}

func TestSearchesMaterializePreviewListsAddsAndWritesNothing(t *testing.T) {
	srv := newSearchesMaterializeTestServer(t, []string{"K1", "K2"}, map[string]string{"K1": "42", "K2": "43"}, map[string][]string{
		"K1": {"SOURCE"},
		"K2": {"OTHER"},
	})

	env := mustRunSearchesMaterializeTestCmd(t, srv, &rootFlags{asJSON: true, maxChanges: -1}, "SK", "--to", "TARGET")
	if !env.OK || env.Mode != "preview" || env.Result != nil || env.Plan.Summary.Planned != 2 || len(env.Plan.Operations) != 2 {
		t.Fatalf("env = %+v, want preview plan with two adds", env)
	}
	for _, op := range env.Plan.Operations {
		if op.Kind != "collection_add" || len(op.Changes) != 1 || op.Changes[0].Field != "collections" || op.Changes[0].Add != "TARGET" {
			t.Fatalf("op = %+v, want collection_add TARGET change", op)
		}
	}
	if srv.patchCounts["K1"] != 0 || srv.patchCounts["K2"] != 0 {
		t.Fatalf("PATCH counts = K1:%d K2:%d, want 0", srv.patchCounts["K1"], srv.patchCounts["K2"])
	}
}

func TestSearchesMaterializeYesAddsAllItems(t *testing.T) {
	srv := newSearchesMaterializeTestServer(t, []string{"K1", "K2"}, map[string]string{"K1": "42", "K2": "43"}, map[string][]string{
		"K1": {"SOURCE"},
		"K2": {"OTHER"},
	})

	env := mustRunSearchesMaterializeTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "SK", "--to", "TARGET")
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.Applied != 2 {
		t.Fatalf("env = %+v, want two applied items", env)
	}
	for _, key := range []string{"K1", "K2"} {
		if srv.patchCounts[key] != 1 {
			t.Fatalf("PATCH count %s = %d, want 1", key, srv.patchCounts[key])
		}
		if srv.patchHeaders[key] != srv.versions[key] {
			t.Errorf("If-Unmodified-Since-Version %s = %q, want %q", key, srv.patchHeaders[key], srv.versions[key])
		}
		collections := searchMaterializePatchBodyCollections(t, srv.patchBodies[key])
		if !stringSliceContains(collections, "TARGET") {
			t.Errorf("PATCH collections %s = %v, want TARGET", key, collections)
		}
	}
}

func TestSearchesMaterializeAlreadyInCollectionIsNoOp(t *testing.T) {
	srv := newSearchesMaterializeTestServer(t, []string{"K1"}, map[string]string{"K1": "42"}, map[string][]string{
		"K1": {"TARGET"},
	})

	env := mustRunSearchesMaterializeTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "SK", "--to", "TARGET")
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.NoOp != 1 || env.Result.Items[0].Status != "no_op" {
		t.Fatalf("env = %+v, want one no_op item", env)
	}
	if srv.patchCounts["K1"] != 0 {
		t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["K1"])
	}
}

func TestSearchesMaterializeEmptySearchYieldsEmptyPlan(t *testing.T) {
	srv := newSearchesMaterializeTestServer(t, nil, map[string]string{}, map[string][]string{})

	env := mustRunSearchesMaterializeTestCmd(t, srv, &rootFlags{asJSON: true, maxChanges: -1}, "SK", "--to", "TARGET")
	if !env.OK || env.Mode != "preview" || env.Plan.Summary.Selected != 0 || len(env.Plan.Operations) != 0 {
		t.Fatalf("env = %+v, want empty preview plan", env)
	}
	journal, ok := env.Journal.(map[string]any)
	if !ok || journal["message"] == "" {
		t.Fatalf("journal = %+v, want empty-plan message", env.Journal)
	}
}

func TestSearchesMaterializeRequiresTo(t *testing.T) {
	srv := newSearchesMaterializeTestServer(t, []string{"K1"}, map[string]string{"K1": "42"}, map[string][]string{
		"K1": {"SOURCE"},
	})

	_, _, err := runSearchesMaterializeTestCmd(t, srv, &rootFlags{asJSON: true, maxChanges: -1}, "SK")
	if err == nil {
		t.Fatalf("searches materialize without --to succeeded")
	}
}

func TestSearchesMaterializePaginatesAcrossPages(t *testing.T) {
	// Build exactly zoteroPageMax + 2 keys so page 0 is full and page 1
	// exercises the next start offset. The handler must see limit=100 on
	// every page and start advancing by 100.
	total := zoteroPageMax + 2
	keys := make([]string, total)
	versions := make(map[string]string, total)
	collections := make(map[string][]string, total)
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("P%03d", i)
		keys[i] = key
		versions[key] = strconv.Itoa(100 + i)
		collections[key] = []string{"SRC"}
	}
	var requestStarts []string
	var requestLimits []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/users/0/searches/") && strings.HasSuffix(r.URL.Path, "/items") && r.Method == http.MethodGet {
			q := r.URL.Query()
			requestStarts = append(requestStarts, q.Get("start"))
			requestLimits = append(requestLimits, q.Get("limit"))
			start, _ := strconv.Atoi(q.Get("start"))
			limit, _ := strconv.Atoi(q.Get("limit"))
			if limit == 0 {
				limit = zoteroPageMax
			}
			end := start + limit
			if end > len(keys) {
				end = len(keys)
			}
			if start >= len(keys) {
				_, _ = fmt.Fprint(w, "[]")
				return
			}
			items := make([]map[string]any, 0, end-start)
			for _, key := range keys[start:end] {
				items = append(items, map[string]any{
					"key":     key,
					"version": versions[key],
					"data":    map[string]any{"collections": collections[key]},
				})
			}
			_, _ = fmt.Fprint(w, searchMaterializeJSON(t, items))
			return
		}
		// Item reads/PATCHes for the mutation apply phase.
		for key := range collections {
			if r.URL.Path != "/users/0/items/"+key {
				continue
			}
			switch r.Method {
			case http.MethodGet:
				version := versions[key]
				w.Header().Set("Last-Modified-Version", version)
				_, _ = fmt.Fprintf(w, `{"key":%q,"version":%s,"data":{"collections":%s}}`, key, version, searchMaterializeJSON(t, collections[key]))
			case http.MethodPatch:
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
	}))
	defer ts.Close()
	t.Setenv("ZOTERO_BASE_URL", ts.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	flags := &rootFlags{asJSON: true, maxChanges: -1}
	cmd := newSearchesMaterializeCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"SK", "--to", "TARGET"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("materialize paginated: %v", err)
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", out.String(), err)
	}
	if len(env.Plan.Operations) != total {
		t.Fatalf("operations = %d, want %d (both pages)", len(env.Plan.Operations), total)
	}
	seen := make(map[string]bool, total)
	for _, op := range env.Plan.Operations {
		seen[op.Key] = true
	}
	for _, key := range keys {
		if !seen[key] {
			t.Errorf("missing operation for key %s", key)
		}
	}
	// Request sequence must show advancing start: 0 then 100.
	if len(requestStarts) < 2 {
		t.Fatalf("search requests = %v, want at least two pages", requestStarts)
	}
	if requestStarts[0] != "0" || requestStarts[1] != strconv.Itoa(zoteroPageMax) {
		t.Fatalf("request starts = %v, want [0 %d ...]", requestStarts, zoteroPageMax)
	}
	for _, lim := range requestLimits {
		if lim != strconv.Itoa(zoteroPageMax) {
			t.Fatalf("request limit = %q, want %d", lim, zoteroPageMax)
		}
	}
}

func TestSearchesMaterializeDuplicateKeysArePaginationFailure(t *testing.T) {
	// Server ignores start and repeats page 0 forever.
	repeatKeys := make([]string, zoteroPageMax)
	for i := 0; i < zoteroPageMax; i++ {
		repeatKeys[i] = fmt.Sprintf("D%03d", i)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/users/0/searches/") && r.Method == http.MethodGet {
			items := make([]map[string]any, 0, len(repeatKeys))
			for _, key := range repeatKeys {
				items = append(items, map[string]any{"key": key, "version": "1", "data": map[string]any{"collections": []string{}}})
			}
			_, _ = fmt.Fprint(w, searchMaterializeJSON(t, items))
			return
		}
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}))
	defer ts.Close()
	t.Setenv("ZOTERO_BASE_URL", ts.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	flags := &rootFlags{asJSON: true, maxChanges: -1}
	cmd := newSearchesMaterializeCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	var errOut bytes.Buffer
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"SK", "--to", "TARGET"})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("materialize with repeating server succeeded; want pagination failure")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "pagination") && !strings.Contains(msg, "duplicate") && !strings.Contains(msg, "ignored start") {
		t.Fatalf("error = %q, want pagination/duplicate failure", err.Error())
	}
}

func TestSearchesMaterializeSamePageDuplicateProducesOneOperation(t *testing.T) {
	// One response page contains the same key twice. The materializer must
	// deduplicate within the page and emit exactly one mutation operation
	// for that key, not two. Cross-page duplicates remain a hard pagination
	// error; same-page duplicates are a benign server quirk that is soft-
	// skipped (see fix for 3f28cd9).
	dupKey := "K1"
	otherKey := "K2"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/users/0/searches/") && strings.HasSuffix(r.URL.Path, "/items") && r.Method == http.MethodGet {
			items := []map[string]any{
				{"key": dupKey, "version": "1", "data": map[string]any{"collections": []string{}}},
				{"key": dupKey, "version": "1", "data": map[string]any{"collections": []string{}}},
				{"key": otherKey, "version": "1", "data": map[string]any{"collections": []string{}}},
			}
			_, _ = fmt.Fprint(w, searchMaterializeJSON(t, items))
			return
		}
		if r.URL.Path == "/users/0/items/"+dupKey || r.URL.Path == "/users/0/items/"+otherKey {
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Last-Modified-Version", "1")
				_, _ = fmt.Fprintf(w, `{"key":%q,"version":1,"data":{"collections":[]}}`, strings.TrimPrefix(r.URL.Path, "/users/0/items/"))
			case http.MethodPatch:
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}))
	defer ts.Close()
	t.Setenv("ZOTERO_BASE_URL", ts.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	flags := &rootFlags{asJSON: true, maxChanges: -1}
	cmd := newSearchesMaterializeCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"SK", "--to", "TARGET"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("materialize with same-page duplicate: %v", err)
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", out.String(), err)
	}
	if len(env.Plan.Operations) != 2 {
		t.Fatalf("operations = %d, want 2 (K1 deduped to one + K2)", len(env.Plan.Operations))
	}
	count := 0
	for _, op := range env.Plan.Operations {
		if op.Key == dupKey {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("operations for %s = %d, want 1; ops=%v", dupKey, count, env.Plan.Operations)
	}
}

func TestApplySearchesMaterializeCollectionAdd_FailsClosedOnZeroVersion(t *testing.T) {
	var patchDispatched bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/0/items/K1" {
			switch r.Method {
			case http.MethodGet:
				// No Last-Modified-Version and empty version string -> version 0.
				_, _ = fmt.Fprint(w, `{"key":"K1","version":"","data":{"collections":[]}}`)
			case http.MethodPatch:
				patchDispatched = true
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "method", http.StatusMethodNotAllowed)
			}
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()
	t.Setenv("ZOTERO_BASE_URL", ts.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	flags := &rootFlags{}
	c, err := flags.newWriteClient()
	if err != nil {
		t.Fatalf("newWriteClient: %v", err)
	}
	status, reason, retErr := applySearchesMaterializeCollectionAdd(c, "/items/K1", "TARGET")
	if retErr == nil || status != "failed" {
		t.Fatalf("status=%q err=%v reason=%v, want failed when version is 0", status, retErr, reason)
	}
	if patchDispatched {
		t.Fatalf("PATCH was dispatched with version 0; want no request")
	}
	msg := strings.ToLower(fmt.Sprint(reason))
	if !strings.Contains(msg, "precondition") && !strings.Contains(msg, "version") {
		t.Fatalf("reason=%v, want missing-precondition message", reason)
	}
}
