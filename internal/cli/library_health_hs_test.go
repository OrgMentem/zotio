// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"zotio/internal/store"
)

// hsRunHealthCmd runs `library health` directly and keeps stderr, where the
// --dry-run publication preview goes.
func hsRunHealthCmd(t *testing.T, flags *rootFlags, args ...string) (string, string, error) {
	t.Helper()
	cmd := newLibraryHealthCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs(args)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// hsDirEntries lists dir by name, so a test can prove a run left nothing
// behind: no output, temporary file or writer lock.
func hsDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// hsLineContaining returns the first line of s that contains needle.
func hsLineContaining(s, needle string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// --dry-run must still read the baseline, diff against it and map the gate,
// but publish neither sidecar and take no output lock, which is a file too.
func TestLibraryHealthDryRunPublishesNoSidecar(t *testing.T) {
	seedBaselineHealthCommandStore(t)
	dir := t.TempDir()
	baselinePath := filepath.Join(dir, "health-baseline.json")
	reportPath := filepath.Join(dir, "health-report.json")
	// A valid baseline with no identities: every current finding is new.
	const baseline = `{"schema_version":1,"generated_at":"2026-01-01T00:00:00Z","preset":"citation","identities":[]}` + "\n"
	if err := os.WriteFile(baselinePath, []byte(baseline), 0o600); err != nil {
		t.Fatalf("seed baseline: %v", err)
	}
	before := hsDirEntries(t, dir)

	stdout, stderr, err := hsRunHealthCmd(t, &rootFlags{asJSON: true, dryRun: true},
		"--for", "citation",
		"--fail-on", "none",
		"--baseline", baselinePath,
		"--fail-on-new", "high",
		"--write-baseline", baselinePath,
		"--report", reportPath,
	)
	if code := ExitCode(err); code != 11 {
		t.Fatalf("exit = %d (%v), want 11: a dry run still evaluates the new-finding gate", code, err)
	}
	var report healthReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode stdout report %q: %v", stdout, err)
	}
	if report.Baseline == nil || !report.Baseline.Established || len(report.Baseline.New) == 0 {
		t.Fatalf("report.baseline = %+v, want the diff against the existing baseline", report.Baseline)
	}

	got, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("read baseline after dry run: %v", err)
	}
	if string(got) != baseline {
		t.Fatalf("baseline after dry run = %q, want it untouched %q", got, baseline)
	}
	if after := hsDirEntries(t, dir); !slices.Equal(after, before) {
		t.Fatalf("dir after dry run = %v, want %v: no report, temporary or lock file", after, before)
	}

	if line := hsLineContaining(stderr, baselinePath); !strings.Contains(line, "replacing the existing file") {
		t.Errorf("stderr = %q, want the baseline target reported as replacing an existing file", stderr)
	}
	if line := hsLineContaining(stderr, reportPath); !strings.Contains(line, "new file") {
		t.Errorf("stderr = %q, want the report target reported as a new file", stderr)
	}
}

// --limit trims the listed findings only. The remediation plan feeds
// `--keys-from -`, so it must still name every item the checks counted.
func TestLibraryHealthRemediationPlanIgnoresDisplayLimit(t *testing.T) {
	db := seedHealthStore(t)
	ctx := newHealthCtx("all", false)
	ctx.limit = 1
	report, err := assembleHealthReport(db, ctx, "all", healthPresets["all"], "", scopeResult{All: true, Expr: "library"})
	if err != nil {
		t.Fatalf("assembleHealthReport: %v", err)
	}

	counted := map[string]int{}
	for _, c := range report.Checks {
		counted[c.Kind] = c.Count
	}
	plan := map[string]healthRemediationPlanStep{}
	for _, step := range report.RemediationPlan {
		if step.Scoped {
			plan[step.Kind] = step
		}
	}
	for _, kind := range []string{"missing_doi", "missing_abstract"} {
		if counted[kind] <= ctx.limit {
			t.Fatalf("seed has %d %s findings, need more than --limit %d", counted[kind], kind, ctx.limit)
		}
		if listed := countFindingsByKind(report.Findings, kind); listed != ctx.limit {
			t.Fatalf("listed %s findings = %d, want the display limit %d", kind, listed, ctx.limit)
		}
		step, ok := plan[kind]
		if !ok {
			t.Fatalf("remediation plan has no scoped %s step: %+v", kind, report.RemediationPlan)
		}
		if step.Count != counted[kind] || len(step.Keys) != counted[kind] {
			t.Errorf("%s plan step = %d keys (count %d), want all %d counted items", kind, len(step.Keys), step.Count, counted[kind])
		}
	}
}

