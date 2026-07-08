// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"zotio/internal/store"
)

func TestLibraryHealthBaselineEstablishingRunWritesSchemaAndUsesAbsoluteBadge(t *testing.T) {
	home := seedBaselineHealthCommandStore(t)
	baselinePath := filepath.Join(home, "health-baseline.json")
	reportPath := filepath.Join(home, "establish-report.json")

	stdout, err := runLibraryHealthBaselineCmd(t, nil,
		"--for", "citation",
		"--fail-on", "none",
		"--baseline", baselinePath,
		"--fail-on-new", "high",
		"--write-baseline", baselinePath,
		"--report", reportPath,
	)
	if err != nil {
		t.Fatalf("establishing run returned %v, want nil", err)
	}

	report := readHealthBaselineReportFile(t, reportPath)
	if report.Baseline == nil {
		t.Fatal("report.baseline is nil, want establishing baseline block")
	}
	if report.Baseline.Established {
		t.Fatalf("report.baseline.established = true, want false for missing baseline")
	}
	if len(report.Baseline.New) != 0 {
		t.Fatalf("establishing run baseline.new = %+v, want zero new findings", report.Baseline.New)
	}
	if report.Summary.Total == 0 {
		t.Fatal("seeded citation store unexpectedly produced zero absolute findings")
	}

	expectedIdentities := expectedHealthBaselineIdentities(t, "citation")
	wantLine := fmt.Sprintf("Baseline established (%d findings recorded)", len(expectedIdentities))
	if !strings.Contains(stdout, wantLine) {
		t.Fatalf("stdout = %q, want %q", stdout, wantLine)
	}
	if strings.Contains(stdout, "New since baseline:") {
		t.Fatalf("stdout = %q, want establishing message rather than delta message", stdout)
	}
	assertHealthBaselineFileSchema(t, baselinePath, "citation", expectedIdentities)
	assertNoHealthBaselineTempFiles(t, baselinePath)

	badgeReportPath := filepath.Join(home, "establish-badge-report.json")
	badgeOut, err := runLibraryHealthBaselineCmd(t, nil,
		"--for", "citation",
		"--fail-on", "none",
		"--baseline", filepath.Join(home, "missing-badge-baseline.json"),
		"--fail-on-new", "high",
		"--badge",
		"--report", badgeReportPath,
	)
	if err != nil {
		t.Fatalf("establishing badge run returned %v, want nil", err)
	}
	badgeReport := readHealthBaselineReportFile(t, badgeReportPath)
	if badgeReport.Baseline == nil || badgeReport.Baseline.Established || len(badgeReport.Baseline.New) != 0 {
		t.Fatalf("badge report baseline = %+v, want missing-baseline establishing block with zero new", badgeReport.Baseline)
	}
	badge := decodeHealthBaselineBadge(t, badgeOut)
	wantBadge := healthBadge{
		SchemaVersion: 1,
		Label:         "bibliography",
		Message:       fmt.Sprintf("%d findings", badgeReport.Summary.Total),
		Color:         "yellow",
	}
	if badge != wantBadge {
		t.Fatalf("establishing badge = %+v, want absolute badge derivation %+v", badge, wantBadge)
	}
}

func TestLibraryHealthBaselineUnchangedStoreIsBrightgreen(t *testing.T) {
	home := seedBaselineHealthCommandStore(t)
	baselinePath := filepath.Join(home, "health-baseline.json")
	if _, err := runLibraryHealthBaselineCmd(t, nil,
		"--for", "citation",
		"--fail-on", "none",
		"--write-baseline", baselinePath,
	); err != nil {
		t.Fatalf("write baseline: %v", err)
	}

	reportPath := filepath.Join(home, "unchanged-report.json")
	badgeOut, err := runLibraryHealthBaselineCmd(t, nil,
		"--for", "citation",
		"--fail-on", "none",
		"--baseline", baselinePath,
		"--fail-on-new", "high",
		"--badge",
		"--report", reportPath,
	)
	if err != nil {
		t.Fatalf("unchanged baseline run returned %v, want nil", err)
	}

	report := readHealthBaselineReportFile(t, reportPath)
	if report.Summary.Total == 0 {
		t.Fatal("seeded store has no standing findings; test cannot prove badge ignores absolute findings")
	}
	if report.Baseline == nil || !report.Baseline.Established {
		t.Fatalf("report.baseline = %+v, want established baseline", report.Baseline)
	}
	if len(report.Baseline.New) != 0 || report.Baseline.ResolvedCount != 0 {
		t.Fatalf("baseline delta = new %d resolved %d, want 0/0", len(report.Baseline.New), report.Baseline.ResolvedCount)
	}
	badge := decodeHealthBaselineBadge(t, badgeOut)
	wantBadge := healthBadge{SchemaVersion: 1, Label: "bibliography", Message: "no new findings", Color: "brightgreen"}
	if badge != wantBadge {
		t.Fatalf("unchanged badge = %+v, want %+v", badge, wantBadge)
	}
}

