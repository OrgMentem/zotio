// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type collectionMoveTestServer struct {
	server       *httptest.Server
	getCount     int
	putCount     int
	putHeader    string
	putBody      map[string]any
	requestPaths []string
}

func newCollectionMoveTestServer(t *testing.T) *collectionMoveTestServer {
	t.Helper()
	ts := &collectionMoveTestServer{}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.requestPaths = append(ts.requestPaths, r.Method+" "+r.URL.Path)
		if r.URL.Path != "/users/0/collections/COLL" {
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			ts.getCount++
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Last-Modified-Version", "77")
			_, _ = w.Write([]byte(`{"key":"COLL","version":77,"data":{"key":"COLL","name":"Child","parentCollection":false}}`))
		case http.MethodPut:
			ts.putCount++
			ts.putHeader = r.Header.Get("If-Unmodified-Since-Version")
			if err := json.NewDecoder(r.Body).Decode(&ts.putBody); err != nil {
				t.Errorf("decode put body: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func runCollectionsMoveTestCmd(t *testing.T, srv *collectionMoveTestServer, flags *rootFlags, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	t.Setenv("ZOTERO_API_KEY", "testkey")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newCollectionsMoveCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestCollectionsMovePreviewWithoutYesWritesNothing(t *testing.T) {
	srv := newCollectionMoveTestServer(t)

	out, stderr, err := runCollectionsMoveTestCmd(t, srv, &rootFlags{}, "--to", "PARENT", "COLL")
	if err != nil {
		t.Fatalf("collections move preview: %v; stderr=%s", err, stderr)
	}
	if got, want := out, "Would move collection COLL under parent PARENT\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if srv.getCount != 0 || srv.putCount != 0 || len(srv.requestPaths) != 0 {
		t.Fatalf("requests = %v (GET=%d PUT=%d), want no HTTP calls", srv.requestPaths, srv.getCount, srv.putCount)
	}
}

func TestCollectionsMoveApplyGetsThenPutsWithVersionPrecondition(t *testing.T) {
	srv := newCollectionMoveTestServer(t)

	_, stderr, err := runCollectionsMoveTestCmd(t, srv, &rootFlags{yes: true}, "--to", "PARENT", "COLL")
	if err != nil {
		t.Fatalf("collections move apply: %v; stderr=%s", err, stderr)
	}
	if srv.getCount != 1 || srv.putCount != 1 {
		t.Fatalf("GET=%d PUT=%d requests=%v, want one GET then one PUT", srv.getCount, srv.putCount, srv.requestPaths)
	}
	if got, want := strings.Join(srv.requestPaths, ","), "GET /users/0/collections/COLL,PUT /users/0/collections/COLL"; got != want {
		t.Fatalf("request order = %q, want %q", got, want)
	}
	if srv.putHeader != "77" {
		t.Fatalf("If-Unmodified-Since-Version = %q, want 77", srv.putHeader)
	}
	if srv.putBody["parentCollection"] != "PARENT" {
		t.Fatalf("PUT body = %+v, want parentCollection PARENT", srv.putBody)
	}
}

func TestCollectionsMoveDryRunWritesNothing(t *testing.T) {
	srv := newCollectionMoveTestServer(t)

	out, stderr, err := runCollectionsMoveTestCmd(t, srv, &rootFlags{dryRun: true}, "--to", "PARENT", "COLL")
	if err != nil {
		t.Fatalf("collections move dry-run: %v; stderr=%s", err, stderr)
	}
	if got, want := out, "Would move collection COLL under parent PARENT\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if srv.getCount != 0 || srv.putCount != 0 || len(srv.requestPaths) != 0 {
		t.Fatalf("requests = %v (GET=%d PUT=%d), want no HTTP calls", srv.requestPaths, srv.getCount, srv.putCount)
	}
}
