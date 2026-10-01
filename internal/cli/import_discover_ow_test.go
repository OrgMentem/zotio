// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// owSeedDiscover seeds one scoped source whose single reference resolves to a
// new work, so every successful run yields one create entry.
func owSeedDiscover(t *testing.T) *importDiscoverProviderCounters {
	t.Helper()
	seedImportDiscoverStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"SRC1","version":1,"data":{"key":"SRC1","itemType":"journalArticle","title":"Scope One","DOI":"10.6100/source1","collections":["OWC"],"dateModified":"2026-01-03T00:00:00Z"}}`),
	})
	return withImportDiscoverProviderMocks(t,
		map[string][]string{"10.6100/source1": {"10.6100/alpha"}},
		map[string]string{"10.6100/alpha": "Alpha OW"},
	)
}

func owRunDiscover(t *testing.T, flags *rootFlags, out string, extra ...string) (importDiscoverReport, error) {
	t.Helper()
	cmd := newImportDiscoverCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append([]string{"--scope", "collection:OWC", "--out", out, "--limit", "10", "--min-count", "1"}, extra...))
	if err := cmd.Execute(); err != nil {
		return importDiscoverReport{}, err
	}
	var report importDiscoverReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode discover report %q: %v", stdout.String(), err)
	}
	return report, nil
}

const owEditedManifest = `{"schema_version":2,"entries":[{"identifier":"10.6100/alpha","action":"skip","note":"reviewed: not relevant"}]}`

// An existing manifest may carry review edits that import apply acts on, so
// discover refuses to replace it without --overwrite, under --dry-run too,
// and refuses before any provider request.
func TestOwImportDiscoverRefusesExistingManifest(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		counters := owSeedDiscover(t)
		dir := t.TempDir()
		target := filepath.Join(dir, "review.json")
		if err := os.WriteFile(target, []byte(owEditedManifest), 0o600); err != nil {
			t.Fatal(err)
		}

		_, err := owRunDiscover(t, &rootFlags{asJSON: true, noCache: true, dryRun: dryRun, timeout: 5 * time.Second}, target)
		if err == nil || ExitCode(err) != 9 {
			t.Fatalf("dryRun=%t: err = %v (exit %d), want precondition exit 9", dryRun, err, ExitCode(err))
		}
		if got, readErr := os.ReadFile(target); readErr != nil || string(got) != owEditedManifest {
			t.Fatalf("dryRun=%t: edited manifest changed to %q (err=%v)", dryRun, got, readErr)
		}
		if n := counters.coci.Load() + counters.crossref.Load() + counters.semanticScholar.Load(); n != 0 {
			t.Fatalf("dryRun=%t: refusal made %d provider requests, want 0", dryRun, n)
		}
	}
}

// --overwrite replaces the manifest and the result says it did.
func TestOwImportDiscoverOverwriteReplacesAndReports(t *testing.T) {
	owSeedDiscover(t)
	target := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(target, []byte(owEditedManifest), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := owRunDiscover(t, &rootFlags{asJSON: true, noCache: true, timeout: 5 * time.Second}, target, "--overwrite")
	if err != nil {
		t.Fatalf("discover --overwrite: %v", err)
	}
	if !report.Replaced || report.DryRun {
		t.Fatalf("report = %+v, want replaced=true", report)
	}
	manifest, err := readImportManifest(target, nil)
	if err != nil {
		t.Fatalf("read replaced manifest: %v", err)
	}
	if len(manifest.Entries) != 1 || manifest.Entries[0].Action != "create" {
		t.Fatalf("manifest entries = %+v, want the freshly discovered create", manifest.Entries)
	}
}

// --dry-run computes the manifest but creates, replaces, and locks nothing.
func TestOwImportDiscoverDryRunWritesNothing(t *testing.T) {
	owSeedDiscover(t)

	t.Run("absent manifest", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "review.json")
		report, err := owRunDiscover(t, &rootFlags{asJSON: true, noCache: true, dryRun: true, timeout: 5 * time.Second}, target)
		if err != nil {
			t.Fatalf("discover --dry-run: %v", err)
		}
		if !report.DryRun || report.Replaced || report.Summary.Entries != 1 {
			t.Fatalf("report = %+v, want a dry run of one new entry", report)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("dry run created %v (err=%v)", entries, err)
		}
	})

	t.Run("existing manifest with overwrite", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "review.json")
		if err := os.WriteFile(target, []byte(owEditedManifest), 0o600); err != nil {
			t.Fatal(err)
		}
		report, err := owRunDiscover(t, &rootFlags{asJSON: true, noCache: true, dryRun: true, timeout: 5 * time.Second}, target, "--overwrite")
		if err != nil {
			t.Fatalf("discover --dry-run --overwrite: %v", err)
		}
		if !report.DryRun || !report.Replaced {
			t.Fatalf("report = %+v, want a dry run that would replace the manifest", report)
		}
		if got, readErr := os.ReadFile(target); readErr != nil || string(got) != owEditedManifest {
			t.Fatalf("dry run changed the manifest to %q (err=%v)", got, readErr)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
			t.Fatalf("dry run created siblings: %v (err=%v)", entries, err)
		}
	})
}