func TestLibraryHealthBaselineNewFindingsGateHumanAndBadge(t *testing.T) {
	home := seedBaselineHealthCommandStore(t)
	baselinePath := filepath.Join(home, "health-baseline.json")
	if _, err := runLibraryHealthBaselineCmd(t, nil,
		"--for", "citation",
		"--fail-on", "none",
		"--write-baseline", baselinePath,
	); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	upsertBaselineHealthItems(t, baselineNewIncompleteItem())

	humanReportPath := filepath.Join(home, "new-human-report.json")
	stdout, err := runLibraryHealthBaselineCmd(t, nil,
		"--for", "citation",
		"--fail-on", "none",
		"--baseline", baselinePath,
		"--fail-on-new", "high",
		"--report", humanReportPath,
	)
	if code := ExitCode(err); code != 11 {
		t.Fatalf("new finding run exit = %d (%v), want gate exit 11", code, err)
	}
	if !strings.Contains(stdout, "New since baseline:") || !strings.Contains(stdout, "NEW ") || !strings.Contains(stdout, "NEW1") {
		t.Fatalf("stdout = %q, want baseline delta and NEW-prefixed NEW1 finding", stdout)
	}
	humanReport := readHealthBaselineReportFile(t, humanReportPath)
	assertNewBaselineFindingForKey(t, humanReport, "NEW1")

	badgeReportPath := filepath.Join(home, "new-badge-report.json")
	badgeOut, err := runLibraryHealthBaselineCmd(t, nil,
		"--for", "citation",
		"--fail-on", "none",
		"--baseline", baselinePath,
		"--fail-on-new", "high",
		"--badge",
		"--report", badgeReportPath,
	)
	if code := ExitCode(err); code != 11 {
		t.Fatalf("new finding badge exit = %d (%v), want gate exit 11", code, err)
	}
	badgeReport := readHealthBaselineReportFile(t, badgeReportPath)
	assertNewBaselineFindingForKey(t, badgeReport, "NEW1")
	newSummary := healthSummaryForFindings(badgeReport.Baseline.New)
	if newSummary.High == 0 || newSummary.Critical != 0 {
		t.Fatalf("new summary = %+v, want new high findings and no new critical findings", newSummary)
	}
	if badgeReport.Summary.Critical == 0 {
		t.Fatal("seeded store has no standing critical findings; test cannot prove badge counts new findings only")
	}
	badge := decodeHealthBaselineBadge(t, badgeOut)
	wantBadge := healthBadge{SchemaVersion: 1, Label: "bibliography", Message: healthBadgeSeverityMessage(newSummary), Color: "red"}
	if badge != wantBadge {
		t.Fatalf("new-finding badge = %+v, want new-only severity badge %+v", badge, wantBadge)
	}
}

