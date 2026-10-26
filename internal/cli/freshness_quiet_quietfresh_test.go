// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zotio/internal/store"
)

// TestLibraryHealthFutureSyncIsStale pins b0216142: a last_synced_at in the
// future (clock stepped back after a sync) must not read as fresh.
func TestLibraryHealthFutureSyncIsStale(t *testing.T) {
	future := time.Now().Add(48 * time.Hour)
	if got := healthFreshnessForTest(t, future, 24*time.Hour); !got.Stale {
		t.Fatalf("future sync reads fresh, want stale")
	}
	if got := healthFreshnessForTest(t, future, 24*time.Hour); !strings.Contains(got.Reason, "clock") {
		t.Fatalf("future sync reason = %q, want it to name the clock problem", got.Reason)
	}
	if got := healthFreshnessForTest(t, time.Now().Add(-time.Hour), 24*time.Hour); got.Stale {
		t.Fatalf("fresh sync reads stale: %+v", got)
	}
}

// healthFreshnessForTest exercises the same guard assembleHealthReport uses,
// through the real report builder.
func healthFreshnessForTest(t *testing.T, syncedAt time.Time, requireFresh time.Duration) *healthFreshness {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), t.TempDir()+"/data.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"ABCD2345","version":1,"data":{"key":"ABCD2345","itemType":"book","title":"Seed"}}`),
	}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.SaveSyncState("items", "cursor", 1); err != nil {
		t.Fatalf("save sync state: %v", err)
	}
	if _, err := db.DB().Exec(`UPDATE sync_state SET last_synced_at = ? WHERE resource_type = 'items'`, syncedAt.UTC()); err != nil {
		t.Fatalf("move last_synced_at: %v", err)
	}
	if _, lastSynced, _, err := db.GetSyncState("items"); err != nil || lastSynced.IsZero() {
		t.Fatalf("GetSyncState = %v, %v", lastSynced, err)
	}
	synced := syncedAt
	ctx := &healthContext{
		src:          FindingSource{Kind: "local", SyncedAt: &synced},
		requireFresh: requireFresh,
		flags:        &rootFlags{},
	}
	report, err := assembleHealthReport(localQueryStore{db}, ctx, "quick", healthPresets["quick"], "", scopeResult{All: true, Expr: "library"})
	if err != nil {
		t.Fatalf("assembleHealthReport: %v", err)
	}
	if report.Freshness == nil {
		t.Fatalf("Freshness is nil with --require-fresh %s", requireFresh)
	}
	return report.Freshness
}

// TestLibraryHealthFutureSyncExit12 runs the freshness verdict through the
// exit mapper: a future last_synced_at must refuse, not certify.
func TestLibraryHealthFutureSyncExit12(t *testing.T) {
	future := time.Now().Add(48 * time.Hour)
	fr := healthFreshnessForTest(t, future, 24*time.Hour)
	report := healthReport{Freshness: fr}
	if err := healthFreshnessExitError(report); ExitCode(err) != 12 {
		t.Fatalf("ExitCode = %d, want 12 (err=%v)", ExitCode(err), err)
	}
}

// TestDoctorFutureSyncIsNotFresh pins b0216142 on the doctor side: a future
// last_synced_at must not report the cache fresh.
func TestDoctorFutureSyncIsNotFresh(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZOTERO_CONFIG", t.TempDir()+"/missing.toml")
	dbPath := fmt.Sprintf("%s/.local/share/zotio/data.db", home)
	s, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	if err := s.SaveSyncState("items", "cursor", 5); err != nil {
		t.Fatalf("save sync state: %v", err)
	}
	if _, err := s.DB().Exec(`UPDATE sync_state SET last_synced_at = ? WHERE resource_type = ?`, time.Now().Add(48*time.Hour).UTC(), "items"); err != nil {
		t.Fatalf("move last_synced_at: %v", err)
	}
	var got sql.NullTime
	if err := s.DB().QueryRow(`SELECT last_synced_at FROM sync_state WHERE resource_type = ?`, "items").Scan(&got); err != nil {
		t.Fatalf("read last_synced_at: %v", err)
	}
	if !got.Valid {
		t.Fatalf("last_synced_at is NULL after update")
	}
	rep := collectCacheReport(context.Background(), "24h")
	if got := rep["status"]; got == "fresh" {
		t.Fatalf("doctor cache status is fresh with a future last_synced_at: %#v", rep)
	}
	resources, _ := rep["resources"].([]map[string]any)
	found := false
	for _, r := range resources {
		if r["type"] == "items" {
			found = true
			note, _ := r["note"].(string)
			if !strings.Contains(note, "clock") {
				t.Fatalf("items resource note = %q, want it to name the clock problem", note)
			}
		}
	}
	if !found {
		t.Fatalf("items missing from resources: %#v", rep["resources"])
	}
}

