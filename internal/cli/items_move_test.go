// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/mutation"
)

type itemMoveTestServer struct {
	server       *httptest.Server
	versions     map[string]string
	collections  map[string][]string
	patchBodies  map[string]map[string]any
	patchHeaders map[string]string
	patchCounts  map[string]int
}

func newItemMoveTestServer(t *testing.T, versions map[string]string, collections map[string][]string) *itemMoveTestServer {
	t.Helper()
	ts := &itemMoveTestServer{
		versions:     versions,
		collections:  collections,
		patchBodies:  map[string]map[string]any{},
		patchHeaders: map[string]string{},
		patchCounts:  map[string]int{},
	}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for key := range ts.collections {
			itemPath := "/users/0/items/" + key
			if r.URL.Path != itemPath {
				continue
			}
			switch r.Method {
			case http.MethodGet:
				version := ts.versions[key]
				w.Header().Set("Last-Modified-Version", version)
				_, _ = fmt.Fprintf(w, `{"key":%q,"version":%s,"data":{"collections":%s}}`, key, version, mustJSON(t, ts.collections[key]))
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

func runItemsMoveTestCmd(t *testing.T, srv *itemMoveTestServer, flags *rootFlags, args ...string) (mutation.Envelope, string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newItemsMoveCmd(flags)
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

func mustRunItemsMoveTestCmd(t *testing.T, srv *itemMoveTestServer, flags *rootFlags, args ...string) mutation.Envelope {
	t.Helper()
	env, stderr, err := runItemsMoveTestCmd(t, srv, flags, args...)
	if err != nil {
		t.Fatalf("items move %v: %v; stderr=%s", args, err, stderr)
	}
	return env
}

func TestItemsMoveToAddsCollection(t *testing.T) {
	srv := newItemMoveTestServer(t, map[string]string{"K1": "42"}, map[string][]string{
		"K1": {"SOURCE"},
	})

	env := mustRunItemsMoveTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--to", "TARGET", "K1")
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want one applied item", env)
	}
	if srv.patchCounts["K1"] != 1 {
		t.Fatalf("PATCH count = %d, want 1", srv.patchCounts["K1"])
	}
	if srv.patchHeaders["K1"] != "42" {
		t.Errorf("If-Unmodified-Since-Version = %q, want 42", srv.patchHeaders["K1"])
	}
	collections := patchBodyCollections(t, srv.patchBodies["K1"])
	if !stringSliceContains(collections, "SOURCE") || !stringSliceContains(collections, "TARGET") {
		t.Errorf("PATCH collections = %v, want SOURCE and TARGET", collections)
	}
}

func TestItemsMoveAlreadyInTargetIsNoOp(t *testing.T) {
	srv := newItemMoveTestServer(t, map[string]string{"K1": "42"}, map[string][]string{
		"K1": {"TARGET"},
	})

	env := mustRunItemsMoveTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--to", "TARGET", "K1")
	if !env.OK || env.Result == nil || env.Result.Summary.NoOp != 1 || env.Result.Items[0].Status != "no_op" {
		t.Fatalf("env = %+v, want no_op", env)
	}
	if srv.patchCounts["K1"] != 0 {
		t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["K1"])
	}
}

func TestItemsMoveFromRemovesCollection(t *testing.T) {
	srv := newItemMoveTestServer(t, map[string]string{"K1": "42"}, map[string][]string{
		"K1": {"SOURCE", "KEEP"},
	})

	env := mustRunItemsMoveTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--from", "SOURCE", "K1")
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want applied remove", env)
	}
	collections := patchBodyCollections(t, srv.patchBodies["K1"])
	if stringSliceContains(collections, "SOURCE") || !stringSliceContains(collections, "KEEP") {
		t.Errorf("PATCH collections = %v, want SOURCE removed and KEEP preserved", collections)
	}
}

func TestItemsMoveFromToMovesCollection(t *testing.T) {
	srv := newItemMoveTestServer(t, map[string]string{"K1": "42"}, map[string][]string{
		"K1": {"SOURCE", "KEEP"},
	})

	env := mustRunItemsMoveTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--from", "SOURCE", "--to", "TARGET", "K1")
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want applied move", env)
	}
	if len(env.Plan.Operations) != 1 || len(env.Plan.Operations[0].Changes) != 2 {
		t.Fatalf("changes = %+v, want remove and add", env.Plan.Operations)
	}
	collections := patchBodyCollections(t, srv.patchBodies["K1"])
	if stringSliceContains(collections, "SOURCE") || !stringSliceContains(collections, "TARGET") || !stringSliceContains(collections, "KEEP") {
		t.Errorf("PATCH collections = %v, want SOURCE removed, TARGET added, KEEP preserved", collections)
	}
}

func TestItemsMoveRequiresToOrFrom(t *testing.T) {
	srv := newItemMoveTestServer(t, map[string]string{"K1": "42"}, map[string][]string{
		"K1": {"SOURCE"},
	})

	_, _, err := runItemsMoveTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "K1")
	if err == nil || !strings.Contains(err.Error(), "--to or --from") {
		t.Fatalf("err = %v, want --to/--from usage error", err)
	}
	if srv.patchCounts["K1"] != 0 {
		t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["K1"])
	}
}

func TestItemsMoveBulkKeysFrom(t *testing.T) {
	srv := newItemMoveTestServer(t, map[string]string{"K1": "42", "K2": "43"}, map[string][]string{
		"K1": {"ONE"},
		"K2": {"TWO"},
	})
	keysPath := filepath.Join(t.TempDir(), "keys.txt")
	if err := os.WriteFile(keysPath, []byte("K1\nK2\n"), 0o600); err != nil {
		t.Fatalf("write keys file: %v", err)
	}

	env := mustRunItemsMoveTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--to", "BULK", "--keys-from", keysPath)
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 2 || len(env.Result.Items) != 2 {
		t.Fatalf("env = %+v, want two applied items", env)
	}
	for _, key := range []string{"K1", "K2"} {
		if srv.patchCounts[key] != 1 {
			t.Fatalf("%s PATCH count = %d, want 1", key, srv.patchCounts[key])
		}
		if !stringSliceContains(patchBodyCollections(t, srv.patchBodies[key]), "BULK") {
			t.Errorf("%s PATCH body = %+v, want BULK collection", key, srv.patchBodies[key])
		}
	}
}

func TestItemsMovePreviewWritesNothing(t *testing.T) {
	srv := newItemMoveTestServer(t, map[string]string{"K1": "42"}, map[string][]string{
		"K1": {"SOURCE"},
	})

	env := mustRunItemsMoveTestCmd(t, srv, &rootFlags{asJSON: true, maxChanges: -1}, "--to", "TARGET", "K1")
	if !env.OK || env.Mode != "preview" || env.Result != nil || env.Plan.Summary.Planned != 1 {
		t.Fatalf("env = %+v, want preview plan with one change", env)
	}
	if srv.patchCounts["K1"] != 0 {
		t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["K1"])
	}
}

func patchBodyCollections(t *testing.T, body map[string]any) []string {
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