func TestLibraryHealthBaselineResolvedUsageReportConflictAndFailOnNone(t *testing.T) {
	home := seedBaselineHealthCommandStore(t)
	baselinePath := filepath.Join(home, "health-baseline.json")
	if _, err := runLibraryHealthBaselineCmd(t, nil,
		"--for", "citation",
		"--fail-on", "none",
		"--write-baseline", baselinePath,
	); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	upsertBaselineHealthItems(t, baselineCompleteP1Item())

	resolvedReportPath := filepath.Join(home, "resolved-report.json")
	stdout, err := runLibraryHealthBaselineCmd(t, nil,
		"--for", "citation",
		"--fail-on", "none",
		"--baseline", baselinePath,
		"--fail-on-new", "high",
		"--report", resolvedReportPath,
	)
	if err != nil {
		t.Fatalf("resolved baseline run returned %v, want nil", err)
	}
	resolvedReport := readHealthBaselineReportFile(t, resolvedReportPath)
	if resolvedReport.Baseline == nil || !resolvedReport.Baseline.Established {
		t.Fatalf("resolved report baseline = %+v, want established baseline", resolvedReport.Baseline)
	}
	if resolvedReport.Baseline.ResolvedCount == 0 {
		t.Fatalf("resolved_count = 0 after fixing P1; report = %+v", resolvedReport.Baseline)
	}
	wantResolvedLine := fmt.Sprintf("resolved %d", resolvedReport.Baseline.ResolvedCount)
	if !strings.Contains(stdout, wantResolvedLine) {
		t.Fatalf("stdout = %q, want %q", stdout, wantResolvedLine)
	}

	failOnNoneReportPath := filepath.Join(home, "fail-on-none-report.json")
	stdout, err = runLibraryHealthBaselineCmd(t, nil,
		"--for", "citation",
		"--fail-on", "none",
		"--report", failOnNoneReportPath,
	)
	if err != nil {
		t.Fatalf("--fail-on none returned %v, want nil despite citation preset findings", err)
	}
	failOnNoneReport := readHealthBaselineReportFile(t, failOnNoneReportPath)
	if failOnNoneReport.Summary.Total == 0 {
		t.Fatal("seeded citation store unexpectedly has no findings")
	}
	if failOnNoneReport.Gate != nil || strings.Contains(stdout, "Gate:") {
		t.Fatalf("--fail-on none gate = %+v stdout = %q, want preset gate disabled", failOnNoneReport.Gate, stdout)
	}

	_, err = runLibraryHealthBaselineCmd(t, nil,
		"--for", "citation",
		"--fail-on-new", "high",
	)
	if code := ExitCode(err); code != 2 {
		t.Fatalf("--fail-on-new without --baseline exit = %d (%v), want usage exit 2", code, err)
	}

	stdout, err = runLibraryHealthBaselineCmd(t, &rootFlags{asJSON: true}, "--badge")
	if code := ExitCode(err); code != 2 {
		t.Fatalf("--badge --json exit = %d (%v), want usage exit 2", code, err)
	}
	if stdout != "" {
		t.Fatalf("--badge --json stdout = %q, want empty stdout", stdout)
	}
}

func seedBaselineHealthCommandStore(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTIO_DEMO", "")
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })
	upsertBaselineHealthItems(t, baselineSeedHealthItems()...)
	return home
}

func runLibraryHealthBaselineCmd(t *testing.T, flags *rootFlags, args ...string) (string, error) {
	t.Helper()
	if flags == nil {
		flags = &rootFlags{}
	}
	cmd := newLibraryHealthCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	return out.String(), err
}

func upsertBaselineHealthItems(t *testing.T, items ...json.RawMessage) {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Fatalf("close store: %v", closeErr)
		}
	}()
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.SaveSyncState("items", "cursor", len(items)); err != nil {
		t.Fatalf("save sync state: %v", err)
	}
}

func baselineSeedHealthItems() []json.RawMessage {
	return []json.RawMessage{
		json.RawMessage(`{"key":"P1","version":1,"data":{"key":"P1","itemType":"journalArticle","title":"P1"}}`),
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"Complete","creators":[{"lastName":"Doe"}],"date":"2020","publicationTitle":"Journal X","DOI":"10/p2","abstractNote":"abs","tags":[{"tag":"x"}],"extra":"Citation Key: doe2020"}}`),
		json.RawMessage(`{"key":"A2","version":1,"data":{"key":"A2","itemType":"attachment","parentItem":"P2","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"C1","version":1,"data":{"key":"C1","itemType":"journalArticle","title":"Conflict One","creators":[{"lastName":"A"}],"date":"2021","publicationTitle":"J","DOI":"10/c1","abstractNote":"a","tags":[{"tag":"y"}],"extra":"Citation Key: same2021"}}`),
		json.RawMessage(`{"key":"C2","version":1,"data":{"key":"C2","itemType":"journalArticle","title":"Conflict Two","creators":[{"lastName":"B"}],"date":"2021","publicationTitle":"J","DOI":"10/c2","abstractNote":"a","tags":[{"tag":"y"}],"extra":"Citation Key: same2021"}}`),
		json.RawMessage(`{"key":"D1","version":1,"data":{"key":"D1","itemType":"journalArticle","title":"Dup A","creators":[{"lastName":"C"}],"date":"2018","publicationTitle":"J","DOI":"10/dup","abstractNote":"a","tags":[{"tag":"z"}],"extra":"Citation Key: dupa2018"}}`),
		json.RawMessage(`{"key":"D2","version":1,"data":{"key":"D2","itemType":"journalArticle","title":"Dup B","creators":[{"lastName":"D"}],"date":"2018","publicationTitle":"J","DOI":"10/dup","abstractNote":"a","tags":[{"tag":"z"}],"extra":"Citation Key: dupb2018"}}`),
		json.RawMessage(`{"key":"T1","version":1,"data":{"key":"T1","itemType":"journalArticle","title":"Tag One","tags":[{"tag":"AI"}]}}`),
		json.RawMessage(`{"key":"T2","version":1,"data":{"key":"T2","itemType":"journalArticle","title":"Tag Two","tags":[{"tag":"ai"}]}}`),
	}
}

