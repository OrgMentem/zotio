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
	"testing"
	"time"

	"zotio/internal/config"
	"zotio/internal/connector"
	"zotio/internal/update"
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

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "password and query token",
			raw:  "https://u:sekret@example.com/api?token=abc&x=1",
			want: "https://u:***@example.com/api?token=***&x=1",
		},
		{
			name: "plain local URL",
			raw:  "http://localhost:23119/api/users/0",
			want: "http://localhost:23119/api/users/0",
		},
		{
			name: "malformed",
			raw:  "http://[::1/api/users/0?token=abc",
			want: "http://[::1/api/users/0?token=abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactURL(tt.raw); got != tt.want {
				t.Fatalf("redactURL(%q): want %q, got %q", tt.raw, tt.want, got)
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

	wantBaseURL := "http://u:***@localhost:23119/api/users/0?token=***&x=1"
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
	if got := updateReport(context.Background(), enabled, nil, "dev", ""); got != "INFO skipped (development build)" {
		t.Fatalf("development row = %q", got)
	}
}
