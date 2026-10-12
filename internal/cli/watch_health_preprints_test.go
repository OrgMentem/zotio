// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func watchPreprintItem(key, arxivID string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"key":%q,"version":1,"data":{"key":%q,"itemType":"preprint","title":"Preprint %s","url":"https://arxiv.org/abs/%s","DOI":"","extra":""}}`, key, key, key, arxivID))
}

// fakePreprintProviders stands in for arXiv and CrossRef. published maps an
// arXiv ID to the external DOI arXiv reports for it; failing makes both hosts
// answer HTTP 500. Hits are counted per arXiv ID.
type fakePreprintProviders struct {
	mu           sync.Mutex
	published    map[string]string
	failing      bool
	arxivHits    map[string]int
	crossrefHits int
	next         http.RoundTripper
}

func newFakePreprintProviders(t *testing.T) *fakePreprintProviders {
	t.Helper()
	f := &fakePreprintProviders{
		published: map[string]string{},
		arxivHits: map[string]int{},
		next:      http.DefaultTransport,
	}
	withDefaultTransport(t, f)
	return f
}

func (f *fakePreprintProviders) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Hostname() {
	case "export.arxiv.org":
		id := req.URL.Query().Get("id_list")
		f.mu.Lock()
		f.arxivHits[id]++
		failing, doi := f.failing, f.published[id]
		f.mu.Unlock()
		switch {
		case failing:
			return testHTTPResponse(http.StatusInternalServerError, "arXiv down"), nil
		case doi == "":
			return testHTTPResponse(http.StatusOK, arxivAtomFeedXML(`<id>http://arxiv.org/abs/`+id+`</id>`)), nil
		default:
			return testHTTPResponse(http.StatusOK, arxivAtomFeedXML(`<arxiv:doi>`+doi+`</arxiv:doi>`)), nil
		}
	case "api.crossref.org":
		doi, _ := url.PathUnescape(strings.TrimPrefix(req.URL.EscapedPath(), "/works/"))
		f.mu.Lock()
		f.crossrefHits++
		failing := f.failing
		f.mu.Unlock()
		if failing {
			return testHTTPResponse(http.StatusInternalServerError, "CrossRef down"), nil
		}
		return testHTTPResponse(http.StatusOK, crossrefWorkJSON(doi, "Journal of Tests", 2026)), nil
	}
	return f.next.RoundTrip(req)
}

func (f *fakePreprintProviders) publish(arxivID, doi string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published[arxivID] = doi
}

func (f *fakePreprintProviders) setFailing(failing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing = failing
}

func (f *fakePreprintProviders) hits(arxivID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.arxivHits[arxivID]
}

func (f *fakePreprintProviders) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.crossrefHits
	for _, hits := range f.arxivHits {
		n += hits
	}
	return n
}

// seedWatchPreprintEnv seeds the mirror and points the recheck cache at a
// fresh state directory, which it returns.
func seedWatchPreprintEnv(t *testing.T, items []json.RawMessage) string {
	t.Helper()
	seedWatchHealthDefaultStore(t, items)
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("ZOTERO_STATE_DIR", stateDir)
	return stateDir
}

// newWatchPreprintMonitor builds a monitor with the preprint check on, no
// other health kinds, the default recheck horizon, and an injected clock.
func newWatchPreprintMonitor(t *testing.T, clock *time.Time, webhook string) *watchHealthMonitor {
	t.Helper()
	m := &watchHealthMonitor{
		enabled:  true,
		preset:   "citation",
		webhook:  webhook,
		flags:    &rootFlags{timeout: 5 * time.Second},
		previous: map[string]Finding{},
		acked:    map[string]Finding{},
	}
	if err := m.enablePreprintCheck(true, watchPreprintDefaultRecheck); err != nil {
		t.Fatalf("enablePreprintCheck: %v", err)
	}
	m.preprints.now = func() time.Time { return *clock }
	m.preprints.pause = 0
	return m
}

