// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

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

	"zotio/internal/config"
	"zotio/internal/connector"
)

func TestResolveWebWriteBase(t *testing.T) {
	// No key -> no routing (writes hit the local read-only guard).
	if base, err := resolveWebWriteBase(context.Background(), &config.Config{}, "", time.Second); err != nil || base != "" {
		t.Errorf("no key: got (%q, %v), want (\"\", nil)", base, err)
	}
	got, err := resolveWebWriteBase(context.Background(), &config.Config{ZoteroApiKey: "k", UserID: "99999"}, "", time.Second)
	if err != nil || got != zoteroWebAPIBase+"/users/99999" {
		t.Errorf("personal: got (%q, %v)", got, err)
	}
	// Group -> /groups/<gid>, no user id needed.
	got, err = resolveWebWriteBase(context.Background(), &config.Config{ZoteroApiKey: "k"}, "12345", time.Second)
	if err != nil || got != zoteroWebAPIBase+"/groups/12345" {
		t.Errorf("group: got (%q, %v)", got, err)
	}
}

func TestResolveWebWriteBaseResolvesAndCachesUserID(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = oldAllowPrivateOutbound })

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
	got, err := resolveWebWriteBase(context.Background(), cfg, "", time.Second)
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

func TestFetchZoteroUserIDHonorsCanceledContext(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"userID":99}`))
	}))
	defer srv.Close()
	old := zoteroWebAPIBase
	zoteroWebAPIBase = srv.URL
	defer func() { zoteroWebAPIBase = old }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fetchZoteroUserID(ctx, &config.Config{ZoteroApiKey: "k"}, time.Second); err == nil {
		t.Fatal("fetchZoteroUserID returned nil error for canceled context")
	}
	if hits != 0 {
		t.Fatalf("server hits = %d, want 0 for pre-canceled context", hits)
	}
}

func TestKeyGroupWriteAccessHonorsCanceledContext(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"access":{"groups":{"123":{"write":true}}}}`))
	}))
	defer srv.Close()
	old := zoteroWebAPIBase
	zoteroWebAPIBase = srv.URL
	defer func() { zoteroWebAPIBase = old }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canWrite, known := keyGroupWriteAccess(ctx, &config.Config{ZoteroApiKey: "k"}, time.Second, "123")
	if canWrite || known {
		t.Fatalf("keyGroupWriteAccess = (%v, %v), want (false, false) for canceled context", canWrite, known)
	}
	if hits != 0 {
		t.Fatalf("server hits = %d, want 0 for pre-canceled context", hits)
	}
}

func TestKeyMetadataReadersRefuseCrossOriginRedirects(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = oldAllowPrivateOutbound })

	readers := []struct {
		name string
		read func() bool
	}{
		{
			name: "user_id",
			read: func() bool {
				_, err := fetchZoteroUserID(context.Background(), &config.Config{ZoteroApiKey: "k"}, time.Second)
				return err == nil
			},
		},
		{
			name: "group_access",
			read: func() bool {
				canWrite, known := keyGroupWriteAccess(context.Background(), &config.Config{ZoteroApiKey: "k"}, time.Second, "123")
				return canWrite || known
			},
		},
	}
	redirects := []struct {
		name   string
		status int
	}{
		{name: "302", status: http.StatusFound},
		{name: "307_body_preserving", status: http.StatusTemporaryRedirect},
	}

	for _, reader := range readers {
		for _, redirect := range redirects {
			t.Run(reader.name+"/"+redirect.name, func(t *testing.T) {
				targetHits := 0
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					targetHits++
					_, _ = w.Write([]byte(`{"userID":99,"access":{"groups":{"123":{"write":true}}}}`))
				}))
				defer target.Close()

				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, target.URL+"/keys/current", redirect.status)
				}))
				defer source.Close()

				oldBase := zoteroWebAPIBase
				zoteroWebAPIBase = source.URL
				defer func() { zoteroWebAPIBase = oldBase }()

				if reader.read() {
					t.Fatal("cross-origin redirect unexpectedly succeeded")
				}
				if targetHits != 0 {
					t.Fatalf("redirect target hits = %d, want 0", targetHits)
				}
			})
		}
	}
}

