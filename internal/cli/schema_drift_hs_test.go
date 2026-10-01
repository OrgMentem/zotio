// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// hsRunSchemaDrift runs `schema drift` with the given flags and keeps stderr,
// where the --dry-run publication preview goes.
func hsRunSchemaDrift(t *testing.T, baseURL string, flags *rootFlags, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", baseURL)
	cmd := newSchemaDriftCmd(flags)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// --dry-run reports what schema drift would capture or adopt but leaves the
// baseline exactly as it was: still absent on a first run, byte-identical
// under --update, with no lock or temporary file beside it.
func TestSchemaDriftDryRunNeverWritesBaseline(t *testing.T) {
	for _, tc := range []struct {
		name      string
		seed      bool
		args      []string
		wantDrift bool
		wantNote  string
	}{
		{name: "first capture", wantNote: "new file"},
		{name: "update", seed: true, args: []string{"--update"}, wantDrift: true, wantNote: "replacing the existing file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			baseline := filepath.Join(dir, "baseline.json")
			if tc.seed {
				old := schemaServer(t, []string{"book"}, []string{"title"}, []string{"firstName"})
				_, _, err := hsRunSchemaDrift(t, old.URL, &rootFlags{asJSON: true}, "--baseline", baseline)
				old.Close()
				if err != nil {
					t.Fatalf("seed baseline: %v", err)
				}
			}
			before, beforeErr := os.ReadFile(baseline)
			beforeEntries := hsDirEntries(t, dir)

			live := schemaServer(t, []string{"book", "preprint"}, []string{"title"}, []string{"firstName"})
			defer live.Close()
			out, stderr, err := hsRunSchemaDrift(t, live.URL, &rootFlags{asJSON: true, dryRun: true},
				append([]string{"--baseline", baseline}, tc.args...)...)
			if err != nil {
				t.Fatalf("dry run: %v", err)
			}
			var res struct {
				Captured bool `json:"baseline_captured"`
				Drift    bool `json:"drift"`
				DryRun   bool `json:"dry_run"`
			}
			if err := json.Unmarshal([]byte(out), &res); err != nil {
				t.Fatalf("decode output %q: %v", out, err)
			}
			if res.Captured || !res.DryRun || res.Drift != tc.wantDrift {
				t.Fatalf("result = %+v, want baseline_captured=false dry_run=true drift=%v", res, tc.wantDrift)
			}

			after, afterErr := os.ReadFile(baseline)
			if tc.seed {
				if beforeErr != nil || afterErr != nil || !bytes.Equal(before, after) {
					t.Fatalf("baseline changed under --dry-run (before err %v, after err %v)", beforeErr, afterErr)
				}
			} else if !errors.Is(afterErr, os.ErrNotExist) {
				t.Fatalf("first-capture dry run created the baseline: %v", afterErr)
			}
			if afterEntries := hsDirEntries(t, dir); !slices.Equal(afterEntries, beforeEntries) {
				t.Fatalf("dir after dry run = %v, want %v: no baseline, temporary or lock file", afterEntries, beforeEntries)
			}
			if line := hsLineContaining(stderr, baseline); !strings.Contains(line, tc.wantNote) {
				t.Errorf("stderr = %q, want the baseline target reported with %q", stderr, tc.wantNote)
			}
		})
	}
}
