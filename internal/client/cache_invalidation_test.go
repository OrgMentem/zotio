// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func unremovableCacheDir(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	cacheDir := filepath.Join(parent, "cache")
	if err := os.Mkdir(cacheDir, 0o700); err != nil {
		t.Fatalf("create cache directory: %v", err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("make cache parent unremovable: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0o700); err != nil {
			t.Errorf("restore cache parent permissions: %v", err)
		}
	})
	return cacheDir
}

func TestInvalidateCacheReturnsRemoveAllError(t *testing.T) {
	c := &Client{cacheDir: unremovableCacheDir(t)}
	// An unclassifiable path takes the full-clear fallback. Both halves of it —
	// publishing the generation marker beside the cache directory and removing
	// the directory itself — need write access to the unwritable parent.
	if err := c.invalidateCache("/connector/saveItems"); err == nil {
		t.Fatal(`invalidateCache("/connector/saveItems") = nil, want RemoveAll error`)
	}
}

func TestSuccessfulMutationNotMaskedByCacheInvalidationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	c.cacheDir = unremovableCacheDir(t)
	// A successful create whose post-mutation cache invalidation fails must NOT
	// be reported as an error: callers check err before status and a masked
	// success could be retried into a duplicate create. The stale-cache risk is
	// surfaced via a one-time stderr warning instead.
	body, status, err := c.Post("/items", map[string]string{"title": "new"})
	if err != nil {
		t.Fatalf("Post error = %v, want nil (successful mutation must not be masked by cache invalidation failure)", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("Post status = %d, want %d", status, http.StatusCreated)
	}
	if string(body) != `{"created":true}` {
		t.Fatalf("Post body = %s, want success response", body)
	}
}

// warmCacheEntry populates one cache entry the way a GET would, and returns the
// file it published so a test can assert survival or removal directly.
func warmCacheEntry(t *testing.T, c *Client, path string, body string) string {
	t.Helper()
	namespace, classified := cacheNamespace(path)
	if !classified {
		t.Fatalf("cacheNamespace(%q) reported unclassified; this helper warms classified reads only", path)
	}
	token, err := c.cacheGenerationSnapshot(namespace)
	if err != nil {
		t.Fatalf("cacheGenerationSnapshot(%q): %v", namespace, err)
	}
	if err := c.writeCacheAtGeneration(token, path, nil, nil, json.RawMessage(body)); err != nil {
		t.Fatalf("writeCacheAtGeneration(%q): %v", path, err)
	}
	file := c.cacheFilePath(namespace, path, nil, nil)
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("warming %q did not publish a cache entry: %v", path, err)
	}
	return file
}

func cacheEntryReadable(t *testing.T, c *Client, path string) bool {
	t.Helper()
	namespace, _ := cacheNamespace(path)
	token, err := c.cacheGenerationSnapshot(namespace)
	if err != nil {
		t.Fatalf("cacheGenerationSnapshot(%q): %v", namespace, err)
	}
	_, ok := c.readCache(token, path, nil, nil)
	return ok
}

func mutationTestServer(t *testing.T) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, "unexpected read", http.StatusMethodNotAllowed)
			return
		}
		_, _ = w.Write([]byte(`{"mutated":true}`))
	}))
	t.Cleanup(server.Close)
	c := clientTestNewClient(t, server.URL)
	c.cacheDir = t.TempDir()
	return c
}

// A collection write changes collection metadata and the owning items' own
// "collections" arrays, but it cannot change the library tag vocabulary or the
// global schema endpoints, so those entries must survive it. This is the
// property the per-resource-type namespaces exist for: before them, every
// mutation removed the whole cache directory.
func TestCollectionWriteClearsCollectionsAndItemsButKeepsUnrelatedTypes(t *testing.T) {
	c := mutationTestServer(t)
	collections := warmCacheEntry(t, c, "/collections", `[{"key":"COLL"}]`)
	items := warmCacheEntry(t, c, "/items", `[{"key":"ITEM"}]`)
	tags := warmCacheEntry(t, c, "/tags", `[{"tag":"survivor"}]`)
	itemTypes := warmCacheEntry(t, c, "/itemTypes", `[{"itemType":"book"}]`)

	if _, _, err := c.Patch("/collections/COLL", map[string]any{"name": "renamed"}); err != nil {
		t.Fatalf("collection PATCH: %v", err)
	}

	for _, cleared := range []struct {
		name string
		file string
		path string
	}{
		{name: "collections", file: collections, path: "/collections"},
		{name: "items", file: items, path: "/items"},
	} {
		if _, err := os.Stat(cleared.file); !os.IsNotExist(err) {
			t.Fatalf("%s cache entry survived a collection write: %v", cleared.name, err)
		}
		if cacheEntryReadable(t, c, cleared.path) {
			t.Fatalf("%s read still served from cache after a collection write", cleared.name)
		}
	}
	for _, kept := range []struct {
		name string
		file string
		path string
	}{
		{name: "tags", file: tags, path: "/tags"},
		{name: "itemTypes", file: itemTypes, path: "/itemTypes"},
	} {
		if _, err := os.Stat(kept.file); err != nil {
			t.Fatalf("%s cache entry was removed by an unrelated collection write: %v", kept.name, err)
		}
		if !cacheEntryReadable(t, c, kept.path) {
			t.Fatalf("%s entry survived on disk but its generation was stranded by an unrelated collection write", kept.name)
		}
	}
}

