// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean test-gaps hcaq): Cover doctor interstitial detection, fail-on policy, and cache report rendering.

package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotero-pp-cli/internal/store"
)

func TestDoctorLooksLikeDoctorInterstitial(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "cloudflare title",
			body: `<!doctype html><html><head><title>Just a moment...</title></head><body>Checking your browser before accessing Zotero.</body></html>`,
			want: "Cloudflare",
		},
		{
			name: "cloudflare turnstile",
			body: `<html><head><title>Checking your browser</title></head><script src="https://challenges.cloudflare.com/turnstile/v0/api.js"></script></html>`,
			want: "Cloudflare",
		},
		{
			name: "akamai denied",
			body: `<html><head><title>Access Denied</title></head><body>Akamai request unsuccessful.</body></html>`,
			want: "Akamai",
		},
		{
			name: "vercel challenge",
			body: `<html><head><title>Security Check</title></head><body>Vercel challenge token required.</body></html>`,
			want: "Vercel",
		},
		{
			name: "aws waf blocked",
			body: `<html><head><title>Request blocked</title></head><body>Request blocked by AWS WAF.</body></html>`,
			want: "AWS WAF",
		},
		{
			name: "datadome captcha",
			body: `<html><head><title>Captcha required</title></head><body>DataDome captcha challenge.</body></html>`,
			want: "DataDome",
		},
		{
			name: "perimeterx captcha",
			body: `<html><head><title>Access</title></head><body><div id="px-captcha">PerimeterX</div></body></html>`,
			want: "PerimeterX",
		},
		{
			name: "normal json",
			body: `{"items":[],"title":"Just a moment"}`,
			want: "",
		},
		{
			name: "empty",
			body: ``,
			want: "",
		},
		{
			name: "benign title text outside title tag",
			body: `<html><head><title>Recipe</title></head><body>Just a moment of pause cookies with Cloudflare frosting.</body></html>`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeDoctorInterstitial([]byte(tt.body)); got != tt.want {
				t.Fatalf("looksLikeDoctorInterstitial(): want %q, got %q", tt.want, got)
			}
		})
	}
}

func TestDoctorExitForFailOn(t *testing.T) {
	healthy := map[string]any{
		"config": "ok",
		"cache":  map[string]any{"status": "fresh"},
	}
	stale := map[string]any{
		"config": "ok",
		"cache":  map[string]any{"status": "stale"},
	}
	mapError := map[string]any{
		"cache": map[string]any{"status": "error"},
	}
	stringError := map[string]any{
		"api": "unreachable: timeout",
	}

	tests := []struct {
		name    string
		failOn  string
		report  map[string]any
		wantErr bool
	}{
		{name: "empty ignores healthy", failOn: "", report: healthy, wantErr: false},
		{name: "empty ignores errors", failOn: "", report: mapError, wantErr: false},
		{name: "error healthy passes", failOn: "error", report: healthy, wantErr: false},
		{name: "error ignores stale", failOn: "error", report: stale, wantErr: false},
		{name: "error catches map status", failOn: "error", report: mapError, wantErr: true},
		{name: "error catches string status", failOn: "error", report: stringError, wantErr: true},
		{name: "stale healthy passes", failOn: "stale", report: healthy, wantErr: false},
		{name: "stale catches stale", failOn: "stale", report: stale, wantErr: true},
		{name: "stale catches error", failOn: "stale", report: mapError, wantErr: true},
		{name: "unknown policy errors", failOn: "missing", report: healthy, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := doctorExitForFailOn(tt.failOn, tt.report)
			if (err != nil) != tt.wantErr {
				t.Fatalf("doctorExitForFailOn(%q): want error %t, got %v", tt.failOn, tt.wantErr, err)
			}
		})
	}
}

func TestDoctorCollectCacheReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dbPath := filepath.Join(home, ".local", "share", "zotero-pp-cli", "data.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	if err := s.SaveSyncState("items", "item-cursor", 12); err != nil {
		t.Fatalf("save fresh sync state: %v", err)
	}
	if err := s.SaveSyncState("collections", "collection-cursor", 3); err != nil {
		t.Fatalf("save stale sync state: %v", err)
	}
	staleAt := time.Now().Add(-3 * time.Hour)
	if _, err := s.DB().Exec(`UPDATE sync_state SET last_synced_at = ? WHERE resource_type = ?`, staleAt, "collections"); err != nil {
		t.Fatalf("age sync state: %v", err)
	}

	rep := collectCacheReport(context.Background(), "1h")
	if got := rep["status"]; got != "stale" {
		t.Fatalf("status: want stale, got %v", got)
	}
	if got := rep["db_path"]; got != dbPath {
		t.Fatalf("db_path: want %q, got %v", dbPath, got)
	}
	if got := rep["stale_after"]; got != "1h0m0s" {
		t.Fatalf("stale_after: want 1h0m0s, got %v", got)
	}
	if _, ok := rep["oldest_age"].(string); !ok {
		t.Fatalf("oldest_age missing from stale report: %#v", rep)
	}

	resources, ok := rep["resources"].([]map[string]any)
	if !ok {
		t.Fatalf("resources has unexpected type %T", rep["resources"])
	}
	byType := doctorTestResourcesByType(resources)
	doctorTestAssertResource(t, byType, "collections", int64(3), false)
	doctorTestAssertResource(t, byType, "items", int64(12), false)
}

func TestDoctorCollectCacheReportMissingDatabase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	rep := collectCacheReport(context.Background(), "1h")
	if got := rep["status"]; got != "unknown" {
		t.Fatalf("status: want unknown, got %v", got)
	}
	if !strings.Contains(rep["hint"].(string), "sync") {
		t.Fatalf("hint should tell the user how to hydrate the cache, got %v", rep["hint"])
	}
}

func TestDoctorRenderCacheReport(t *testing.T) {
	rep := map[string]any{
		"status":         "stale",
		"db_path":        "/tmp/zotero-pp-cli/data.db",
		"schema_version": 1,
		"db_bytes":       int64(4096),
		"stale_after":    "1h0m0s",
		"oldest_age":     "3h0m0s",
		"resources": []map[string]any{
			{"type": "collections", "rows": int64(3), "staleness": "3h0m0s"},
			{"type": "items", "rows": int64(12), "staleness": "5m0s"},
		},
		"hint": "Some resources are older than stale_after; run 'zotero-pp-cli sync' to refresh.",
	}

	var buf bytes.Buffer
	renderCacheReport(&buf, rep)
	out := buf.String()
	for _, want := range []string{
		"WARN Cache: stale",
		"db_path: /tmp/zotero-pp-cli/data.db",
		"schema_version: 1",
		"db_bytes: 4096",
		"stale_after: 1h0m0s",
		"oldest_age: 3h0m0s",
		"resources:",
		"- collections: 3 rows, 3h0m0s",
		"- items: 12 rows, 5m0s",
		"hint: Some resources are older than stale_after",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered cache report missing %q in:\n%s", want, out)
		}
	}
}

func doctorTestResourcesByType(resources []map[string]any) map[string]map[string]any {
	byType := make(map[string]map[string]any, len(resources))
	for _, resource := range resources {
		rtype, _ := resource["type"].(string)
		byType[rtype] = resource
	}
	return byType
}

func doctorTestAssertResource(t *testing.T, resources map[string]map[string]any, rtype string, rows int64, never bool) {
	t.Helper()
	resource, ok := resources[rtype]
	if !ok {
		t.Fatalf("resource %q missing from %#v", rtype, resources)
	}
	if got := resource["rows"]; got != rows {
		t.Fatalf("%s rows: want %d, got %v", rtype, rows, got)
	}
	if _, ok := resource["last_synced_at"].(string); !ok && !never {
		t.Fatalf("%s last_synced_at missing from %#v", rtype, resource)
	}
	if got := resource["staleness"]; (got == "never") != never {
		t.Fatalf("%s staleness never=%t, got %v", rtype, never, got)
	}
}
