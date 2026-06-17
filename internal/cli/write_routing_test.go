// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: cover Web API write-base resolution: no-key, cached/env user id, group, and
// the one-time keys/current lookup (against an overridable web base).

package cli

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"zotero-pp-cli/internal/config"
)

func TestResolveWebWriteBase(t *testing.T) {
	// No key -> no routing (writes hit the local read-only guard).
	if base, err := resolveWebWriteBase(&config.Config{}, "", time.Second); err != nil || base != "" {
		t.Errorf("no key: got (%q, %v), want (\"\", nil)", base, err)
	}
	// Personal with a known user id -> /users/<id>, no network.
	got, err := resolveWebWriteBase(&config.Config{ZoteroApiKey: "k", UserID: "5847066"}, "", time.Second)
	if err != nil || got != zoteroWebAPIBase+"/users/5847066" {
		t.Errorf("personal: got (%q, %v)", got, err)
	}
	// Group -> /groups/<gid>, no user id needed.
	got, err = resolveWebWriteBase(&config.Config{ZoteroApiKey: "k"}, "12345", time.Second)
	if err != nil || got != zoteroWebAPIBase+"/groups/12345" {
		t.Errorf("group: got (%q, %v)", got, err)
	}
}

func TestResolveWebWriteBaseResolvesAndCachesUserID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/keys/current" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("Zotero-API-Key") != "k" {
			http.Error(w, "missing key", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"userID":99,"username":"x","access":{}}`))
	}))
	defer srv.Close()
	old := zoteroWebAPIBase
	zoteroWebAPIBase = srv.URL
	defer func() { zoteroWebAPIBase = old }()

	cfg := &config.Config{ZoteroApiKey: "k", Path: filepath.Join(t.TempDir(), "config.toml")}
	got, err := resolveWebWriteBase(cfg, "", time.Second)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != srv.URL+"/users/99" {
		t.Errorf("base = %q, want %s/users/99", got, srv.URL)
	}
	if cfg.UserID != "99" {
		t.Errorf("user id not cached on config: %q", cfg.UserID)
	}
}
