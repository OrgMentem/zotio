// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// owExportServer answers every page request with the same three items, so an
// export finishes after one page.
func owExportServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Total-Results", "3")
		_, _ = w.Write([]byte(exportPageBody("OW", 3)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func owRunExportJSON(t *testing.T, flags *rootFlags, target string) exportFileResult {
	t.Helper()
	cmd := newExportCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"items", "--output", target})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export: %v", err)
	}
	var res exportFileResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode export result %q: %v", out.String(), err)
	}
	return res
}

// --dry-run must not create, replace, or lock the named output, yet still
// report the record count and whether an existing file would be replaced.
func TestOwExportDryRunWritesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	useTestServer(t, owExportServer(t))

	for _, tc := range []struct {
		name     string
		existing bool
	}{
		{name: "absent target", existing: false},
		{name: "existing target", existing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "items.jsonl")
			const previous = "curated backup\n"
			if tc.existing {
				if err := os.WriteFile(target, []byte(previous), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			res := owRunExportJSON(t, &rootFlags{asJSON: true, dryRun: true}, target)
			if !res.DryRun || res.Replaced != tc.existing || res.Records != 3 || res.Output != target {
				t.Fatalf("result = %+v, want dry_run, replaced=%t, 3 records for %s", res, tc.existing, target)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			wantEntries := 0
			if tc.existing {
				wantEntries = 1
				got, err := os.ReadFile(target)
				if err != nil || string(got) != previous {
					t.Fatalf("dry run changed the existing file to %q (err=%v)", got, err)
				}
			}
			if len(entries) != wantEntries {
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Fatalf("dry run left %v in the output directory, want %d entries", names, wantEntries)
			}
		})
	}
}

// A real run replaces an existing file (re-running an export is a normal
// scripted workflow) and says that it did.
func TestOwExportReportsReplacement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	useTestServer(t, owExportServer(t))
	target := filepath.Join(t.TempDir(), "items.jsonl")

	first := owRunExportJSON(t, &rootFlags{asJSON: true, noCache: true}, target)
	if first.Replaced || first.DryRun || first.Records != 3 {
		t.Fatalf("first result = %+v, want a new file with 3 records", first)
	}
	if err := os.WriteFile(target, []byte("curated backup\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	second := owRunExportJSON(t, &rootFlags{asJSON: true, noCache: true}, target)
	if !second.Replaced || second.DryRun || second.Records != 3 {
		t.Fatalf("second result = %+v, want replaced=true with 3 records", second)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte("curated backup")) || bytes.Count(got, []byte("\n")) != 3 {
		t.Fatalf("target after replacement = %q, want the 3 exported records", got)
	}
}
