// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package client

import (
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
	if err := c.invalidateCache(); err == nil {
		t.Fatal("invalidateCache() = nil, want RemoveAll error")
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
