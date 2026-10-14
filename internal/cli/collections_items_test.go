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

	"github.com/spf13/cobra"

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
	if err := db.SaveSyncState("collections", "", len(rows)); err != nil {
		_ = db.Close()
		t.Fatalf("save collections sync state: %v", err)
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
	const itemPayload = `{"key":"ITEMAB23","version":1,"data":{"key":"ITEMAB23","itemType":"book","title":"In collection","collections":["CLCTAB23"]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/collections/CLCTAB23":
			// The existence check: same row the mirror already holds.
			_, _ = w.Write([]byte(`{"key":"CLCTAB23","version":1,"data":{"key":"CLCTAB23","name":"Mine","parentCollection":false}}`))
		case "/collections/CLCTAB23/items":
			_, _ = w.Write([]byte(`[` + itemPayload + `]`))
		default:
			t.Errorf("path = %q, want the collection or its items", r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL)

	dbPath := collChildCacheSeedCollections(t, []json.RawMessage{
		json.RawMessage(`{"key":"CLCTAB23","version":1,"data":{"key":"CLCTAB23","name":"Mine","parentCollection":false}}`),
	})

	flags := &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: 5 * time.Second}
	cmd := newCollectionsItemsCmd(flags)
	cmd.SetArgs([]string{"CLCTAB23"})
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
	if !strings.Contains(string(env.Results), "ITEMAB23") {
		t.Fatalf("results = %s, want the live ITEMAB23 row", env.Results)
	}

	mirror := collChildCacheOpenMirror(t, dbPath)
	if n, err := mirror.Count("collections"); err != nil || n != 1 {
		t.Fatalf("collections rows = %d, err = %v; want exactly the seeded CLCTAB23", n, err)
	}
	got, err := mirror.Get("items", "ITEMAB23")
	if err != nil {
		t.Fatalf("item ITEMAB23 not written through to the items resource: %v", err)
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
	if !strings.Contains(string(localData), "CLCTAB23") || strings.Contains(string(localData), "ITEMAB23") {
		t.Fatalf("local collections = %s, want only the seeded CLCTAB23", localData)
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
		switch r.URL.Path {
		case "/collections/CLCTAB23":
			_, _ = w.Write([]byte(`{"key":"CLCTAB23","version":1,"data":{"key":"CLCTAB23","name":"Mine","parentCollection":false}}`))
		case "/collections/CLCTAB23/items":
			_, _ = w.Write([]byte(bibtex))
		default:
			t.Errorf("path = %q, want the collection or its items", r.URL.Path)
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL)

	flags := &rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: 5 * time.Second}
	cmd := newCollectionsItemsCmd(flags)
	cmd.SetArgs([]string{"CLCTAB23", "--format", "bibtex"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("collections items --format bibtex: %v", err)
	}

	dbPath := helpersTestDefaultDBPath(t, "zotio")
	mirror := collChildCacheOpenMirror(t, dbPath)
	for _, resource := range []string{"items", "tags"} {
		if n, err := mirror.Count(resource); err != nil || n != 0 {
			t.Fatalf("%s rows = %d, err = %v; want nothing cached from a rendered response", resource, n, err)
		}
	}
	// Only the collection row the existence check read may be cached, never
	// the rendered payload.
	if n, err := mirror.Count("collections"); err != nil || n != 1 {
		t.Fatalf("collections rows = %d, err = %v; want only the verified collection", n, err)
	}
	row, err := mirror.Get("collections", "CLCTAB23")
	if err != nil || strings.Contains(string(row), "@book") {
		t.Fatalf("cached collection = %q, err = %v; want the JSON row, not the rendered payload", row, err)
	}
}

// Zotero's local API answers /collections/<unknown>/items with the whole
// library, so an unknown key must be refused before the items are read: exit 3
// and no output, never another collection's data with exit 0.
func TestCollectionScopedReadsRefuseUnknownCollection(t *testing.T) {
	collChildCacheIsolateMirror(t)
	var itemReads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/collections/MISSAB23":
			http.Error(w, "Collection not found", http.StatusNotFound)
		default:
			itemReads++
			_, _ = w.Write([]byte(`[{"key":"ITEMAB23","version":1,"data":{"key":"ITEMAB23"}}]`))
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL)

	for name, build := range map[string]func(*rootFlags) *cobra.Command{
		"items":          newCollectionsItemsCmd,
		"subcollections": newCollectionsSubcollectionsCmd,
		"tags":           newCollectionsTagsCmd,
	} {
		t.Run(name, func(t *testing.T) {
			cmd := build(&rootFlags{asJSON: true, dataSource: "auto", noCache: true, timeout: 5 * time.Second})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetArgs([]string{"MISSAB23"})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.Execute()
			if err == nil || ExitCode(err) != 3 {
				t.Fatalf("error = %v, exit=%d; want not found (exit 3)", err, ExitCode(err))
			}
			if out.Len() != 0 || itemReads != 0 {
				t.Fatalf("out=%q itemReads=%d; want nothing read or printed for an unknown collection", out.String(), itemReads)
			}
		})
	}

	t.Run("local mirror", func(t *testing.T) {
		collChildCacheSeedCollections(t, []json.RawMessage{
			json.RawMessage(`{"key":"CLCTAB23","version":1,"data":{"key":"CLCTAB23","name":"Mine","parentCollection":false}}`),
		})
		cmd := newCollectionsItemsCmd(&rootFlags{asJSON: true, dataSource: "local", timeout: time.Second})
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		cmd.SetArgs([]string{"MISSAB23"})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err == nil || ExitCode(err) != 3 {
			t.Fatalf("error = %v, exit=%d; want not found (exit 3)", err, ExitCode(err))
		}
	})
}