// watchPreprintHook records every health webhook payload.
func watchPreprintHook(t *testing.T) (string, func() []watchHealthWebhookPayload) {
	t.Helper()
	oldAllowPrivateOutbound := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllowPrivateOutbound) })
	var mu sync.Mutex
	var payloads []watchHealthWebhookPayload
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload watchHealthWebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decoding webhook: %v", err)
		}
		mu.Lock()
		payloads = append(payloads, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)
	return hook.URL, func() []watchHealthWebhookPayload {
		mu.Lock()
		defer mu.Unlock()
		return append([]watchHealthWebhookPayload(nil), payloads...)
	}
}

func watchPreprintCmd() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	return cmd, &out, &errOut
}

func watchPreprintFindings(report healthReport) []Finding {
	var out []Finding
	for _, f := range report.Findings {
		if f.Kind == "preprint_published" {
			out = append(out, f)
		}
	}
	return out
}

func watchPreprintSkipOf(report healthReport) *healthSkip {
	for i := range report.Skipped {
		if report.Skipped[i].Kind == "preprint_published" {
			return &report.Skipped[i]
		}
	}
	return nil
}

func TestWatchHealthPreprintPublishedDeliversExactlyOneWebhookFinding(t *testing.T) {
	seedWatchPreprintEnv(t, []json.RawMessage{watchPreprintItem("PRE1", "2401.00001")})
	providers := newFakePreprintProviders(t)
	hookURL, payloads := watchPreprintHook(t)
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	monitor := newWatchPreprintMonitor(t, &clock, hookURL)
	cmd, out, errOut := watchPreprintCmd()

	// Cycle 1: still a preprint, so the baseline is empty.
	monitor.run(context.Background(), cmd, clock)
	if got := out.String(); !strings.Contains(got, "[health] baseline citation total=0") {
		t.Fatalf("cycle 1 stdout = %q, want an empty baseline", got)
	}
	if providers.hits("2401.00001") != 1 {
		t.Fatalf("cycle 1 arXiv lookups = %d, want 1", providers.hits("2401.00001"))
	}

	// The preprint is published; no Zotero item changes. The recheck
	// horizon has passed, so cycle 2 asks again.
	providers.publish("2401.00001", "10.2000/published")
	clock = clock.Add(watchPreprintDefaultRecheck + time.Hour)
	out.Reset()
	errOut.Reset()
	monitor.run(context.Background(), cmd, clock)
	if got := out.String(); !strings.Contains(got, `[health] new info preprint_published PRE1 "Preprint PRE1"`) || !strings.Contains(got, "[health] resolved_count 0") {
		t.Fatalf("cycle 2 stdout = %q, want one new preprint_published line", got)
	}
	if errOut.Len() != 0 {
		t.Fatalf("cycle 2 stderr = %q, want none", errOut.String())
	}

	// Cycle 3: no repeat of the alert, and no new provider traffic for a
	// published preprint.
	lookupsBefore := providers.total()
	clock = clock.Add(5 * time.Minute)
	out.Reset()
	monitor.run(context.Background(), cmd, clock)
	if got := out.String(); strings.Contains(got, "[health] new") || !strings.Contains(got, "[health] resolved_count 0") {
		t.Fatalf("cycle 3 stdout = %q, want no new finding", got)
	}
	if providers.total() != lookupsBefore {
		t.Fatalf("cycle 3 made %d provider call(s), want none for a published preprint", providers.total()-lookupsBefore)
	}

	got := payloads()
	if len(got) != 3 {
		t.Fatalf("webhook payloads = %d, want one per cycle", len(got))
	}
	if len(got[0].New) != 0 || len(got[2].New) != 0 {
		t.Fatalf("cycle 1/3 webhook new = %+v / %+v, want none", got[0].New, got[2].New)
	}
	if len(got[1].New) != 1 {
		t.Fatalf("cycle 2 webhook new = %+v, want exactly one finding", got[1].New)
	}
	f := got[1].New[0]
	if f.Kind != "preprint_published" || f.ItemKey != "PRE1" || f.Evidence["doi"] != "10.2000/published" || f.Evidence["arxiv_id"] != "2401.00001" {
		t.Fatalf("webhook finding = %+v, want preprint_published PRE1 with the DOI and arXiv ID", f)
	}
	if f.RecommendedAction == nil || f.RecommendedAction.Command != "zotio items preprint-check fix" {
		t.Fatalf("webhook finding action = %+v, want the preprint-check fix command", f.RecommendedAction)
	}
	if got[1].Totals.Info != 1 || got[1].Totals.Total != 1 {
		t.Fatalf("cycle 2 totals = %+v, want one info finding", got[1].Totals)
	}
}