// TestQuietVersionPrintsNothing pins ed97c959 for the version command:
// --quiet prints nothing but keeps the exit code.
func TestQuietVersionPrintsNothing(t *testing.T) {
	flags := &rootFlags{quiet: true}
	cmd := newVersionCliCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("version --quiet: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("--quiet stdout = %q, want nothing", out.String())
	}
}

// TestQuietAuthStatusPrintsNothing pins ed97c959: auth status --quiet prints
// nothing on stdout; unauthenticated still exits non-zero via stderr.
func TestQuietAuthStatusPrintsNothing(t *testing.T) {
	isolateAuthHome(t)
	configPath := writeAuthTestConfigFile(t, "base_url = \""+authTestBaseURL+"\"\n")
	flags := &rootFlags{quiet: true, configPath: configPath}
	cmd := newAuthStatusCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.Execute()
	// No credentials configured: the exit code stays the answer.
	if ExitCode(err) == 0 {
		t.Fatalf("auth status --quiet without credentials exits 0, want non-zero")
	}
	if out.Len() != 0 {
		t.Fatalf("--quiet stdout = %q, want nothing", out.String())
	}
}

// TestQuietAuthLogoutPreviewPrintsNothing pins ed97c959: the logout preview
// prints nothing under --quiet and changes nothing.
func TestQuietAuthLogoutPreviewPrintsNothing(t *testing.T) {
	isolateAuthHome(t)
	writeAuthTestConfigFile(t, "base_url = \""+authTestBaseURL+"\"\n")
	out, err := runAuthLogoutCmd(t, &rootFlags{quiet: true})
	if err != nil {
		t.Fatalf("auth logout --quiet preview: %v (output %q)", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("--quiet stdout = %q, want nothing", out)
	}
}

// TestQuietProfileListPrintsNothing pins ed97c959 for profile list: empty or
// not, --quiet prints nothing.
func TestQuietProfileListPrintsNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	flags := &rootFlags{quiet: true}
	cmd := newProfileListCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("profile list --quiet: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("--quiet stdout = %q, want nothing", out.String())
	}
}

// TestQuietSearchPrintsNothing pins ed97c959 for search: the piped-JSON
// envelope branch must honor --quiet.
func TestQuietSearchPrintsNothing(t *testing.T) {
	oldAllow := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllow) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"key":"ABCD2345","version":1,"data":{"key":"ABCD2345","itemType":"book","title":"Attention"}}]`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", t.TempDir()+"/missing.toml")
	for name, flags := range map[string]*rootFlags{
		"piped":     {quiet: true, dataSource: "live", noCache: true},
		"json flag": {quiet: true, asJSON: true, dataSource: "live", noCache: true},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := newSearchCmd(flags)
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"attention"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("search --quiet: %v", err)
			}
			if out.Len() != 0 {
				t.Fatalf("--quiet stdout = %q, want nothing", out.String())
			}
		})
	}
}

// TestQuietItemsTopPrintsNothing pins ed97c959 for items top.
func TestQuietItemsTopPrintsNothing(t *testing.T) {
	oldAllow := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllow) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"key":"ABCD2345","version":1,"data":{"key":"ABCD2345","itemType":"book","title":"Attention"}}]`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", t.TempDir()+"/missing.toml")
	for name, flags := range map[string]*rootFlags{
		"piped":     {quiet: true, dataSource: "live", noCache: true},
		"json flag": {quiet: true, asJSON: true, dataSource: "live", noCache: true},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := newItemsTopCmd(flags)
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("items top --quiet: %v", err)
			}
			if out.Len() != 0 {
				t.Fatalf("--quiet stdout = %q, want nothing", out.String())
			}
		})
	}
}

// TestQuietSearchKeepsExitCode: --quiet suppresses output but does not change
// success/failure.
func TestQuietSearchKeepsExitCode(t *testing.T) {
	oldAllow := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllow) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"key":"ABCD2345","version":1,"data":{"key":"ABCD2345","itemType":"book","title":"Attention"}}]`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", t.TempDir()+"/missing.toml")
	cmd := newSearchCmd(&rootFlags{quiet: true, dataSource: "live", noCache: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"attention"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("search --quiet exits %v, want nil", err)
	}
}