// zotio writes tags by PATCHing the owning item (internal/cli/items_tags_write.go,
// tags_rename.go), and collection membership is an item field, so an item write
// must clear the tag vocabulary and the collection item lists too. Only the
// global schema endpoints, which no library write can change, survive it.
func TestItemWriteClearsEveryItemDerivedNamespace(t *testing.T) {
	c := mutationTestServer(t)
	warmed := map[string]string{}
	for _, path := range []string{"/items", "/collections", "/tags", "/searches", "/fulltext", "/deleted"} {
		warmed[path] = warmCacheEntry(t, c, path, `[{"key":"WARM"}]`)
	}
	itemTypes := warmCacheEntry(t, c, "/itemTypes", `[{"itemType":"book"}]`)

	if _, _, err := c.Patch("/items/ITEM", map[string]any{"tags": []any{}}); err != nil {
		t.Fatalf("item PATCH: %v", err)
	}

	for path, file := range warmed {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("%s cache entry survived an item write: %v", path, err)
		}
		if cacheEntryReadable(t, c, path) {
			t.Fatalf("%s read still served from cache after an item write", path)
		}
	}
	if _, err := os.Stat(itemTypes); err != nil {
		t.Fatalf("global schema cache entry was removed by an item write: %v", err)
	}
	if !cacheEntryReadable(t, c, "/itemTypes") {
		t.Fatal("global schema entry survived on disk but its generation was stranded by an item write")
	}
}

// An unclassifiable mutation path has no known dependency set, so it must fall
// back to clearing everything. Under-invalidating it would serve pre-write data
// silently, which is worse than dropping a regenerable cache.
func TestUnclassifiedMutationClearsEveryNamespace(t *testing.T) {
	c := mutationTestServer(t)
	warmed := map[string]string{}
	for _, path := range []string{"/items", "/tags", "/itemTypes"} {
		warmed[path] = warmCacheEntry(t, c, path, `[{"key":"WARM"}]`)
	}

	if _, _, err := c.Post("/connector/saveItems", map[string]any{"items": []any{}}); err != nil {
		t.Fatalf("unclassified POST: %v", err)
	}

	for path, file := range warmed {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("%s cache entry survived an unclassified write: %v", path, err)
		}
		if cacheEntryReadable(t, c, path) {
			t.Fatalf("%s read still served from cache after an unclassified write", path)
		}
	}
}

// An unclassifiable read cannot be invalidated selectively, so it must never be
// cached: a later classified mutation would leave it behind and a subsequent
// read would serve pre-write bytes.
func TestUnclassifiedReadIsNotCached(t *testing.T) {
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		_, _ = w.Write([]byte(`{"ping":true}`))
	}))
	defer server.Close()

	c := clientTestNewClient(t, server.URL)
	c.cacheDir = t.TempDir()
	for range 2 {
		if _, err := c.Get("/connector/ping", nil); err != nil {
			t.Fatalf("unclassified GET: %v", err)
		}
	}
	if reads != 2 {
		t.Fatalf("server saw %d reads, want 2: an unclassifiable path must not be cached", reads)
	}
	entries, err := os.ReadDir(c.cacheDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading cache directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("cache directory holds %d entries, want none for an unclassifiable path", len(entries))
	}
}

func TestCacheNamespaceClassification(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "/items", want: "items"},
		{path: "/items/ABCD/children", want: "items"},
		{path: "/items?limit=1", want: "items"},
		{path: "/collections/ABCD/items", want: "collections"},
		{path: "/itemTypeCreatorTypes", want: "itemTypeCreatorTypes"},
		{path: "/connector/ping", want: ""},
		{path: "/", want: ""},
		{path: "", want: ""},
		{path: "/../../escape", want: ""},
		{path: "/Items", want: ""},
	} {
		got, classified := cacheNamespace(tc.path)
		if got != tc.want || classified != (tc.want != "") {
			t.Errorf("cacheNamespace(%q) = (%q, %v), want (%q, %v)", tc.path, got, classified, tc.want, tc.want != "")
		}
	}
	// Every namespace must clear itself, or a write to it would leave its own
	// stale reads in place.
	for namespace, targets := range cacheInvalidationTargets {
		self := false
		for _, target := range targets {
			if target == namespace {
				self = true
			}
			if _, known := cacheInvalidationTargets[target]; !known {
				t.Errorf("namespace %q depends on unknown namespace %q", namespace, target)
			}
		}
		if !self {
			t.Errorf("namespace %q does not clear itself", namespace)
		}
	}
}
