// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/store"
)

// collChildCacheIsolateMirror points the mirror and the config lookup at
// throwaway directories and clears the group scope, so write-through and
// fallback assertions below observe only what the test itself stored.
func collChildCacheIsolateMirror(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	prevGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(prevGroup) })
}

// collChildCacheEnvelope decodes the shared {results, meta} read envelope.
type collChildCacheEnvelope struct {
	Results json.RawMessage `json:"results"`
	Meta    DataProvenance  `json:"meta"`
}

func collChildCacheDecodeEnvelope(t *testing.T, out string) collChildCacheEnvelope {
	t.Helper()
	var env collChildCacheEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode read envelope %q: %v", out, err)
	}
	return env
}

func collChildCacheSeedCollections(t *testing.T, rows []json.RawMessage) string {
	t.Helper()
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir store dir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("collections", rows); err != nil {
		_ = db.Close()
		t.Fatalf("seed collections: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	return dbPath
}

func collChildCacheOpenMirror(t *testing.T, dbPath string) *store.Store {
	t.Helper()
	mirror, err := store.OpenReadOnlyContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open mirror: %v", err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	return mirror
}

// TestCollectionsItemsAutoWriteThroughTargetsItems runs a live auto-mode
// `collections items` against a test server and then opens the SQLite mirror:
// the item row must be stored under the items resource, and no item payload
// may be present under the collections resource.
func TestCollectionsItemsAutoWriteThroughTargetsItems(t *testing.T) {
	collChildCacheIsolateMirror(t)
	const itemPayload = `{"key":"IT1","version":1,"data":{"key":"IT1","itemType":"book","title":"In collection","collections":["COL1"]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/collections/COL1/items" {
			t.Errorf("path = %q, want /collections/COL1/items", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[` + itemPayload + `]`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL)

	dbPath := collChildCacheSeedCollections(t, []json.RawMessage{
		json.RawMessage(`{"key":"COL1","version":1,"data":{"key":"COL1","name":"Mine","parentCollection":false}}`),
	})

	flags := &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: 5 * time.Second}
	cmd := newCollectionsItemsCmd(flags)
	cmd.SetArgs([]string{"COL1"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("collections items: %v", err)
	}
	env := collChildCacheDecodeEnvelope(t, out.String())
	if env.Meta.Source != "live" {
		t.Fatalf("provenance source = %q, want live", env.Meta.Source)
	}
	if !strings.Contains(string(env.Results), "IT1") {
		t.Fatalf("results = %s, want the live IT1 row", env.Results)
	}

	mirror := collChildCacheOpenMirror(t, dbPath)
	if n, err := mirror.Count("collections"); err != nil || n != 1 {
		t.Fatalf("collections rows = %d, err = %v; want exactly the seeded COL1", n, err)
	}
	got, err := mirror.Get("items", "IT1")
	if err != nil {
		t.Fatalf("item IT1 not written through to the items resource: %v", err)
	}
	if string(got) != itemPayload {
		t.Fatalf("cached item = %q, want %q", got, itemPayload)
	}

	// The local collections list still returns the seeded collection only:
	// no item row leaks into the collections resource.
	localFlags := &rootFlags{asJSON: true, dataSource: "local", noCache: true, timeout: time.Second}
	localData, prov, err := resolveRead(context.Background(), nil, localFlags, "collections", false, "/collections", nil, nil)
	if err != nil {
		t.Fatalf("local collections list: %v", err)
	}
	if !strings.Contains(string(localData), "COL1") || strings.Contains(string(localData), "IT1") {
		t.Fatalf("local collections = %s, want only the seeded COL1", localData)
	}
	if prov.Source != "local" || prov.ResourceType != "collections" {
		t.Fatalf("local provenance = %+v, want local collections", prov)
	}
}

// TestResolveReadCollectionItemsRenderedFormatSkipsWriteThrough serves a
// rendered (non-JSON) collection-items response in auto mode: with no row
// identity present, nothing may be cached under any resource.
func TestResolveReadCollectionItemsRenderedFormatSkipsWriteThrough(t *testing.T) {
	collChildCacheIsolateMirror(t)
	const bibtex = "@book{it1,\n\ttitle = {In collection}\n}\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/collections/COL1/items" {
			t.Errorf("path = %q, want /collections/COL1/items", r.URL.Path)
		}
		_, _ = w.Write([]byte(bibtex))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL)

	flags := &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: 5 * time.Second}
	cmd := newCollectionsItemsCmd(flags)
	cmd.SetArgs([]string{"COL1", "--format", "bibtex"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("collections items --format bibtex: %v", err)
	}

	dbPath := helpersTestDefaultDBPath(t, "zotio")
	mirror := collChildCacheOpenMirror(t, dbPath)
	for _, resource := range []string{"collections", "items", "tags"} {
		if n, err := mirror.Count(resource); err != nil || n != 0 {
			t.Fatalf("%s rows = %d, err = %v; want nothing cached from a rendered response", resource, n, err)
		}
	}
}
