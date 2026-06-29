// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean pathenc-2): cover single-segment URL encoding for CLI path builders.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type pathencCaptureGetter struct {
	paths []string
}

func (g *pathencCaptureGetter) Get(path string, params map[string]string) (json.RawMessage, error) {
	g.paths = append(g.paths, path)
	if path == "/collections/..%2FROOT%2FA/collections" {
		return json.RawMessage(`[{"key":"../CHILD/X"}]`), nil
	}
	return json.RawMessage(`[]`), nil
}

func TestCollectionsExportEscapesCollectionPathSegments(t *testing.T) {
	getter := &pathencCaptureGetter{}
	var out bytes.Buffer

	if err := exportCollection(getter, &out, "../ROOT/A", "bibtex", false, 50, map[string]bool{}); err != nil {
		t.Fatalf("exportCollection returned error: %v", err)
	}

	want := []string{
		"/collections/..%2FROOT%2FA/items",
		"/collections/..%2FROOT%2FA/collections",
		"/collections/..%2FCHILD%2FX/items",
		"/collections/..%2FCHILD%2FX/collections",
	}
	if !reflect.DeepEqual(getter.paths, want) {
		t.Fatalf("paths = %#v, want %#v", getter.paths, want)
	}
}

func TestAnnotationsExportEscapesCollectionPathSegment(t *testing.T) {
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.RequestURI)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	cmd := newAnnotationsExportCmd(&rootFlags{asJSON: true, noCache: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--collection", "../COLL/A", "--format", "json", "--refresh", "--limit", "1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("annotations export failed: %v; stderr: %s", err, errOut.String())
	}
	if len(requests) == 0 {
		t.Fatal("annotations export made no requests")
	}
	if !strings.HasPrefix(requests[0], "/users/0/collections/..%2FCOLL%2FA/items?") {
		t.Fatalf("request path = %q, want escaped collection segment", requests[0])
	}
}
