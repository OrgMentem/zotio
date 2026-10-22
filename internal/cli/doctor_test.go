// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
	"zotio/internal/connector"
	"zotio/internal/update"
	"zotio/internal/zoteroprefs"
)

func TestIsLocalZoteroAPI(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    bool
	}{
		{name: "localhost", baseURL: "http://localhost:23119/api/users/0", want: true},
		{name: "ipv4 loopback", baseURL: "http://127.0.0.1:23119/api/users/0", want: true},
		{name: "ipv6 loopback", baseURL: "http://[::1]:23119/api/users/0", want: true},
		{name: "web api", baseURL: "https://api.zotero.org/users/0", want: false},
		{name: "localhost suffix", baseURL: "http://localhost.example:23119/api/users/0", want: false},
		{name: "ipv4 suffix", baseURL: "http://127.0.0.1.example:23119/api/users/0", want: false},
		{name: "wrong port", baseURL: "http://localhost:1234/api/users/0", want: false},
		{name: "missing port", baseURL: "http://localhost/api/users/0", want: false},
		{name: "malformed", baseURL: "http://[::1/api/users/0", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLocalZoteroAPI(tt.baseURL); got != tt.want {
				t.Fatalf("isLocalZoteroAPI(%q): want %t, got %t", tt.baseURL, tt.want, got)
			}
		})
	}
}

func TestDoctorReportRedactsBaseURL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTIO_DEMO", "0")
	t.Setenv("ZOTERO_API_KEY", "")
	t.Setenv("ZOTERO_BASE_URL", "")

	oldConnectorPing := connectorPing
	connectorPing = func(context.Context, *connector.Client) error {
		return errors.New("connector ping disabled")
	}
	t.Cleanup(func() { connectorPing = oldConnectorPing })

	rawBaseURL := "http://u:sekret@localhost:23119/api/users/0?token=abc&x=1"
	flags := &rootFlags{
		asJSON:     true,
		configPath: doctorTestConfigFile(t, rawBaseURL),
		timeout:    50 * time.Millisecond,
	}
	cmd := newDoctorCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("doctor: %v; stderr=%s", err, errOut.String())
	}

	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode doctor report: %v; stdout=%s", err, out.String())
	}

	wantBaseURL := "http://***@localhost:23119/api/users/0?token=***&x=1"
	if got := report["base_url"]; got != wantBaseURL {
		t.Fatalf("report base_url: want %q, got %v", wantBaseURL, got)
	}
	hint, _ := report["auth_hint"].(string)
	if !strings.Contains(hint, wantBaseURL) {
		t.Fatalf("auth_hint = %q, want redacted base URL %q", hint, wantBaseURL)
	}
	if strings.Contains(hint, "sekret") || strings.Contains(hint, "token=abc") {
		t.Fatalf("auth_hint leaked raw base URL secret: %q", hint)
	}
}

func doctorTestConfigFile(t *testing.T, baseURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("base_url = "+strconv.Quote(baseURL)+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestDoctorUpdateRows(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.3","html_url":"https://github.com/OrgMentem/zotio/releases/tag/v1.2.3"}`))
	}))
	defer server.Close()

	enabled := &config.Config{Updates: &config.UpdatesConfig{Check: true}}
	newChecker := func() *update.Checker {
		return update.NewWithOptions(update.Options{
			DataDir:     t.TempDir(),
			ReleasesURL: server.URL,
			Client:      server.Client(),
			Now:         func() time.Time { return now },
		})
	}

	if got := updateReport(context.Background(), &config.Config{}, nil, "1.2.3", ""); !strings.HasPrefix(got, "INFO disabled") {
		t.Fatalf("disabled row = %q", got)
	}
	if got := updateReport(context.Background(), enabled, newChecker(), "1.2.3", ""); got != "OK current (1.2.3)" {
		t.Fatalf("current row = %q", got)
	}
	if got := updateReport(context.Background(), enabled, newChecker(), "1.2.2", "/opt/homebrew/bin/zotio"); got != "WARN 1.2.3 available — brew upgrade zotio" {
		t.Fatalf("behind row = %q", got)
	}
	if got := updateReport(context.Background(), enabled, newChecker(), "v1.2.3-rc.1", ""); !strings.HasPrefix(got, "WARN 1.2.3 available") {
		t.Fatalf("release-candidate row = %q, want the stable release reported as available", got)
	}
	if got := updateReport(context.Background(), enabled, nil, "dev", ""); got != "INFO skipped (development build)" {
		t.Fatalf("development row = %q", got)
	}
}

