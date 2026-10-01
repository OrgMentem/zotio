// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// iduInitEnv isolates HOME and config, seeds an API key, and points the
// client at an httptest Zotero that answers every request with an empty
// page, so init reaches the first-sync decision with a reachable local API.
func iduInitEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{
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
	// A config path with no file: updates consent is unset, so an
	// interactive run asks the update question before the sync question.
	t.Setenv("ZOTERO_CONFIG", filepath.Join(home, "zotio-config.toml"))
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_API_KEY", "FIXTURE-init-api-key")
}

func TestIduInitFirstSyncConsent(t *testing.T) {
	tests := []struct {
		name       string
		flags      rootFlags
		stdin      string
		wantStatus string
		wantSynced bool
		wantPrompt bool
	}{
		{
			name:       "no-input without yes does not sync",
			flags:      rootFlags{noInput: true},
			wantStatus: "consent_required",
		},
		{
			name:       "agent without yes does not sync",
			flags:      rootFlags{agent: true, noInput: true},
			wantStatus: "consent_required",
		},
		{
			name:       "no-input with yes syncs without a prompt",
			flags:      rootFlags{noInput: true, yes: true},
			wantStatus: "synced",
			wantSynced: true,
		},
		{
			name:       "interactive decline does not sync",
			stdin:      "n\nn\n",
			wantStatus: "declined",
			wantPrompt: true,
		},
		{
			name:       "interactive empty answer defaults to yes",
			stdin:      "n\n\n",
			wantStatus: "synced",
			wantSynced: true,
			wantPrompt: true,
		},
		{
			name:       "interactive end of input does not sync",
			stdin:      "n\n",
			wantStatus: "declined",
			wantPrompt: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			iduInitEnv(t)
			flags := tc.flags
			flags.timeout = 5 * time.Second
			cmd := &cobra.Command{Use: "init"}
			cmd.SetContext(context.Background())
			cmd.SetIn(strings.NewReader(tc.stdin))
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)

			report, err := runInit(cmd, &flags, false)

			steps := map[string]initStepReport{}
			for _, step := range report.Steps {
				steps[step.Step] = step
			}
			sync := steps[initStepSync]
			if sync.Status != tc.wantStatus || sync.OK != tc.wantSynced {
				t.Fatalf("sync step = %+v, want status %q ok=%v\nstdout:\n%s\nstderr:\n%s", sync, tc.wantStatus, tc.wantSynced, stdout.String(), stderr.String())
			}
			if gotPrompt := strings.Contains(stdout.String(), "Run the first sync now?"); gotPrompt != tc.wantPrompt {
				t.Fatalf("sync prompt shown = %v, want %v; stdout=%q", gotPrompt, tc.wantPrompt, stdout.String())
			}
			_, statErr := os.Stat(helpersTestDefaultDBPath(t, "zotio"))
			if tc.wantSynced {
				if err != nil || !report.OK {
					t.Fatalf("runInit = (ok=%v, %v), want a clean first run", report.OK, err)
				}
				if statErr != nil {
					t.Fatalf("local store missing after consented sync: %v", statErr)
				}
				return
			}
			if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("local store stat err = %v, want not-exist: init must not write the store without consent", statErr)
			}
			if len(report.Sync) != 0 {
				t.Fatalf("report.Sync = %v, want no synced resources", report.Sync)
			}
			if code := ExitCode(err); code != 9 {
				t.Fatalf("ExitCode = %d (err %v), want 9 setup required", code, err)
			}
			if !strings.Contains(sync.Remediation, "zotio init --yes") {
				t.Fatalf("sync remediation = %q, want the --yes rerun", sync.Remediation)
			}
		})
	}
}

func TestIduInitHumanReportPrintsHealthVerdictOnce(t *testing.T) {
	iduInitEnv(t)
	flags := &rootFlags{noInput: true, yes: true, timeout: 5 * time.Second}
	cmd := &cobra.Command{Use: "init"}
	cmd.SetContext(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	report, err := runInit(cmd, flags, false)
	if err != nil {
		t.Fatalf("runInit: %v\nstderr:\n%s", err, stderr.String())
	}
	if report.HealthVerdict == "" {
		t.Fatal("health verdict is empty, want the quick health verdict")
	}
	stdout.Reset()
	if err := renderInitReport(cmd, flags, report); err != nil {
		t.Fatalf("renderInitReport: %v", err)
	}
	if got := strings.Count(stdout.String(), report.HealthVerdict); got != 1 {
		t.Fatalf("health verdict %q printed %d times, want once:\n%s", report.HealthVerdict, got, stdout.String())
	}
}