// hsSeedCollectionsOnlyStore writes a store whose collections are synced but
// whose items never were: the synced_store preflight accepts it, yet health
// has nothing to inspect.
func hsSeedCollectionsOnlyStore(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenWithContext(ctx, helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Fatalf("close store: %v", closeErr)
		}
	}()
	rows := []json.RawMessage{json.RawMessage(`{"key":"COL1","version":1,"data":{"key":"COL1","name":"Reading"}}`)}
	if _, _, err := db.UpsertBatchContext(ctx, "collections", rows); err != nil {
		t.Fatalf("seed collections: %v", err)
	}
	if err := db.SaveSyncStateContext(ctx, "collections", "", len(rows)); err != nil {
		t.Fatalf("save collections sync state: %v", err)
	}
}

// The zotio-action contract: a gated or badge run over a library whose items
// were never synced refuses with exit 9 in every output mode, whether the
// store is missing or holds only other resources, so CI can never pass an
// uninspected library with exit 0. --require-fresh keeps its own verdict, 12.
func TestLibraryHealthUnsyncedLibraryRefusesGatedRun(t *testing.T) {
	for _, tc := range []struct {
		name            string
		collectionsOnly bool
		args            []string
		wantCode        int
		wantEnvelope    bool
	}{
		{name: "no store, explicit fail-on json", args: []string{"--json", "library", "health", "--fail-on", "high"}, wantCode: 9, wantEnvelope: true},
		{name: "no store, preset default gate", args: []string{"library", "health", "--for", "citation"}, wantCode: 9},
		{name: "no store, badge", args: []string{"library", "health", "--badge"}, wantCode: 9},
		{name: "items never synced, explicit fail-on json", collectionsOnly: true, args: []string{"--json", "library", "health", "--fail-on", "high"}, wantCode: 9, wantEnvelope: true},
		{name: "items never synced, preset default gate", collectionsOnly: true, args: []string{"library", "health", "--for", "citation"}, wantCode: 9},
		{name: "items never synced, new-finding gate", collectionsOnly: true, args: []string{"library", "health", "--fail-on", "none", "--baseline", "{tmp}/baseline.json", "--fail-on-new", "high"}, wantCode: 9},
		{name: "items never synced, badge", collectionsOnly: true, args: []string{"library", "health", "--badge"}, wantCode: 9},
		{name: "items never synced, require-fresh", collectionsOnly: true, args: []string{"--json", "library", "health", "--fail-on", "high", "--require-fresh", "24h"}, wantCode: 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, out, _ := newPreflightTestRoot(t)
			if tc.collectionsOnly {
				hsSeedCollectionsOnlyStore(t)
			}
			tmp := t.TempDir()
			args := make([]string, len(tc.args))
			for i, a := range tc.args {
				args[i] = strings.ReplaceAll(a, "{tmp}", tmp)
			}
			root.SetArgs(args)
			err := root.Execute()
			if code := ExitCode(err); code != tc.wantCode {
				t.Fatalf("exit = %d (%v), want %d", code, err, tc.wantCode)
			}
			if !tc.wantEnvelope {
				return
			}
			var env preconditionUnmetEnvelope
			if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
				t.Fatalf("decode precondition envelope %q: %v", out.String(), decodeErr)
			}
			if env.Kind != "precondition_unmet" || env.Capability != "library health" || env.Precondition != preconditionSyncedStore {
				t.Fatalf("envelope = %+v, want precondition_unmet/library health/%s", env, preconditionSyncedStore)
			}
		})
	}
}