func TestWatchHealthPreprintDisabledMakesNoProviderCalls(t *testing.T) {
	seedWatchPreprintEnv(t, []json.RawMessage{watchPreprintItem("PRE1", "2401.00001")})
	providers := newFakePreprintProviders(t)
	providers.publish("2401.00001", "10.2000/published")
	monitor := &watchHealthMonitor{enabled: true, preset: "citation", flags: &rootFlags{timeout: time.Second}, previous: map[string]Finding{}, acked: map[string]Finding{}}

	report, err := monitor.report(context.Background())
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if providers.total() != 0 {
		t.Fatalf("provider calls = %d, want none with the monitor off", providers.total())
	}
	for _, check := range report.Checks {
		if check.Kind == "preprint_published" {
			t.Fatalf("checks = %+v, want no preprint_published check when off", report.Checks)
		}
	}
	if len(watchPreprintFindings(report)) != 0 || watchPreprintSkipOf(report) != nil {
		t.Fatalf("report = %+v, want no preprint finding or skip when off", report)
	}

	cmd := newWatchCmd(&rootFlags{})
	if flag := cmd.Flags().Lookup("health-check-preprints"); flag == nil || flag.DefValue != "false" {
		t.Fatalf("--health-check-preprints = %+v, want an off-by-default flag", flag)
	}
	if flag := cmd.Flags().Lookup("health-preprint-recheck"); flag == nil || flag.DefValue != "" {
		t.Fatalf("--health-preprint-recheck = %+v, want an unset default meaning the 7d horizon", flag)
	}
	for _, raw := range []string{"", "7d"} {
		if d, err := parseWatchPreprintRecheck(raw); err != nil || d != watchPreprintDefaultRecheck {
			t.Fatalf("parse %q = %v, %v; want the 7d default horizon", raw, d, err)
		}
	}
}

func TestWatchHealthPreprintCachedNegativeSuppressesRecheckInsideHorizon(t *testing.T) {
	seedWatchPreprintEnv(t, []json.RawMessage{watchPreprintItem("PRE1", "2401.00001")})
	providers := newFakePreprintProviders(t)
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	monitor := newWatchPreprintMonitor(t, &clock, "")

	report, err := monitor.report(context.Background())
	if err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if len(watchPreprintFindings(report)) != 0 || providers.hits("2401.00001") != 1 {
		t.Fatalf("cycle 1 findings = %+v, lookups = %d; want none and one lookup", report.Findings, providers.hits("2401.00001"))
	}

	// Published upstream, but the cached negative is still inside the horizon.
	providers.publish("2401.00001", "10.2000/published")
	clock = clock.Add(watchPreprintDefaultRecheck - time.Minute)
	report, err = monitor.report(context.Background())
	if err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if providers.total() != 1 {
		t.Fatalf("cycle 2 lookups = %d, want the cached negative to suppress the recheck", providers.hits("2401.00001"))
	}
	if len(watchPreprintFindings(report)) != 0 || watchPreprintSkipOf(report) != nil {
		t.Fatalf("cycle 2 report = %+v, want no finding and no skip from a cache hit", report)
	}

	// Past the horizon the answer is asked again and the publication shows.
	clock = clock.Add(2 * time.Minute)
	report, err = monitor.report(context.Background())
	if err != nil {
		t.Fatalf("cycle 3: %v", err)
	}
	if providers.hits("2401.00001") != 2 || len(watchPreprintFindings(report)) != 1 {
		t.Fatalf("cycle 3 lookups = %d, findings = %+v; want a recheck and one finding", providers.hits("2401.00001"), report.Findings)
	}
}

