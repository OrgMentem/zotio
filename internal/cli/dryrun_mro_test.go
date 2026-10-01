// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestMroDemoResetDryRunDeletesNothing: `demo --reset --dry-run` used to delete
// demo.db and re-seed it. A preview must leave the existing sandbox byte for
// byte and still name the files a real reset would delete.
func TestMroDemoResetDryRunDeletesNothing(t *testing.T) {
	home := isolateDemoEnv(t, "0")
	demoDB := filepath.Join(home, ".local", "share", "zotio", "demo.db")
	runDemoCmd(t)
	before, err := os.ReadFile(demoDB)
	if err != nil {
		t.Fatalf("read seeded sandbox: %v", err)
	}

	report := mroRunDemoDryRun(t, "--reset")

	after, err := os.ReadFile(demoDB)
	if err != nil {
		t.Fatalf("demo --reset --dry-run removed the sandbox: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("demo --reset --dry-run changed demo.db")
	}
	if !report.DryRun || !report.WouldSeed || !slices.Contains(report.WouldDelete, demoDB) {
		t.Fatalf("dry-run report = %+v, want dry_run, would_seed, and %s in would_delete", report, demoDB)
	}
}

// TestMroDemoDryRunCreatesNothing: on a fresh home, `demo --dry-run` reports
// that it would seed but creates neither the sandbox directory nor demo.db.
func TestMroDemoDryRunCreatesNothing(t *testing.T) {
	home := isolateDemoEnv(t, "0")
	dir := filepath.Join(home, ".local", "share", "zotio")

	report := mroRunDemoDryRun(t)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("demo --dry-run created %s (stat err = %v)", dir, err)
	}
	if !report.WouldSeed || len(report.WouldDelete) != 0 {
		t.Fatalf("dry-run report = %+v, want would_seed and nothing to delete", report)
	}
}

func mroRunDemoDryRun(t *testing.T, args ...string) demoDryRunReport {
	t.Helper()
	cmd := newDemoCmd(&rootFlags{asJSON: true, dryRun: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("demo --dry-run %v: %v; stderr=%s", args, err, errOut.String())
	}
	var report demoDryRunReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode demo dry-run report %q: %v", out.String(), err)
	}
	return report
}

// TestMroExportSnapshotDryRunTouchesNoFile: `export snapshot --dry-run` used to
// remove the checkpoint and truncate the existing backup before fetching. A
// preview must send no request, leave the backup, manifest, and checkpoint
// sidecars as they were, take no output lock, and report what a run would
// replace and delete.
func TestMroExportSnapshotDryRunTouchesNoFile(t *testing.T) {
	for _, format := range []string{"jsonl", "bibtex"} {
		t.Run(format, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				_, _ = w.Write([]byte(`[]`))
			}))
			t.Cleanup(srv.Close)
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

			output := filepath.Join(t.TempDir(), "backup."+format)
			manifest := output + ".manifest.json"
			checkpoint := output + ".checkpoint.json"
			seeded := map[string][]byte{
				output:     []byte("previous good backup\n"),
				manifest:   []byte(`{"count":1}`),
				checkpoint: []byte(`{"path":"/items","next_start":1,"fetched":1}`),
			}
			if format != "jsonl" {
				seeded[checkpoint+".items.jsonl"] = []byte("{\"key\":\"A\"}\n")
			}
			for p, data := range seeded {
				if err := os.WriteFile(p, data, 0o600); err != nil {
					t.Fatalf("seed %s: %v", p, err)
				}
			}

			cmd := newExportSnapshotCmd(&rootFlags{asJSON: true, dryRun: true})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetArgs([]string{"--output", output, "--format", format})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("export snapshot --dry-run: %v", err)
			}

			if requests != 0 {
				t.Errorf("requests = %d, want none for a dry run", requests)
			}
			for p, want := range seeded {
				got, err := os.ReadFile(p)
				if err != nil {
					t.Errorf("dry run removed %s: %v", p, err)
					continue
				}
				if !bytes.Equal(got, want) {
					t.Errorf("dry run changed %s: got %q, want %q", p, got, want)
				}
			}
			if _, err := os.Stat(output + ".lock"); !os.IsNotExist(err) {
				t.Errorf("dry run created the output lock (stat err = %v)", err)
			}

			var report struct {
				DryRun       bool     `json:"dry_run"`
				Resume       bool     `json:"resume"`
				WouldReplace []string `json:"would_replace"`
				WouldDelete  []string `json:"would_delete"`
			}
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatalf("decode dry-run report %q: %v", out.String(), err)
			}
			if !report.DryRun || report.Resume {
				t.Errorf("report = %+v, want dry_run without resume", report)
			}
			if !slices.Equal(report.WouldReplace, []string{output, manifest}) {
				t.Errorf("would_replace = %q, want the backup and its manifest", report.WouldReplace)
			}
			if !slices.Contains(report.WouldDelete, checkpoint) {
				t.Errorf("would_delete = %q, want the checkpoint %s", report.WouldDelete, checkpoint)
			}
		})
	}
}