func TestKeyMetadataReadersFollowSameOriginRedirect(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = oldAllowPrivateOutbound })

	redirectHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/keys/current":
			http.Redirect(w, r, "/keys/redirected", http.StatusFound)
		case "/keys/redirected":
			redirectHits++
			if got := r.Header.Get("Zotero-API-Key"); got != "k" {
				http.Error(w, "missing key", http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"userID":99,"access":{"groups":{"123":{"write":true}}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldBase := zoteroWebAPIBase
	zoteroWebAPIBase = srv.URL
	defer func() { zoteroWebAPIBase = oldBase }()

	userID, err := fetchZoteroUserID(context.Background(), &config.Config{ZoteroApiKey: "k"}, time.Second)
	if err != nil || userID != "99" {
		t.Fatalf("fetchZoteroUserID = (%q, %v), want (99, nil)", userID, err)
	}
	canWrite, known := keyGroupWriteAccess(context.Background(), &config.Config{ZoteroApiKey: "k"}, time.Second, "123")
	if !canWrite || !known {
		t.Fatalf("keyGroupWriteAccess = (%v, %v), want (true, true)", canWrite, known)
	}
	if redirectHits != 2 {
		t.Fatalf("same-origin redirect hits = %d, want 2", redirectHits)
	}
}

func TestKeyMetadataReadersBoundAndValidateResponse(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = oldAllowPrivateOutbound })

	valid := `{"userID":99,"access":{"groups":{"123":{"write":true}}}}`
	atLimit := valid + strings.Repeat(" ", int(maxKeyMetadataResponseBytes)-len(valid))
	cases := []struct {
		name   string
		body   string
		wantOK bool
	}{
		{name: "exactly_at_limit", body: atLimit, wantOK: true},
		{name: "over_limit", body: atLimit + " ", wantOK: false},
		{name: "malformed_within_limit", body: `{"userID":99`, wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			oldBase := zoteroWebAPIBase
			zoteroWebAPIBase = srv.URL
			defer func() { zoteroWebAPIBase = oldBase }()

			userID, err := fetchZoteroUserID(context.Background(), &config.Config{ZoteroApiKey: "k"}, time.Second)
			userOK := err == nil && userID == "99"
			if userOK != tc.wantOK {
				t.Fatalf("fetchZoteroUserID = (%q, %v), success = %v, want %v", userID, err, userOK, tc.wantOK)
			}

			canWrite, known := keyGroupWriteAccess(context.Background(), &config.Config{ZoteroApiKey: "k"}, time.Second, "123")
			groupOK := canWrite && known
			if groupOK != tc.wantOK {
				t.Fatalf("keyGroupWriteAccess = (%v, %v), success = %v, want %v", canWrite, known, groupOK, tc.wantOK)
			}
			if !tc.wantOK && (canWrite || known) {
				t.Fatalf("failed group access read = (%v, %v), want (false, false)", canWrite, known)
			}
		})
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

func TestNewWriteClientDryRunSkipsHybridRouteResolution(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = oldAllowPrivateOutbound })

	var keyMetadataRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyMetadataRequests++
		http.Error(w, "dry-run must not resolve the write route", http.StatusInternalServerError)
	}))
	defer srv.Close()
	oldBase := zoteroWebAPIBase
	zoteroWebAPIBase = srv.URL
	t.Cleanup(func() { zoteroWebAPIBase = oldBase })

	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte("base_url = \"http://localhost:23119/api/users/0\"\napi_key = \"k\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	flags := rootFlags{configPath: configPath, dryRun: true, timeout: time.Second, ctx: context.Background()}
	c, err := flags.newWriteClient()
	if err != nil {
		t.Fatalf("new dry-run write client: %v", err)
	}
	if c.ResolveWriteBase != nil {
		t.Fatal("dry-run write client retained a lazy write-route resolver")
	}
	if _, _, err := c.Post("/items", map[string]string{"itemType": "book"}); err != nil {
		t.Fatalf("dry-run post: %v", err)
	}
	if keyMetadataRequests != 0 {
		t.Fatalf("key metadata requests = %d, want 0 during dry-run", keyMetadataRequests)
	}
}
