// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		src:         FindingSource{Kind: "local"},
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

	// The live opt-in checks (broken attachments, retraction probe) must be
	// loudly skipped, not silently dropped.
	skips := map[string]healthSkip{}
	for _, s := range report.Skipped {
		skips[s.Kind] = s
	}
	if len(report.Skipped) != 2 {
		t.Fatalf("expected broken_attachment_file + retracted_item skips, got %+v", report.Skipped)
	}
	if s, ok := skips["broken_attachment_file"]; !ok || s.Precondition != "live_local_api" {
		t.Errorf("broken_attachment_file skip = %+v, want live_local_api precondition", s)
	}
	if s, ok := skips["retracted_item"]; !ok || s.Precondition == "" {
		t.Errorf("retracted_item skip = %+v, want a named precondition", s)
	}
	for kind, s := range skips {
		if len(s.Remediation) == 0 {
			t.Errorf("%s skip must carry remediation steps", kind)
		}
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

func TestRetractedItemSkipsLoudlyWithoutCheckRetractions(t *testing.T) {
	findings, skip, err := runRetractedItem(localQueryStore{}, &healthContext{preset: "all", flags: &rootFlags{}})
	if err != nil {
		t.Fatalf("runRetractedItem: %v", err)
	}
	if findings != nil {
		t.Fatalf("findings = %+v, want none when live retraction check is disabled", findings)
	}
	if skip == nil || skip.Kind != "retracted_item" || skip.Precondition != "external_crossref" {
		t.Fatalf("skip = %+v, want external_crossref retracted_item skip", skip)
	}
	if !strings.Contains(skip.Detail, "off by default") {
		t.Fatalf("skip detail = %q, want loud opt-in explanation", skip.Detail)
	}
	var sawCheckRetractions bool
	for _, r := range skip.Remediation {
		if strings.Contains(r.Command, "--check-retractions") {
			sawCheckRetractions = true
		}
	}
	if !sawCheckRetractions {
		t.Fatalf("skip remediation = %+v, want --check-retractions command", skip.Remediation)
	}
}

func TestLibraryHealthCheckRetractionsInjectsIntoQuickPresetAndRunsProbe(t *testing.T) {
	seedRetractionDefaultStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"RET","version":1,"data":{"key":"RET","itemType":"journalArticle","title":"Retracted Work","creators":[{"lastName":"Author"}],"date":"2020","publicationTitle":"Journal","DOI":"10.777/retracted","extra":"Citation Key: author2020"}}`),
	})

	var sawProbe bool
	var sawWork bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/works" && r.URL.Query().Get("rows") == "0":
			sawProbe = true
			_, _ = w.Write([]byte(`{"message":{}}`))
		case r.URL.EscapedPath() == "/works/10.777%2Fretracted":
			sawWork = true
			_, _ = w.Write([]byte(`{"message":{"updated-by":[{"DOI":"10.777/retraction-notice","type":"retraction","label":"Retracted","source":"publisher","updated":{"date-parts":[[2025,1,15]]}}]}}`))
		default:
			http.Error(w, "unexpected CrossRef request", http.StatusNotFound)
			t.Errorf("unexpected CrossRef request path=%q rawQuery=%q", r.URL.Path, r.URL.RawQuery)
		}
	}))
	t.Cleanup(srv.Close)
	withBase(t, &crossrefRetractionBaseURL, srv.URL)

	cmd := newLibraryHealthCmd(&rootFlags{asJSON: true, timeout: time.Second})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"--for", "quick", "--check-retractions"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("library health --check-retractions: %v", err)
	}
	if !sawProbe || !sawWork {
		t.Fatalf("CrossRef calls: probe=%v work=%v, want probe and work lookup", sawProbe, sawWork)
	}

	var report healthReport
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &report); err != nil {
		t.Fatalf("decode health report %q: %v", out.String(), err)
	}
	var retractedCheck healthCheckRun
	for _, check := range report.Checks {
		if check.Kind == "retracted_item" {
			retractedCheck = check
		}
	}
	if !retractedCheck.Ran || retractedCheck.Count != 1 {
		t.Fatalf("retracted_item check = %+v, want injected run with one finding", retractedCheck)
	}
	var sawFinding bool
	for _, finding := range report.Findings {
		if finding.Kind != "retracted_item" {
			continue
		}
		sawFinding = true
		if finding.ItemKey != "RET" || finding.Severity != sevCritical || sqlStringValue(finding.Evidence["status"]) != "retracted" {
			t.Fatalf("retracted finding = %+v, want critical RET status=retracted", finding)
		}
	}
	if !sawFinding {
		t.Fatalf("findings = %+v, want retracted_item finding", report.Findings)
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
		return &healthContext{src: FindingSource{Kind: "local", SyncedAt: syncedAt}, preset: "quick", flags: &rootFlags{}, requireFresh: 24 * time.Hour}
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

func TestHealthBadgeForReportVerdicts(t *testing.T) {
	cases := []struct {
		name   string
		report healthReport
		want   healthBadge
	}{
		{
			name:   "zero findings is healthy",
			report: healthReport{},
			want:   healthBadge{SchemaVersion: 1, Label: "bibliography", Message: "healthy", Color: "brightgreen"},
		},
		{
			name: "failed gate reports nonzero severities most severe first",
			report: healthReport{
				Summary: healthSummary{Critical: 2, High: 1, Info: 3, Total: 6},
				Gate:    &healthGate{Status: "failed"},
			},
			want: healthBadge{SchemaVersion: 1, Label: "bibliography", Message: "2 critical, 1 high, 3 info", Color: "red"},
		},
		{
			name: "indeterminate gate asks for setup",
			report: healthReport{
				Summary: healthSummary{Critical: 1, Total: 1},
				Gate:    &healthGate{Status: "indeterminate"},
			},
			want: healthBadge{SchemaVersion: 1, Label: "bibliography", Message: "setup required", Color: "orange"},
		},
		{
			name: "passing gate with findings stays yellow",
			report: healthReport{
				Summary: healthSummary{High: 2, Total: 2},
				Gate:    &healthGate{Status: "passed"},
			},
			want: healthBadge{SchemaVersion: 1, Label: "bibliography", Message: "2 findings", Color: "yellow"},
		},
		{
			name: "stale freshness takes sync-needed precedence",
			report: healthReport{
				Summary:   healthSummary{Critical: 1, Total: 1},
				Gate:      &healthGate{Status: "failed"},
				Freshness: &healthFreshness{Stale: true},
			},
			want: healthBadge{SchemaVersion: 1, Label: "bibliography", Message: "stale — sync needed", Color: "lightgrey"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := healthBadgeForReport(tc.report, "bibliography"); got != tc.want {
				t.Fatalf("badge = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLibraryHealthBadgeRejectsJSONBeforeStoreAccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	flags := &rootFlags{asJSON: true}
	cmd := newLibraryHealthCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"--badge"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected --badge with --json to fail as a usage error")
	}
	if code := ExitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want no badge when usage is invalid", out.String())
	}
}

func TestLibraryHealthBadgeEmptyStorePrintsNotSyncedAndPrecondition(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newLibraryHealthCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"--badge", "--badge-label", "library"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected empty local store to return a precondition error in badge mode")
	}
	if code := ExitCode(err); code != 9 {
		t.Fatalf("exit code = %d, want 9", code)
	}
	var badge healthBadge
	if decodeErr := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &badge); decodeErr != nil {
		t.Fatalf("decode badge %q: %v", out.String(), decodeErr)
	}
	want := healthBadge{SchemaVersion: 1, Label: "library", Message: "not synced", Color: "lightgrey"}
	if badge != want {
		t.Fatalf("badge = %+v, want %+v", badge, want)
	}
}
