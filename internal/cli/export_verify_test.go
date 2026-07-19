// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// export snapshot verify drift-classification coverage.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestExportSnapshotVerifyIdenticalLibraryIsUnchanged(t *testing.T) {
	items := []json.RawMessage{
		exportVerifyTestItem("UNCHANGED1", 1, "First"),
		exportVerifyTestItem("UNCHANGED2", 7, "Second"),
	}
	lockPath := writeExportVerifyTestLockfile(t, "library", items)

	for _, failOnDrift := range []bool{false, true} {
		t.Run(exportVerifyFailOnDriftName(failOnDrift), func(t *testing.T) {
			report, err := runExportSnapshotVerifyTestCmd(t, lockPath, items, failOnDrift)
			if err != nil {
				t.Fatalf("verify identical library failOnDrift=%v: %v", failOnDrift, err)
			}
			want := exportVerifySummary{Unchanged: 2}
			if report.Summary != want {
				t.Fatalf("summary = %+v, want %+v", report.Summary, want)
			}
			assertExportVerifyClasses(t, report, map[string]string{
				"UNCHANGED1": "unchanged",
				"UNCHANGED2": "unchanged",
			})
		})
	}
}

func TestExportSnapshotVerifyVersionChurnIsTouchedNotDrift(t *testing.T) {
	lockItems := []json.RawMessage{exportVerifyTestItem("TOUCHED1", 1, "Stable Content")}
	currentItems := []json.RawMessage{exportVerifyTestItem("TOUCHED1", 2, "Stable Content")}
	lockPath := writeExportVerifyTestLockfile(t, "library", lockItems)

	report, err := runExportSnapshotVerifyTestCmd(t, lockPath, currentItems, true)
	if err != nil {
		t.Fatalf("verify touched item with --fail-on-drift: %v", err)
	}
	want := exportVerifySummary{Touched: 1}
	if report.Summary != want {
		t.Fatalf("summary = %+v, want %+v", report.Summary, want)
	}
	if len(report.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(report.Items))
	}
	item := report.Items[0]
	if item.Class != "touched" || item.LockVersion != 1 || item.CurrentVersion != 2 {
		t.Fatalf("item = %+v, want touched version 1 -> 2", item)
	}
	if item.LockContentSHA256 == "" || item.LockContentSHA256 != item.CurrentContentSHA256 {
		t.Fatalf("content hashes = lock %q current %q, want same non-empty hash", item.LockContentSHA256, item.CurrentContentSHA256)
	}
}

func TestExportSnapshotVerifyContentChangeFailsDriftGate(t *testing.T) {
	lockItems := []json.RawMessage{exportVerifyTestItem("CHANGED1", 1, "Original Title")}
	currentItems := []json.RawMessage{exportVerifyTestItem("CHANGED1", 2, "Edited Title")}
	lockPath := writeExportVerifyTestLockfile(t, "library", lockItems)

	report, err := runExportSnapshotVerifyTestCmd(t, lockPath, currentItems, true)
	if code := ExitCode(err); code != 11 {
		t.Fatalf("ExitCode(err) = %d (%v), want 11", code, err)
	}
	want := exportVerifySummary{Changed: 1}
	if report.Summary != want {
		t.Fatalf("summary = %+v, want %+v", report.Summary, want)
	}
	if len(report.Items) != 1 || report.Items[0].Class != "changed" {
		t.Fatalf("items = %+v, want one changed item", report.Items)
	}
	if report.Items[0].LockContentSHA256 == report.Items[0].CurrentContentSHA256 {
		t.Fatalf("changed item content hashes both %q, want different hashes", report.Items[0].LockContentSHA256)
	}
}