func TestWatchHealthPreprintCachePersistsAcrossRestarts(t *testing.T) {
	stateDir := seedWatchPreprintEnv(t, []json.RawMessage{
		watchPreprintItem("PUB", "2401.00001"),
		watchPreprintItem("DRAFT", "2401.00002"),
	})
	providers := newFakePreprintProviders(t)
	providers.publish("2401.00001", "10.2000/published")
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	first := newWatchPreprintMonitor(t, &clock, "")
	if _, err := first.report(context.Background()); err != nil {
		t.Fatalf("first process: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, watchPreprintCacheName))
	if err != nil {
		t.Fatalf("recheck cache file: %v", err)
	}
	var file watchPreprintCacheFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("decode cache %q: %v", data, err)
	}
	if got := file.Entries["2401.00001"]; !got.Published || got.DOI != "10.2000/published" || got.Venue != "Journal of Tests" || got.Year != 2026 {
		t.Fatalf("cached positive = %+v, want the published DOI, venue, and year", got)
	}
	if got, ok := file.Entries["2401.00002"]; !ok || got.Published {
		t.Fatalf("cached negative = %+v (present %v), want an unpublished entry", got, ok)
	}
	lookups := providers.total()

	// A restart inside the horizon asks nobody, yet the published preprint
	// is still a finding, so a baseline taken after a restart is complete.
	clock = clock.Add(24 * time.Hour)
	second := newWatchPreprintMonitor(t, &clock, "")
	report, err := second.report(context.Background())
	if err != nil {
		t.Fatalf("second process: %v", err)
	}
	if providers.total() != lookups {
		t.Fatalf("restart made %d provider call(s), want none inside the horizon", providers.total()-lookups)
	}
	if findings := watchPreprintFindings(report); len(findings) != 1 || findings[0].ItemKey != "PUB" {
		t.Fatalf("restart findings = %+v, want the persisted published preprint PUB", findings)
	}

	// A positive never expires: long after the horizon only the negative
	// is asked again.
	clock = clock.Add(90 * 24 * time.Hour)
	third := newWatchPreprintMonitor(t, &clock, "")
	if _, err := third.report(context.Background()); err != nil {
		t.Fatalf("third process: %v", err)
	}
	if providers.hits("2401.00001") != 1 || providers.hits("2401.00002") != 2 {
		t.Fatalf("lookups PUB=%d DRAFT=%d, want the published one kept and only the preprint rechecked", providers.hits("2401.00001"), providers.hits("2401.00002"))
	}
}

func TestWatchHealthPreprintProviderFailureKeepsBaselineAndReportsSkip(t *testing.T) {
	seedWatchPreprintEnv(t, []json.RawMessage{
		watchPreprintItem("PUB", "2401.00001"),
		watchPreprintItem("DRAFT", "2401.00002"),
	})
	providers := newFakePreprintProviders(t)
	providers.publish("2401.00001", "10.2000/published")
	hookURL, payloads := watchPreprintHook(t)
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	monitor := newWatchPreprintMonitor(t, &clock, hookURL)
	cmd, out, errOut := watchPreprintCmd()
	pubKey := watchHealthFindingKey(Finding{Kind: "preprint_published", ItemKey: "PUB"})

	monitor.run(context.Background(), cmd, clock)
	if _, ok := monitor.previous[pubKey]; !ok {
		t.Fatalf("cycle 1 findings = %v, want PUB in the baseline", monitor.previous)
	}

	// The horizon passes and both providers go down. The expired negative
	// cannot be rechecked; the published finding must survive, uncounted
	// as resolved, and the failure must be loud.
	providers.setFailing(true)
	clock = clock.Add(watchPreprintDefaultRecheck + time.Hour)
	out.Reset()
	errOut.Reset()
	monitor.run(context.Background(), cmd, clock)
	if _, ok := monitor.previous[pubKey]; !ok {
		t.Fatalf("cycle 2 findings = %v, want PUB kept through the provider failure", monitor.previous)
	}
	if got := out.String(); !strings.Contains(got, "[health] resolved_count 0") || strings.Contains(got, "[health] new") {
		t.Fatalf("cycle 2 stdout = %q, want resolved_count 0 and no new finding", got)
	}
	wantSkip := "[health] 2026-10-08T13:00:00Z skipped preprint_published (external_arxiv_crossref): "
	if got := errOut.String(); !strings.Contains(got, wantSkip) || !strings.Contains(got, "HTTP 500") || !strings.Contains(got, "2401.00002") {
		t.Fatalf("cycle 2 stderr = %q, want the skip naming the HTTP 500 and the arXiv ID", got)
	}
	if providers.hits("2401.00001") != 1 {
		t.Fatalf("published preprint looked up %d times, want 1: a positive is never asked again", providers.hits("2401.00001"))
	}

	// Recovery: the failed lookup was not cached, so it is retried at once,
	// the publication is announced once, and the skip clears.
	providers.setFailing(false)
	providers.publish("2401.00002", "10.2000/draft-now-published")
	clock = clock.Add(5 * time.Minute)
	out.Reset()
	errOut.Reset()
	monitor.run(context.Background(), cmd, clock)
	if got := out.String(); !strings.Contains(got, `[health] new info preprint_published DRAFT "Preprint DRAFT"`) {
		t.Fatalf("cycle 3 stdout = %q, want DRAFT announced as new", got)
	}
	if got := errOut.String(); got != "[health] 2026-10-08T13:05:00Z skip cleared preprint_published\n" {
		t.Fatalf("cycle 3 stderr = %q, want only the skip cleared line", got)
	}
	if providers.hits("2401.00002") != 3 {
		t.Fatalf("DRAFT lookups = %d, want 3 (first, failed, retried)", providers.hits("2401.00002"))
	}

	got := payloads()
	if len(got) != 3 {
		t.Fatalf("webhook payloads = %d, want 3", len(got))
	}
	if len(got[1].New) != 0 || got[1].ResolvedCount != 0 {
		t.Fatalf("failure-cycle webhook = %+v, want the baseline intact: no new, no resolved", got[1])
	}
	if len(got[2].New) != 1 || got[2].New[0].ItemKey != "DRAFT" {
		t.Fatalf("recovery webhook new = %+v, want exactly DRAFT", got[2].New)
	}
}

func TestWatchHealthPreprintScopeLimitsLookupsToTheCohort(t *testing.T) {
	seedWatchPreprintEnv(t, []json.RawMessage{
		json.RawMessage(`{"key":"IN1","version":1,"data":{"key":"IN1","itemType":"preprint","title":"In","url":"https://arxiv.org/abs/2401.00001","collections":["C1"]}}`),
		json.RawMessage(`{"key":"OUT1","version":1,"data":{"key":"OUT1","itemType":"preprint","title":"Out","url":"https://arxiv.org/abs/2401.00002","collections":["C2"]}}`),
	})
	providers := newFakePreprintProviders(t)
	providers.publish("2401.00001", "10.2000/in")
	providers.publish("2401.00002", "10.2000/out")
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	monitor := newWatchPreprintMonitor(t, &clock, "")
	if err := monitor.setScope("collection:C1"); err != nil {
		t.Fatalf("setScope: %v", err)
	}

	report, err := monitor.report(context.Background())
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if providers.hits("2401.00002") != 0 || providers.hits("2401.00001") != 1 {
		t.Fatalf("lookups IN=%d OUT=%d, want only the cohort asked", providers.hits("2401.00001"), providers.hits("2401.00002"))
	}
	if findings := watchPreprintFindings(report); len(findings) != 1 || findings[0].ItemKey != "IN1" {
		t.Fatalf("findings = %+v, want only the in-scope IN1", findings)
	}
}

func TestWatchHealthPreprintSkipsChildrenAndAlreadyFixedItems(t *testing.T) {
	seedWatchPreprintEnv(t, []json.RawMessage{
		watchPreprintItem("PARENT", "2401.00001"),
		// A connector-saved PDF names arXiv in its title and URL; it is a
		// child, and an alert on its key would not be actionable.
		json.RawMessage(`{"key":"CHILD","version":1,"data":{"key":"CHILD","itemType":"attachment","parentItem":"PARENT","title":"arXiv Fulltext PDF","url":"https://arxiv.org/pdf/2401.00009.pdf"}}`),
		json.RawMessage(`{"key":"FIXEDDOI","version":1,"data":{"key":"FIXEDDOI","itemType":"preprint","title":"Fixed by DOI","url":"https://arxiv.org/abs/2401.00003","DOI":"https://doi.org/10.2000/FIXED"}}`),
		json.RawMessage(`{"key":"FIXEDEXTRA","version":1,"data":{"key":"FIXEDEXTRA","itemType":"preprint","title":"Fixed by Extra","url":"https://arxiv.org/abs/2401.00004","extra":"zotio preprint-check: published as doi:10.2000/fixed2 (Journal, 2026) on 2026-10-01"}}`),
	})
	providers := newFakePreprintProviders(t)
	for id, doi := range map[string]string{
		"2401.00001": "10.2000/parent",
		"2401.00009": "10.2000/child",
		"2401.00003": "10.2000/fixed",
		"2401.00004": "10.2000/fixed2",
	} {
		providers.publish(id, doi)
	}
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	monitor := newWatchPreprintMonitor(t, &clock, "")

	report, err := monitor.report(context.Background())
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if providers.hits("2401.00009") != 0 {
		t.Fatalf("the child attachment was looked up %d time(s), want none", providers.hits("2401.00009"))
	}
	if findings := watchPreprintFindings(report); len(findings) != 1 || findings[0].ItemKey != "PARENT" {
		t.Fatalf("findings = %+v, want only PARENT: no child, no item that already records the published DOI", findings)
	}
}

func TestWatchHealthPreprintLookupBudgetSpreadsDueWorkAcrossCycles(t *testing.T) {
	var items []json.RawMessage
	total := watchPreprintMaxLookups + 10
	for i := 1; i <= total; i++ {
		items = append(items, watchPreprintItem(fmt.Sprintf("K%03d", i), fmt.Sprintf("2402.%05d", i)))
	}
	seedWatchPreprintEnv(t, items)
	providers := newFakePreprintProviders(t)
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	monitor := newWatchPreprintMonitor(t, &clock, "")

	report, err := monitor.report(context.Background())
	if err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if got := len(providers.arxivHits); got != watchPreprintMaxLookups {
		t.Fatalf("cycle 1 looked up %d preprints, want the per-cycle limit %d", got, watchPreprintMaxLookups)
	}
	skip := watchPreprintSkipOf(report)
	if skip == nil || skip.Precondition != "preprint_check_budget" || !strings.Contains(skip.Detail, "10 due preprint(s) were deferred") {
		t.Fatalf("cycle 1 skip = %+v, want a loud budget skip naming the 10 deferred preprints", skip)
	}

	report, err = monitor.report(context.Background())
	if err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if len(providers.arxivHits) != total {
		t.Fatalf("after cycle 2 %d preprints were looked up, want all %d", len(providers.arxivHits), total)
	}
	for id, hits := range providers.arxivHits {
		if hits != 1 {
			t.Fatalf("%s looked up %d times, want once: cycle 2 must take only the deferred ones", id, hits)
		}
	}
	if skip := watchPreprintSkipOf(report); skip != nil {
		t.Fatalf("cycle 2 skip = %+v, want none once everything is answered", skip)
	}
}

func TestWatchHealthPreprintOutageStopsAfterConsecutiveFailuresAndRotates(t *testing.T) {
	var items []json.RawMessage
	for i := 1; i <= 10; i++ {
		items = append(items, watchPreprintItem(fmt.Sprintf("K%02d", i), fmt.Sprintf("2403.%05d", i)))
	}
	seedWatchPreprintEnv(t, items)
	providers := newFakePreprintProviders(t)
	providers.setFailing(true)
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	monitor := newWatchPreprintMonitor(t, &clock, "")

	report, err := monitor.report(context.Background())
	if err != nil {
		t.Fatalf("a provider outage must be a skip, not a failed cycle: %v", err)
	}
	if providers.total() != watchPreprintMaxConsecutiveFailures {
		t.Fatalf("provider calls during an outage = %d, want to stop after %d", providers.total(), watchPreprintMaxConsecutiveFailures)
	}
	skip := watchPreprintSkipOf(report)
	if skip == nil || skip.Precondition != "external_arxiv_crossref" || !strings.Contains(skip.Detail, "lookups stopped after 3 consecutive failures; 7 due preprint(s) were not tried") {
		t.Fatalf("skip = %+v, want an outage skip naming the 7 untried preprints", skip)
	}
	if len(watchPreprintFindings(report)) != 0 {
		t.Fatalf("findings = %+v, want none from failed lookups", report.Findings)
	}
	if got := report.Summary.Total; got != 0 {
		t.Fatalf("summary total = %d, want 0", got)
	}

	// The next cycle starts with the preprints not yet tried, so a few bad
	// identifiers cannot starve the rest.
	if _, err := monitor.report(context.Background()); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if len(providers.arxivHits) != 2*watchPreprintMaxConsecutiveFailures {
		t.Fatalf("distinct preprints tried after two cycles = %d, want %d", len(providers.arxivHits), 2*watchPreprintMaxConsecutiveFailures)
	}
	for id, hits := range providers.arxivHits {
		if hits != 1 {
			t.Fatalf("%s tried %d times, want each once while untried ones remain", id, hits)
		}
	}
}

func TestWatchHealthPreprintUnusableCacheStartsEmptyAndSaysSo(t *testing.T) {
	stateDir := seedWatchPreprintEnv(t, []json.RawMessage{watchPreprintItem("PRE1", "2401.00001")})
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	path := filepath.Join(stateDir, watchPreprintCacheName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt cache: %v", err)
	}
	providers := newFakePreprintProviders(t)
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	monitor := newWatchPreprintMonitor(t, &clock, "")

	report, err := monitor.report(context.Background())
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	skip := watchPreprintSkipOf(report)
	if skip == nil || skip.Precondition != "preprint_recheck_cache" || !strings.Contains(skip.Detail, "ignored the unusable recheck cache") {
		t.Fatalf("skip = %+v, want the corrupt cache reported", skip)
	}
	if providers.hits("2401.00001") != 1 {
		t.Fatalf("lookups = %d, want the preprint looked up from the empty cache", providers.hits("2401.00001"))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read repaired cache: %v", err)
	}
	var file watchPreprintCacheFile
	if err := json.Unmarshal(data, &file); err != nil || file.SchemaVersion != watchPreprintCacheSchema {
		t.Fatalf("repaired cache %q (%v), want a valid schema_version %d file", data, err, watchPreprintCacheSchema)
	}

	clock = clock.Add(time.Minute)
	report, err = monitor.report(context.Background())
	if err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if skip := watchPreprintSkipOf(report); skip != nil {
		t.Fatalf("cycle 2 skip = %+v, want the cache note reported once", skip)
	}
}

func TestWatchHealthPreprintUnwritableCacheStillFindsAndSaysSo(t *testing.T) {
	seedWatchHealthDefaultStore(t, []json.RawMessage{watchPreprintItem("PRE1", "2401.00001")})
	// A state directory nested under a regular file can be neither read nor
	// created.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("file"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	t.Setenv("ZOTERO_STATE_DIR", filepath.Join(blocker, "state"))
	providers := newFakePreprintProviders(t)
	providers.publish("2401.00001", "10.2000/published")
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	monitor := newWatchPreprintMonitor(t, &clock, "")

	report, err := monitor.report(context.Background())
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if findings := watchPreprintFindings(report); len(findings) != 1 {
		t.Fatalf("findings = %+v, want the publication reported despite the cache failure", findings)
	}
	skip := watchPreprintSkipOf(report)
	if skip == nil || !strings.Contains(skip.Detail, "the recheck cache was not saved") {
		t.Fatalf("skip = %+v, want the failed cache save reported", skip)
	}
}

func TestParseWatchPreprintRecheck(t *testing.T) {
	good := map[string]time.Duration{
		"7d":   7 * 24 * time.Hour,
		"2w":   14 * 24 * time.Hour,
		"168h": 168 * time.Hour,
		"1h":   time.Hour,
		"90m":  90 * time.Minute,
		" 3d ": 72 * time.Hour,
	}
	for raw, want := range good {
		got, err := parseWatchPreprintRecheck(raw)
		if err != nil || got != want {
			t.Errorf("parse %q = %v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{"d", "0d", "-1d", "1.5d", "1d12h", "abc", "30m", "59m", "-2h", "99999999999d"} {
		_, err := parseWatchPreprintRecheck(raw)
		if err == nil {
			t.Errorf("parse %q succeeded, want a usage error", raw)
			continue
		}
		if ExitCode(err) != 2 || !strings.Contains(err.Error(), "--health-preprint-recheck") {
			t.Errorf("parse %q error = %v (exit %d), want a usage error naming the flag", raw, err, ExitCode(err))
		}
	}
}

func TestWatchHealthCheckPreprintsFlagValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"needs health", []string{"--once", "--health-check-preprints"}, "--health-check-preprints requires --health"},
		{"recheck needs the check", []string{"--once", "--health", "--health-preprint-recheck", "3d"}, "--health-preprint-recheck requires --health-check-preprints"},
		{"recheck floor", []string{"--once", "--health", "--health-check-preprints", "--health-preprint-recheck", "5m"}, "must be at least 1h"},
		{"recheck syntax", []string{"--once", "--health", "--health-check-preprints", "--health-preprint-recheck", "soon"}, "invalid --health-preprint-recheck"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newWatchCmd(&rootFlags{})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetArgs(tc.args)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) || ExitCode(err) != 2 {
				t.Fatalf("err = %v (exit %d), want usage error containing %q", err, ExitCode(err), tc.want)
			}
		})
	}
}

// The candidate selection treats a limit of zero as "no cap", so the monitor
// sees every arXiv preprint of a large library rather than the first hundred
// per source.
func TestFetchArxivPreprintCandidatesZeroLimitIsUnbounded(t *testing.T) {
	var page []map[string]any
	for i := 1; i <= 150; i++ {
		page = append(page, map[string]any{"key": fmt.Sprintf("K%03d", i), "url": fmt.Sprintf("https://arxiv.org/abs/2404.%05d", i)})
	}
	stub := &preprintDetectStubGetter{t: t, pages: map[string][]map[string]any{"preprint": page, "arxiv": {}}}

	all, err := fetchArxivPreprintCandidates(stub, 0)
	if err != nil {
		t.Fatalf("fetchArxivPreprintCandidates(0): %v", err)
	}
	if len(all) != 150 {
		t.Fatalf("limit 0 returned %d candidates, want all 150", len(all))
	}
	capped, err := fetchArxivPreprintCandidates(stub, 20)
	if err != nil {
		t.Fatalf("fetchArxivPreprintCandidates(20): %v", err)
	}
	if len(capped) != 20 {
		t.Fatalf("limit 20 returned %d candidates, want 20", len(capped))
	}
}
