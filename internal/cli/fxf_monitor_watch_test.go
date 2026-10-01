// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fxfEditedManifest = `{"reviewed":"by hand"}`

// fxfMonitorServer serves one OpenAlex work and counts requests.
func fxfMonitorServer(t *testing.T) *int {
	t.Helper()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]string{{"id": "W1", "doi": "10.5000/one", "title": "One", "publication_date": "2026-02-02"}}})
	}))
	t.Cleanup(server.Close)
	withBase(t, &enrichOpenAlexBase, server.URL)
	return &requests
}

func fxfWriteEditedManifest(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reviewed.json")
	if err := os.WriteFile(path, []byte(fxfEditedManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fxfAssertManifestUnchanged(t *testing.T, path string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != fxfEditedManifest {
		t.Fatalf("existing manifest = %q, %v; want it unchanged", got, err)
	}
}

func TestFxfImportMonitorRefusesExistingManifestWithoutOverwrite(t *testing.T) {
	seedImportDiscoverStore(t, nil)
	requests := fxfMonitorServer(t)
	path := fxfWriteEditedManifest(t)
	_, err := runImportMonitorTest(t, "--query", "graphs", "--since", "2026-01-01", "--out", path)
	if err == nil || ExitCode(err) != 9 || !strings.Contains(err.Error(), "--overwrite") {
		t.Fatalf("existing manifest without --overwrite = %v (exit %d); want exit 9 naming --overwrite", err, ExitCode(err))
	}
	if *requests != 0 {
		t.Fatalf("refusal made %d provider requests; want none", *requests)
	}
	fxfAssertManifestUnchanged(t, path)
}

func TestFxfImportMonitorOverwriteReplacesAndReports(t *testing.T) {
	seedImportDiscoverStore(t, nil)
	fxfMonitorServer(t)
	path := fxfWriteEditedManifest(t)
	report, err := runImportMonitorTest(t, "--query", "graphs", "--since", "2026-01-01", "--out", path, "--overwrite")
	if err != nil || !report.Replaced || report.DryRun || report.Emitted != 1 {
		t.Fatalf("--overwrite report = %+v, %v; want replaced with one entry", report, err)
	}
	manifest, err := readImportManifest(path, nil)
	if err != nil || len(manifest.Entries) != 1 || manifest.Entries[0].Identifier != "10.5000/one" {
		t.Fatalf("replaced manifest = %+v, %v", manifest, err)
	}

	// A fresh path reports replaced=false.
	fresh := filepath.Join(t.TempDir(), "fresh.json")
	report, err = runImportMonitorTest(t, "--query", "graphs", "--since", "2026-01-01", "--out", fresh, "--overwrite")
	if err != nil || report.Replaced {
		t.Fatalf("fresh --overwrite report = %+v, %v; want replaced=false", report, err)
	}
}

func TestFxfImportMonitorDryRunWritesNothing(t *testing.T) {
	seedImportDiscoverStore(t, nil)
	fxfMonitorServer(t)
	flags := &rootFlags{asJSON: true, noCache: true, dryRun: true, timeout: time.Second}

	path := fxfWriteEditedManifest(t)
	report, err := runImportMonitorTestWithFlags(t, flags, "--query", "graphs", "--since", "2026-01-01", "--out", path, "--overwrite")
	if err != nil || !report.DryRun || !report.Replaced || report.Emitted != 1 {
		t.Fatalf("dry-run overwrite report = %+v, %v; want would-replace", report, err)
	}
	fxfAssertManifestUnchanged(t, path)
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("dry run created a lock file: %v", err)
	}

	fresh := filepath.Join(t.TempDir(), "fresh.json")
	report, err = runImportMonitorTestWithFlags(t, flags, "--query", "graphs", "--since", "2026-01-01", "--out", fresh)
	if err != nil || !report.DryRun || report.Replaced {
		t.Fatalf("dry-run fresh report = %+v, %v", report, err)
	}
	entries, err := os.ReadDir(filepath.Dir(fresh))
	if err != nil || len(entries) != 0 {
		t.Fatalf("dry run wrote %v, %v; want an empty directory", entries, err)
	}
}

func TestFxfWatchFailedCycleLogsErrorOnceWithoutUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("HOME", t.TempDir())

	cmd := newWatchCmd(&rootFlags{timeout: time.Second})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--interval", "10s", "--once"})

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("watch --once against a failing API returned nil; stderr=%s", stderr.String())
	}
	got := stderr.String()
	if strings.Contains(got, "Usage:") {
		t.Fatalf("failed cycle printed the sync usage block:\n%s", got)
	}
	if n := strings.Count(got, err.Error()); n != 1 {
		t.Fatalf("failed cycle logged the error %d times; want once:\n%s", n, got)
	}
	if !strings.Contains(got, "cycle error: "+err.Error()) {
		t.Fatalf("stderr lacks the watch cycle error line:\n%s", got)
	}
}
