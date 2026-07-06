// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean write-safety): coverage for saved-search materialization mutations.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
