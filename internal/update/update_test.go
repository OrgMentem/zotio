// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpdateCheckFetchesFreshRelease(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/releases/latest" || r.URL.RawQuery != "" {
			t.Errorf("request = %s %s", r.Method, r.URL)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q", got)
			http.Error(w, "bad Accept", http.StatusBadRequest)
			return
		}
		w.Header().Set("ETag", `"release-1"`)
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.3","html_url":"https://github.com/OrgMentem/zotio/releases/tag/v1.2.3"}`))
	}))
	defer server.Close()

	checker := NewWithOptions(Options{
		DataDir:     t.TempDir(),
		ReleasesURL: server.URL + "/releases/latest",
		Client:      server.Client(),
		Now:         func() time.Time { return now },
	})
	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info == nil || info.LatestVersion != "1.2.3" || info.URL != "https://github.com/OrgMentem/zotio/releases/tag/v1.2.3" || !info.CheckedAt.Equal(now) {
		t.Fatalf("info = %#v", info)
	}
	cached := checker.readCache()
	if cached.ETag != `"release-1"` || cached.LatestVersion != "1.2.3" {
		t.Fatalf("cache = %#v", cached)
	}
}

func TestUpdateCheckReusesETagAfterNotModified(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("If-None-Match"); got != `"release-1"` {
			t.Errorf("If-None-Match = %q", got)
			http.Error(w, "bad If-None-Match", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	checker := NewWithOptions(Options{DataDir: t.TempDir(), ReleasesURL: server.URL, Client: server.Client(), Now: func() time.Time { return now }})
	if err := checker.writeCache(cacheEntry{ETag: `"release-1"`, LatestVersion: "1.2.3", URL: "https://example.test/release", CheckedAt: now.Add(-checkInterval)}); err != nil {
		t.Fatal(err)
	}
	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info == nil || info.LatestVersion != "1.2.3" || !info.CheckedAt.Equal(now) {
		t.Fatalf("info = %#v", info)
	}
}

func TestUpdateCheckRateLimitsNetworkRequests(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Errorf("fresh cache made a network request")
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	defer server.Close()

	checker := NewWithOptions(Options{DataDir: t.TempDir(), ReleasesURL: server.URL, Client: server.Client(), Now: func() time.Time { return now }})
	if err := checker.writeCache(cacheEntry{LatestVersion: "1.2.3", URL: "https://example.test/release", CheckedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info == nil || info.LatestVersion != "1.2.3" || calls.Load() != 0 {
		t.Fatalf("info = %#v; calls = %d", info, calls.Load())
	}
}

// A CheckedAt in the future means the clock moved back since the check. It
// must not suppress refreshes until the wall clock passes it.
func TestUpdateCheckRefreshesWhenCheckedAtIsInTheFuture(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.3","html_url":"https://example.test/release/v1.2.3"}`))
	}))
	defer server.Close()

	checker := NewWithOptions(Options{DataDir: t.TempDir(), ReleasesURL: server.URL, Client: server.Client(), Now: func() time.Time { return now }})
	if err := checker.writeCache(cacheEntry{LatestVersion: "1.2.2", URL: "https://example.test/release/v1.2.2", CheckedAt: now.Add(48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if calls.Load() != 1 || info == nil || info.LatestVersion != "1.2.3" || !info.CheckedAt.Equal(now) {
		t.Fatalf("info = %#v; calls = %d, want one refresh to 1.2.3 stamped now", info, calls.Load())
	}
}

func TestUpdateCheckSoftFailsForRateLimit(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	checker := NewWithOptions(Options{DataDir: t.TempDir(), ReleasesURL: server.URL, Client: server.Client(), Now: func() time.Time { return now }})
	if err := checker.writeCache(cacheEntry{LatestVersion: "1.2.2", URL: "https://example.test/release", CheckedAt: now.Add(-checkInterval)}); err != nil {
		t.Fatal(err)
	}
	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info == nil || info.LatestVersion != "1.2.2" || !info.CheckedAt.Equal(now.Add(-checkInterval)) {
		t.Fatalf("info = %#v", info)
	}
}

// Every failed refresh that falls back to the cache must say so; a recent or
// refreshed answer must not.
func TestUpdateCheckMarksFailedRefreshFallbackStale(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		wantOld bool
	}{
		{"rate limited", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }, true},
		{"malformed json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{not json`)) }, true},
		{"missing fields", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }, true},
		{"healthy", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"tag_name":"v1.2.3","html_url":"https://example.test/release/v1.2.3"}`))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			checker := NewWithOptions(Options{DataDir: t.TempDir(), ReleasesURL: server.URL, Client: server.Client(), Now: func() time.Time { return now }})
			if err := checker.writeCache(cacheEntry{LatestVersion: "1.2.2", URL: "https://example.test/release/v1.2.2", CheckedAt: now.Add(-checkInterval)}); err != nil {
				t.Fatal(err)
			}
			info, err := checker.Check(context.Background())
			if err != nil || info == nil {
				t.Fatalf("Check = %#v, %v", info, err)
			}
			if info.Stale != tc.wantOld {
				t.Fatalf("Stale = %v, want %v (info %#v)", info.Stale, tc.wantOld, info)
			}
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		url := server.URL
		server.Close()
		checker := NewWithOptions(Options{DataDir: t.TempDir(), ReleasesURL: url, Client: http.DefaultClient, Now: func() time.Time { return now }})
		if err := checker.writeCache(cacheEntry{LatestVersion: "1.2.2", URL: "https://example.test/release/v1.2.2", CheckedAt: now.Add(-checkInterval)}); err != nil {
			t.Fatal(err)
		}
		info, _ := checker.Check(context.Background())
		if info == nil || !info.Stale {
			t.Fatalf("info = %#v, want stale last-known metadata", info)
		}
	})

	t.Run("recent cache is not stale", func(t *testing.T) {
		checker := NewWithOptions(Options{DataDir: t.TempDir(), ReleasesURL: "http://127.0.0.1:1", Client: http.DefaultClient, Now: func() time.Time { return now }})
		if err := checker.writeCache(cacheEntry{LatestVersion: "1.2.2", URL: "https://example.test/release/v1.2.2", CheckedAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		info, _ := checker.Check(context.Background())
		if info == nil || info.Stale {
			t.Fatalf("info = %#v, want a fresh cached answer", info)
		}
	})
}

func TestCompareVersion(t *testing.T) {
	tests := []struct {
		name        string
		left, right string
		want        int
	}{
		{
			name:  "fourth segment is newer",
			left:  "1.2.3.4",
			right: "1.2.3",
			want:  1,
		},
		{
			name:  "fourth segment is older",
			left:  "1.2.3",
			right: "1.2.3.4",
			want:  -1,
		},
		{
			name:  "trailing zero segment is equal",
			left:  "1.2.3.0",
			right: "1.2.3",
			want:  0,
		},
		{
			name:  "missing trailing zero segment is equal",
			left:  "1.2",
			right: "1.2.0",
			want:  0,
		},
		{
			name:  "newer patch",
			left:  "1.2.4",
			right: "1.2.3",
			want:  1,
		},
		{
			name:  "leading v is ignored",
			left:  "v1.2.3",
			right: "1.2.3",
			want:  0,
		},
		{
			name:  "build metadata is ignored",
			left:  "1.2.3+build.7",
			right: "1.2.3",
			want:  0,
		},
		{
			name:  "pre-release is older than its release",
			left:  "1.2.3-rc.1+build.7",
			right: "v1.2.3",
			want:  -1,
		},
		{
			name:  "release is newer than its pre-release",
			left:  "v1.2.3",
			right: "v1.2.3-rc.1",
			want:  1,
		},
		{
			name:  "pre-release of a newer core outranks an older release",
			left:  "1.2.4-rc.1",
			right: "1.2.3",
			want:  1,
		},
		{
			name:  "numeric pre-release identifiers compare numerically",
			left:  "1.2.3-rc.10",
			right: "1.2.3-rc.2",
			want:  1,
		},
		{
			name:  "numeric identifier is lower than alphanumeric",
			left:  "1.2.3-1",
			right: "1.2.3-alpha",
			want:  -1,
		},
		{
			name:  "alphanumeric identifiers compare lexically",
			left:  "1.2.3-alpha",
			right: "1.2.3-beta",
			want:  -1,
		},
		{
			name:  "shorter pre-release with equal prefix is lower",
			left:  "1.2.3-alpha",
			right: "1.2.3-alpha.1",
			want:  -1,
		},
		{
			name:  "equal pre-releases",
			left:  "v1.2.3-rc.1",
			right: "1.2.3-rc.1+build.9",
			want:  0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := compareVersion(test.left, test.right); got != test.want {
				t.Fatalf("compareVersion(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
			}
		})
	}
}

func TestUpdateDevVersionNeverAppearsBehind(t *testing.T) {
	if IsNewer("1.2.3", "dev") {
		t.Fatal("development version reported behind")
	}
	if !IsDevelopmentVersion("dev") || IsDevelopmentVersion("1.2.3") {
		t.Fatal("unexpected development version detection")
	}
	if !IsNewer("v1.2.3", "1.2.2") || IsNewer("1.2.3", "1.2.3") {
		t.Fatal("unexpected released-version comparison")
	}
	if got := UpgradeHint("/opt/homebrew/bin/zotio", "https://example.test/releases"); got != "brew upgrade zotio" {
		t.Fatalf("homebrew hint = %q", got)
	}
	if got := UpgradeHint("/Applications/zotio", "https://example.test/releases"); got != "https://example.test/releases" {
		t.Fatalf("release hint = %q", got)
	}
}