func TestExportSnapshotVerifyResolvedScopeReportsRemovedAddedAndSummary(t *testing.T) {
	lockItems := []json.RawMessage{
		exportVerifyTestItem("REMOVED1", 1, "Removed"),
		exportVerifyTestItem("CHANGED1", 1, "Original"),
		exportVerifyTestItem("TOUCHED1", 1, "Touched"),
		exportVerifyTestItem("SAME1", 1, "Same"),
	}
	currentItems := []json.RawMessage{
		exportVerifyTestItem("CHANGED1", 2, "Edited"),
		exportVerifyTestItem("TOUCHED1", 2, "Touched"),
		exportVerifyTestItem("SAME1", 1, "Same"),
		exportVerifyTestItem("ADDED1", 1, "Added"),
	}
	lockPath := writeExportVerifyTestLockfile(t, "library", lockItems)

	report, err := runExportSnapshotVerifyTestCmd(t, lockPath, currentItems, false)
	if err != nil {
		t.Fatalf("verify mixed drift: %v", err)
	}
	if report.AddedDetection != "resolved" || report.AddedDetectionReason != "" {
		t.Fatalf("added detection = %q reason %q, want resolved with no reason", report.AddedDetection, report.AddedDetectionReason)
	}
	want := exportVerifySummary{Added: 1, Removed: 1, Changed: 1, Touched: 1, Unchanged: 1}
	if report.Summary != want {
		t.Fatalf("summary = %+v, want %+v", report.Summary, want)
	}
	assertExportVerifyClasses(t, report, map[string]string{
		"CHANGED1": "changed",
		"REMOVED1": "removed",
		"ADDED1":   "added",
		"TOUCHED1": "touched",
		"SAME1":    "unchanged",
	})
	counts := map[string]int{}
	for _, item := range report.Items {
		counts[item.Class]++
	}
	if counts["added"] != report.Summary.Added || counts["removed"] != report.Summary.Removed || counts["changed"] != report.Summary.Changed || counts["touched"] != report.Summary.Touched || counts["unchanged"] != report.Summary.Unchanged {
		t.Fatalf("per-item counts = %+v, summary = %+v", counts, report.Summary)
	}
}

func runExportSnapshotVerifyTestCmd(t *testing.T, lockPath string, currentItems []json.RawMessage, failOnDrift bool) (exportVerifyReport, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/items") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("start") != "0" || r.URL.Query().Get("limit") != "100" {
			t.Fatalf("query = %q, want start=0&limit=100", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[" + exportVerifyJoinRaw(currentItems) + "]"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	flags := &rootFlags{asJSON: true}
	cmd := newExportSnapshotVerifyCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	args := []string{lockPath}
	if failOnDrift {
		args = append(args, "--fail-on-drift")
	}
	cmd.SetArgs(args)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()

	var report exportVerifyReport
	if stdout.Len() == 0 {
		t.Fatalf("stdout is empty, want JSON report")
	}
	if decodeErr := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &report); decodeErr != nil {
		t.Fatalf("decode report %q: %v", stdout.String(), decodeErr)
	}
	return report, err
}

func writeExportVerifyTestLockfile(t *testing.T, scope string, items []json.RawMessage) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshot.jsonl.lock.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create lockfile: %v", err)
	}
	lockfile, err := buildExportLockfile(scope, "jsonl", items)
	if err != nil {
		_ = file.Close()
		t.Fatalf("build lockfile: %v", err)
	}
	if err := writeExportLockfile(file, lockfile); err != nil {
		_ = file.Close()
		t.Fatalf("write lockfile: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close lockfile: %v", err)
	}
	return path
}

func exportVerifyTestItem(key string, version int, title string) json.RawMessage {
	return json.RawMessage(`{"key":"` + key + `","version":` + strconv.Itoa(version) + `,"title":"` + title + `"}`)
}

func exportVerifyJoinRaw(items []json.RawMessage) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, string(item))
	}
	return strings.Join(parts, ",")
}

func exportVerifyFailOnDriftName(failOnDrift bool) string {
	if failOnDrift {
		return "fail-on-drift"
	}
	return "allow-drift"
}

func assertExportVerifyClasses(t *testing.T, report exportVerifyReport, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	for _, item := range report.Items {
		got[item.Key] = item.Class
	}
	if len(got) != len(want) {
		t.Fatalf("classes = %+v, want %+v", got, want)
	}
	for key, wantClass := range want {
		if got[key] != wantClass {
			t.Fatalf("class[%s] = %q, want %q (all classes %+v)", key, got[key], wantClass, got)
		}
	}
}
