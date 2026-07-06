// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase1): tests for the composite `library health` report —
// check composition, --for presets, the --fail-on quality gate (exit 11), and the
// precondition contract for the live broken-attachment check (loud skip + exit 9).

package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/store"
)

// seedHealthStore builds a store exercising every local check kind:
//   - P1: bare journalArticle (missing citekey, citation core, DOI, abstract, tags, PDF)
//   - P2 + A2: a complete journalArticle with a PDF child (clean control)
//   - C1/C2: share a Better BibTeX citation key (citekey_conflict)
//   - D1/D2: share a DOI (duplicate_candidates)
//   - T1/T2: tag "AI" vs "ai" (tag_drift)
func seedHealthStore(t *testing.T) localQueryStore {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	items := []json.RawMessage{
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
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return localQueryStore{db}
}

func newHealthCtx(preset string, verifyFiles bool) *healthContext {
	return &healthContext{
		src:         healthSource{Kind: "local"},
		preset:      preset,
		verifyFiles: verifyFiles,
		flags:       &rootFlags{},
	}
}

func findingKinds(report healthReport) map[string]int {
	got := map[string]int{}
	for _, f := range report.Findings {
		got[f.Kind]++
	}
	return got
}

func TestLibraryHealthComposesAllChecks(t *testing.T) {
	db := seedHealthStore(t)
	report, err := assembleHealthReport(db, newHealthCtx("all", false), "all", healthPresets["all"], "", scopeResult{All: true, Expr: "library"})
	if err != nil {
		t.Fatalf("assembleHealthReport: %v", err)
	}

	got := findingKinds(report)
	for _, want := range []string{
		"citekey_missing", "citekey_conflict", "missing_citation", "missing_doi",
		"missing_abstract", "missing_tags", "missing_pdf", "duplicate_candidates", "tag_drift",
	} {
		if got[want] == 0 {
			t.Errorf("expected at least one %q finding, got none (%v)", want, got)
		}
	}
	if got["citekey_conflict"] != 2 {
		t.Errorf("citekey_conflict = %d, want 2 (C1+C2 share a key)", got["citekey_conflict"])
	}

	// The live broken-attachment check must be loudly skipped, not silently dropped.
	if len(report.Skipped) != 1 || report.Skipped[0].Kind != "broken_attachment_file" {
		t.Fatalf("expected one broken_attachment_file skip, got %+v", report.Skipped)
	}
	if report.Skipped[0].Precondition != "live_local_api" {
		t.Errorf("skip precondition = %q, want live_local_api", report.Skipped[0].Precondition)
	}
	if len(report.Skipped[0].Remediation) == 0 {
		t.Error("skip must carry remediation steps")
	}

	plan := map[string]healthRemediationPlanStep{}
	for _, step := range report.RemediationPlan {
		plan[step.Kind] = step
		if !step.Preview {
			t.Errorf("remediation step %s is not preview-first: %+v", step.Kind, step)
		}
	}
	if step := plan["missing_doi"]; step.Command != "zotio items enrich --missing-doi --keys-from -" || !step.Scoped || len(step.Keys) == 0 {
		t.Errorf("missing_doi remediation step = %+v, want scoped keys-from command", step)
	}
	if step := plan["missing_abstract"]; step.Command != "zotio items enrich --missing-abstract --keys-from -" || !step.Scoped || len(step.Keys) == 0 {
		t.Errorf("missing_abstract remediation step = %+v, want scoped keys-from command", step)
	}
	if step := plan["missing_pdf"]; step.Command != "zotio items enrich --missing-pdf --keys-from -" || !step.Scoped || len(step.Keys) == 0 {
		t.Errorf("missing_pdf remediation step = %+v, want scoped keys-from command", step)
	}
	if step := plan["duplicate_candidates"]; step.Command == "" || step.Scoped {
		t.Errorf("duplicate remediation step = %+v, want broad delegated preview command", step)
	}
	if step := plan["tag_drift"]; step.Command != "zotio tags audit fix" || step.Scoped {
		t.Errorf("tag remediation step = %+v, want broad tag-audit preview", step)
	}

	// No --fail-on -> no gate, regardless of how many findings exist.
	if report.Gate != nil {
		t.Errorf("gate should be nil without --fail-on, got %+v", report.Gate)
	}
	if err := healthGateExitError(report); err != nil {
		t.Errorf("no gate -> nil exit error, got %v", err)
	}
}

func TestLibraryHealthGateFailsExit11(t *testing.T) {
	db := seedHealthStore(t)
	report, err := assembleHealthReport(db, newHealthCtx("citation", false), "citation", healthPresets["citation"], sevHigh, scopeResult{All: true, Expr: "library"})
	if err != nil {
		t.Fatalf("assembleHealthReport: %v", err)
	}
	if report.Gate == nil || report.Gate.Status != "failed" {
		t.Fatalf("gate = %+v, want status failed", report.Gate)
	}
	exitErr := healthGateExitError(report)
	if exitErr == nil {
		t.Fatal("expected a gate exit error")
	}
	if code := ExitCode(exitErr); code != 11 {
		t.Errorf("gate failure exit code = %d, want 11", code)
	}
}

func TestLibraryHealthGateIndeterminateExit9(t *testing.T) {
	db := seedHealthStore(t)
	// quick includes broken_attachment_file (critical, live). Without --verify-files
	// it is skipped; with --fail-on critical the gate cannot be certified -> exit 9.
	report, err := assembleHealthReport(db, newHealthCtx("quick", false), "quick", healthPresets["quick"], sevCritical, scopeResult{All: true, Expr: "library"})
	if err != nil {
		t.Fatalf("assembleHealthReport: %v", err)
	}
	if report.Gate == nil || report.Gate.Status != "indeterminate" {
		t.Fatalf("gate = %+v, want status indeterminate", report.Gate)
	}
	if code := ExitCode(healthGateExitError(report)); code != 9 {
		t.Errorf("indeterminate gate exit code = %d, want 9", code)
	}
}

func TestLibraryHealthCleanStorePassesGate(t *testing.T) {
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	items := []json.RawMessage{
		json.RawMessage(`{"key":"P2","version":1,"data":{"key":"P2","itemType":"journalArticle","title":"Complete","creators":[{"lastName":"Doe"}],"date":"2020","publicationTitle":"Journal X","DOI":"10/p2","abstractNote":"abs","tags":[{"tag":"x"}],"extra":"Citation Key: doe2020"}}`),
		json.RawMessage(`{"key":"A2","version":1,"data":{"key":"A2","itemType":"attachment","parentItem":"P2","contentType":"application/pdf"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	qs := localQueryStore{db}

	report, err := assembleHealthReport(qs, newHealthCtx("citation", false), "citation", healthPresets["citation"], sevHigh, scopeResult{All: true, Expr: "library"})
	if err != nil {
		t.Fatalf("assembleHealthReport: %v", err)
	}
	if report.Summary.Total != 0 {
		t.Errorf("clean store should have no findings, got %d (%v)", report.Summary.Total, findingKinds(report))
	}
	if report.Gate == nil || report.Gate.Status != "passed" {
		t.Fatalf("gate = %+v, want status passed", report.Gate)
	}
	if err := healthGateExitError(report); err != nil {
		t.Errorf("passed gate -> nil exit error, got %v", err)
	}
}

func TestBrokenAttachmentSkipsLoudlyWithoutVerifyFiles(t *testing.T) {
	findings, skip, err := runBrokenAttachmentFile(localQueryStore{}, newHealthCtx("systematic-review", false))
	if err != nil {
		t.Fatalf("runBrokenAttachmentFile: %v", err)
	}
	if findings != nil {
		t.Errorf("expected no findings when skipped, got %v", findings)
	}
	if skip == nil || skip.Precondition != "live_local_api" {
		t.Fatalf("expected a live_local_api skip, got %+v", skip)
	}
	var sawVerify bool
	for _, r := range skip.Remediation {
		if strings.Contains(r.Command, "--verify-files") {
			sawVerify = true
		}
	}
	if !sawVerify {
		t.Errorf("skip remediation should suggest --verify-files, got %+v", skip.Remediation)
	}
}

func TestGateCrossed(t *testing.T) {
	cases := []struct {
		name    string
		summary healthSummary
		failOn  string
		want    bool
	}{
		{"critical-hit", healthSummary{Critical: 1}, sevCritical, true},
		{"critical-miss-on-high", healthSummary{High: 3}, sevCritical, false},
		{"high-hit-via-critical", healthSummary{Critical: 1}, sevHigh, true},
		{"high-hit-via-high", healthSummary{High: 1}, sevHigh, true},
		{"high-miss-on-info", healthSummary{Info: 9}, sevHigh, false},
		{"any-hit-on-info", healthSummary{Info: 1, Total: 1}, "any", true},
		{"any-miss-empty", healthSummary{}, "any", false},
		{"none-never", healthSummary{Critical: 9, Total: 9}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gateCrossed(tc.summary, tc.failOn); got != tc.want {
				t.Errorf("gateCrossed(%+v, %q) = %v, want %v", tc.summary, tc.failOn, got, tc.want)
			}
		})
	}
}

func TestSelectHealthChecksReturnsRegistryOrder(t *testing.T) {
	checks := selectHealthChecks(healthPresets["citation"])
	got := make([]string, len(checks))
	for i, c := range checks {
		got[i] = c.kind
	}
	// citation kinds, in registry order (conflict before missing).
	want := []string{"citekey_conflict", "citekey_missing", "duplicate_candidates", "missing_citation"}
	if len(got) != len(want) {
		t.Fatalf("selected = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("selected = %v, want %v", got, want)
		}
	}
}

func TestHealthPresetsReferenceRealKinds(t *testing.T) {
	known := map[string]bool{}
	for _, c := range healthCheckRegistry() {
		known[c.kind] = true
	}
	for preset, kinds := range healthPresets {
		for _, k := range kinds {
			if !known[k] {
				t.Errorf("preset %q references unknown check kind %q", preset, k)
			}
		}
	}
	if len(healthPresets["all"]) != len(known) {
		t.Errorf("preset \"all\" lists %d kinds, registry has %d", len(healthPresets["all"]), len(known))
	}
}

func TestLibraryHealthFreshnessGate(t *testing.T) {
	db := seedHealthStore(t)
	freshCtx := func(syncedAt *time.Time) *healthContext {
		return &healthContext{src: healthSource{Kind: "local", SyncedAt: syncedAt}, preset: "quick", flags: &rootFlags{}, requireFresh: 24 * time.Hour}
	}
	all := scopeResult{All: true, Expr: "library"}

	// Stale: synced 48h ago, require 24h -> exit 12.
	old := time.Now().Add(-48 * time.Hour)
	stale, err := assembleHealthReport(db, freshCtx(&old), "quick", healthPresets["quick"], "", all)
	if err != nil {
		t.Fatalf("assembleHealthReport: %v", err)
	}
	if stale.Freshness == nil || !stale.Freshness.Stale {
		t.Fatalf("expected stale freshness, got %+v", stale.Freshness)
	}
	if code := ExitCode(healthFreshnessExitError(stale)); code != 12 {
		t.Errorf("stale exit code = %d, want 12", code)
	}

	// Fresh: synced now -> no freshness error.
	now := time.Now()
	fresh, err := assembleHealthReport(db, freshCtx(&now), "quick", healthPresets["quick"], "", all)
	if err != nil {
		t.Fatalf("assembleHealthReport: %v", err)
	}
	if fresh.Freshness == nil || fresh.Freshness.Stale {
		t.Errorf("expected fresh, got %+v", fresh.Freshness)
	}
	if err := healthFreshnessExitError(fresh); err != nil {
		t.Errorf("fresh -> nil exit error, got %v", err)
	}

	// Never synced -> stale.
	never, err := assembleHealthReport(db, freshCtx(nil), "quick", healthPresets["quick"], "", all)
	if err != nil {
		t.Fatalf("assembleHealthReport: %v", err)
	}
	if never.Freshness == nil || !never.Freshness.Stale {
		t.Errorf("never-synced should be stale, got %+v", never.Freshness)
	}
}
