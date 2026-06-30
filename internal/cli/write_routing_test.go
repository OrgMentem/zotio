// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: cover Web API write-base resolution: no-key, cached/env user id, group, and
// the one-time keys/current lookup (against an overridable web base).

package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotero-pp-cli/internal/config"
	"zotero-pp-cli/internal/connector"
)

func TestResolveWebWriteBase(t *testing.T) {
	// No key -> no routing (writes hit the local read-only guard).
	if base, err := resolveWebWriteBase(&config.Config{}, "", time.Second); err != nil || base != "" {
		t.Errorf("no key: got (%q, %v), want (\"\", nil)", base, err)
	}
	// PATCH(glean 61a2a8a9): synthetic user id (was a real account id).
	got, err := resolveWebWriteBase(&config.Config{ZoteroApiKey: "k", UserID: "99999"}, "", time.Second)
	if err != nil || got != zoteroWebAPIBase+"/users/99999" {
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

func TestConnectorBaseFromAPIBase(t *testing.T) {
	t.Parallel()

	got, ok := connectorBaseFromAPIBase("http://localhost:23119/api/users/0")
	if !ok || got != "http://localhost:23119/connector" {
		t.Fatalf("local base = (%q, %v), want connector base", got, ok)
	}
	got, ok = connectorBaseFromAPIBase("https://api.zotero.org/users/5847066")
	if ok || got != "" {
		t.Fatalf("web base = (%q, %v), want no connector base", got, ok)
	}
}

func TestResolveCreateVia(t *testing.T) {
	oldPing := connectorPing
	defer func() { connectorPing = oldPing }()
	connectorPing = func(ctx context.Context, c *connector.Client) error {
		if !strings.HasSuffix(c.BaseURL, "/connector") {
			return fmt.Errorf("unexpected connector base %q", c.BaseURL)
		}
		return nil
	}

	localFlags := rootFlags{configPath: testConfigFile(t, "http://localhost:23119/api/users/0"), via: "auto", timeout: time.Second}
	got, err := localFlags.resolveCreateVia(context.Background(), false)
	if err != nil || got != "connector" {
		t.Fatalf("auto local reachable = (%q, %v), want connector", got, err)
	}

	got, err = localFlags.resolveCreateVia(context.Background(), true)
	if err != nil || got != "connector" {
		t.Fatalf("auto with collection = (%q, %v), want connector", got, err)
	}

	groupFlags := rootFlags{configPath: testConfigFile(t, "http://localhost:23119/api/users/0"), via: "auto", group: "12345", timeout: time.Second}
	got, err = groupFlags.resolveCreateVia(context.Background(), false)
	if err != nil || got != "web" {
		t.Fatalf("auto group = (%q, %v), want web", got, err)
	}

	explicitGroupFlags := rootFlags{configPath: testConfigFile(t, "http://localhost:23119/api/users/0"), via: "connector", group: "12345", timeout: time.Second}
	if got, err := explicitGroupFlags.resolveCreateVia(context.Background(), false); err == nil || got != "" {
		t.Fatalf("explicit connector group = (%q, %v), want error", got, err)
	}

	webFlags := rootFlags{configPath: testConfigFile(t, "https://api.zotero.org/users/5847066"), via: "connector", timeout: time.Second}
	if got, err := webFlags.resolveCreateVia(context.Background(), false); err == nil || got != "" {
		t.Fatalf("explicit connector non-local = (%q, %v), want error", got, err)
	}
}

func testConfigFile(t *testing.T, baseURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("base_url = %q\n", baseURL)), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