// The JSON envelope has always carried file_storage_profile and
// file_storage_hint; the human renderer at one point dropped both, so an
// operator running plain `zotio doctor` had no way to see the actionable
// remediation (e.g. pinning ZOTERO_PROFILE_DIR) that --json exposed. This
// exercises the actual command's human output path, not the report map
// directly, so it fails if the rendering is removed rather than just the
// data.
func TestDoctorHumanOutputShowsFileStorageProfileAndHint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTIO_DEMO", "0")
	t.Setenv("ZOTERO_API_KEY", "")
	t.Setenv("ZOTERO_BASE_URL", "")

	oldConnectorPing := connectorPing
	connectorPing = func(context.Context, *connector.Client) error {
		return errors.New("connector ping disabled")
	}
	t.Cleanup(func() { connectorPing = oldConnectorPing })

	dir := t.TempDir()
	writeTestPrefs(t, dir, `
user_pref("extensions.zotero.sync.storage.protocol", "webdav");
user_pref("extensions.zotero.sync.storage.url", "nas.example.com/zotero");
`)
	ambiguous, err := zoteroprefs.LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	ambiguous.Ambiguous = true
	ambiguous.ProfileCount = 2

	oldLoad := loadZoteroFileStorage
	loadZoteroFileStorage = func() (zoteroprefs.FileStorage, error) { return ambiguous, nil }
	resetZoteroFileStorageCache()
	t.Cleanup(func() { loadZoteroFileStorage = oldLoad; resetZoteroFileStorageCache() })

	flags := &rootFlags{
		configPath: doctorTestConfigFile(t, "http://localhost:23119/api/users/0"),
		timeout:    50 * time.Millisecond,
	}
	cmd := newDoctorCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("doctor: %v; stderr=%s", err, errOut.String())
	}

	stdout := out.String()
	if !strings.Contains(stdout, "profile: "+ambiguous.ProfilePath) {
		t.Fatalf("human output missing file_storage_profile line:\n%s", stdout)
	}
	if !strings.Contains(stdout, zoteroprefs.ProfileDirEnv) {
		t.Fatalf("human output missing file_storage_hint's profile-pinning instruction:\n%s", stdout)
	}
}

// runDoctorAgainst runs `doctor --json --fail-on error` against a base URL with
// an API key configured and returns the decoded report and the command error.
func runDoctorAgainst(t *testing.T, baseURL string) (map[string]any, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTIO_DEMO", "0")
	t.Setenv("ZOTERO_API_KEY", "test-key")
	t.Setenv("ZOTERO_BASE_URL", "")
	oldConnectorPing := connectorPing
	connectorPing = func(context.Context, *connector.Client) error {
		return errors.New("connector ping disabled")
	}
	t.Cleanup(func() { connectorPing = oldConnectorPing })

	flags := &rootFlags{
		asJSON:     true,
		configPath: doctorTestConfigFile(t, baseURL),
		timeout:    2 * time.Second,
	}
	cmd := newDoctorCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	_ = cmd.Flags().Set("fail-on", "error")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	var report map[string]any
	if jsonErr := json.Unmarshal(out.Bytes(), &report); jsonErr != nil {
		t.Fatalf("decode doctor report: %v; stdout=%s", jsonErr, out.String())
	}
	return report, err
}

