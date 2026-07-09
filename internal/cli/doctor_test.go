// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"zotio/internal/connector"
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
