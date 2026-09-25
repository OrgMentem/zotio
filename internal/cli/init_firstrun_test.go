// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zotio/internal/store"

	"github.com/spf13/cobra"
)

// The fresh-install first-run flow (local-API probe, initial sync, health
// verdict) had no witness: a broken init shipped green and surfaced only on a
// real library. This runs init end to end against an httptest Zotero that
// answers every sync resource with an empty page, in an isolated HOME with a
// seeded API key, and pins the completed checkpoint plus the health verdict.
func TestInitFirstRunSyncsEmptyLibraryEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{
		"ZOTERO_CONFIG",
		"ZOTERO_API_KEY",
		"ZOTERO_BASE_URL",
		"ZOTERO_USER_ID",
		"ZOTERO_PROFILE",
		"ZOTERO_GROUP",
		"ZOTERO_HOME",
		"ZOTERO_CONFIG_DIR",
		"ZOTERO_DATA_DIR",
		"ZOTERO_STATE_DIR",
		"ZOTERO_CACHE_DIR",
		"XDG_CONFIG_HOME",
		"XDG_DATA_HOME",
		"XDG_STATE_HOME",
		"XDG_CACHE_HOME",
		"ZOTIO_DEMO",
	} {
		t.Setenv(key, "")
	}
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_API_KEY", "FIXTURE-init-api-key")

	flags := &rootFlags{noInput: true, timeout: 5 * time.Second}
	cmd := &cobra.Command{Use: "init"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	report, err := runInit(cmd, flags, false)
	if err != nil {
		t.Fatalf("runInit: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if !report.OK {
		t.Fatalf("report.OK = false, want a clean first run: %+v\nstderr:\n%s", report.Steps, stderr.String())
	}
	steps := map[string]initStepReport{}
	for _, step := range report.Steps {
		steps[step.Step] = step
	}
	assertInitStep(t, steps[initStepLocalAPI], initStepLocalAPI, true, "reachable", "")
	assertInitStep(t, steps[initStepAPIKey], initStepAPIKey, true, "configured", "")
	assertInitStep(t, steps[initStepSync], initStepSync, true, "synced", "")
	if len(report.Sync) != len(defaultSyncResources()) {
		t.Fatalf("synced %d resources, want %d (%v)", len(report.Sync), len(defaultSyncResources()), report.Sync)
	}
	for _, res := range report.Sync {
		if res.Status != "ok" {
			t.Errorf("resource %s status = %q (%q), want ok", res.Resource, res.Status, res.Error)
		}
	}
	// The empty library skips one check (nothing to audit), which the verdict
	// names explicitly.
	if report.HealthVerdict != "quick health: no findings (1 check(s) skipped)" {
		t.Fatalf("health verdict = %q, want the empty-library verdict", report.HealthVerdict)
	}

	// The run must leave a completed sync checkpoint behind: without one the
	// next init would sync again and health would report not_synced.
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	db, err := store.OpenReadOnlyContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open synced store: %v", err)
	}
	defer db.Close()
	if v, _, err := db.StoredLibraryVersion("items"); err != nil || v != 1 {
		t.Fatalf("stored items version = %d (err %v), want 1", v, err)
	}
}