// A rejected key answers every path with a 4xx. Reachability alone must not
// make that look healthy: doctor is the preflight agents and CI trust.
func TestDoctorReportsRejectedCredentialsAsError(t *testing.T) {
	var probed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/items" {
			probed.Store(true)
		}
		http.Error(w, "Forbidden", http.StatusForbidden)
	}))
	defer server.Close()

	report, err := runDoctorAgainst(t, server.URL)
	if !probed.Load() {
		t.Fatal("doctor never sent an authenticated library read")
	}
	if got, _ := report["credentials"].(string); !strings.HasPrefix(got, "error:") {
		t.Fatalf("credentials = %q, want an error verdict", got)
	}
	if got := report["status"]; got != "error" {
		t.Fatalf("status = %v, want error", got)
	}
	if code := ExitCode(err); code != 11 {
		t.Fatalf("--fail-on error exit code = %d (err %v), want 11", code, err)
	}
}

func TestDoctorReportsAcceptedCredentialsAsOK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	report, err := runDoctorAgainst(t, server.URL)
	if err != nil {
		t.Fatalf("doctor against a healthy server: %v", err)
	}
	if got := report["credentials"]; got != "OK accepted by the API" {
		t.Fatalf("credentials = %v, want accepted", got)
	}
}

// A non-auth failure of the probe says nothing about the key: it must not be
// reported as a credential error.
func TestDoctorCredentialProbeLeavesNonAuthFailuresUnverified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()
	c := client.New(&config.Config{BaseURL: server.URL}, 2*time.Second, 0)
	c.NoCache = true
	got := doctorCredentialProbe(c, true, false)
	if !strings.HasPrefix(got, "INFO unverified") {
		t.Fatalf("probe verdict = %q, want INFO unverified", got)
	}
}

// The desktop answers 403 to everything when "Allow other applications" is off.
func TestDoctorCredentialProbeNamesDisabledLocalAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Local API is not enabled", http.StatusForbidden)
	}))
	defer server.Close()
	c := client.New(&config.Config{BaseURL: server.URL}, 2*time.Second, 0)
	c.NoCache = true
	got := doctorCredentialProbe(c, false, true)
	if !strings.HasPrefix(got, "error:") || !strings.Contains(got, "Allow other applications") {
		t.Fatalf("probe verdict = %q, want an error naming the desktop setting", got)
	}
}

// A failed refresh that falls back to an expired cached release must not read
// as "OK current": the latest release is unknown, and the row says how old the
// evidence is. A healthy refresh recovers the verdict.
func TestDoctorUpdateRowReportsStaleWhenRefreshFails(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	var tag atomic.Value
	tag.Store("v1.2.3")
	var failing atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"` + tag.Load().(string) + `","html_url":"https://example.test/release"}`))
	}))
	defer server.Close()

	enabled := &config.Config{Updates: &config.UpdatesConfig{Check: true}}
	dataDir := t.TempDir()
	checkerAt := func(at time.Time) *update.Checker {
		return update.NewWithOptions(update.Options{
			DataDir:     dataDir,
			ReleasesURL: server.URL,
			Client:      server.Client(),
			Now:         func() time.Time { return at },
		})
	}
	// Seed the cache three days ago with the installed version as the latest.
	if got := updateReport(context.Background(), enabled, checkerAt(now.Add(-72*time.Hour)), "1.2.3", ""); got != "OK current (1.2.3)" {
		t.Fatalf("seed row = %q", got)
	}

	failing.Store(true)
	got := updateReport(context.Background(), enabled, checkerAt(now), "1.2.3", "")
	if strings.HasPrefix(got, "OK") || !strings.Contains(got, "2026-07-15") {
		t.Fatalf("row after a failed refresh = %q, want a stale verdict naming the last check date", got)
	}

	failing.Store(false)
	tag.Store("v1.2.4")
	got = updateReport(context.Background(), enabled, checkerAt(now), "1.2.3", "")
	if !strings.HasPrefix(got, "WARN 1.2.4 available") {
		t.Fatalf("row after recovery = %q, want the newer release reported", got)
	}
}