func baselineNewIncompleteItem() json.RawMessage {
	return json.RawMessage(`{"key":"NEW1","version":1,"data":{"key":"NEW1","itemType":"journalArticle","title":"New Incomplete"}}`)
}

func baselineCompleteP1Item() json.RawMessage {
	return json.RawMessage(`{"key":"P1","version":2,"data":{"key":"P1","itemType":"journalArticle","title":"P1 Complete","creators":[{"lastName":"Fixed"}],"date":"2024","publicationTitle":"Journal Fixed","DOI":"10/p1","abstractNote":"abs","tags":[{"tag":"fixed"}],"extra":"Citation Key: fixed2024"}}`)
}

func expectedHealthBaselineIdentities(t *testing.T, preset string) []string {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotio"))
	if err != nil {
		t.Fatalf("open store for expected identities: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Fatalf("close expected identity store: %v", closeErr)
		}
	}()
	report, err := assembleHealthReport(localQueryStore{db}, newHealthCtx(preset, false), preset, healthPresets[preset], "", scopeResult{All: true, Expr: "library"})
	if err != nil {
		t.Fatalf("assemble expected report: %v", err)
	}
	return healthFindingIdentities(healthCurrentFindings(report))
}

func readHealthBaselineReportFile(t *testing.T, path string) healthReport {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report %s: %v", path, err)
	}
	var report healthReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode report %s: %v\n%s", path, err, string(data))
	}
	return report
}

func decodeHealthBaselineBadge(t *testing.T, stdout string) healthBadge {
	t.Helper()
	var badge healthBadge
	if err := json.Unmarshal(bytes.TrimSpace([]byte(stdout)), &badge); err != nil {
		t.Fatalf("decode badge %q: %v", stdout, err)
	}
	return badge
}

func assertHealthBaselineFileSchema(t *testing.T, path string, wantPreset string, wantIdentities []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read baseline %s: %v", path, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("decode baseline object: %v\n%s", err, string(data))
	}
	for _, key := range []string{"schema_version", "generated_at", "preset", "identities"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("baseline keys = %v, missing %q", raw, key)
		}
	}
	if len(raw) != 4 {
		t.Fatalf("baseline schema keys = %v, want exactly schema_version/generated_at/preset/identities", raw)
	}
	var baseline healthBaselineFile
	if err := json.Unmarshal(data, &baseline); err != nil {
		t.Fatalf("decode baseline schema: %v", err)
	}
	if baseline.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", baseline.SchemaVersion)
	}
	if _, err := time.Parse(time.RFC3339, baseline.GeneratedAt); err != nil {
		t.Fatalf("generated_at = %q, want RFC3339: %v", baseline.GeneratedAt, err)
	}
	if baseline.Preset != wantPreset {
		t.Fatalf("preset = %q, want %q", baseline.Preset, wantPreset)
	}
	if !slices.IsSorted(baseline.Identities) {
		t.Fatalf("identities are not sorted: %+v", baseline.Identities)
	}
	if !slices.Equal(baseline.Identities, wantIdentities) {
		t.Fatalf("identities = %+v, want %+v", baseline.Identities, wantIdentities)
	}
}

func assertNoHealthBaselineTempFiles(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read baseline dir: %v", err)
	}
	prefix := "." + filepath.Base(path) + ".tmp-"
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			t.Fatalf("atomic baseline write left temp file %q", entry.Name())
		}
	}
}

func assertNewBaselineFindingForKey(t *testing.T, report healthReport, key string) {
	t.Helper()
	if report.Baseline == nil || !report.Baseline.Established {
		t.Fatalf("report.baseline = %+v, want established baseline", report.Baseline)
	}
	for _, finding := range report.Baseline.New {
		if finding.ItemKey == key {
			return
		}
	}
	t.Fatalf("baseline.new = %+v, want a finding for item %s", report.Baseline.New, key)
}
