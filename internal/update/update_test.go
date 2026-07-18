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
			t.Fatalf("request = %s %s", r.Method, r.URL)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Fatalf("Accept = %q", got)
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
			t.Fatalf("If-None-Match = %q", got)
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
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
		t.Fatal("fresh cache made a network request")
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
