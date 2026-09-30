// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func watchRetractionItem(key, doi string, version int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"key":%q,"version":%d,"data":{"key":%q,"itemType":"journalArticle","title":"Work %s","creators":[{"lastName":"Author"}],"date":"2020","publicationTitle":"Journal","DOI":%q}}`, key, version, key, key, doi))
}

// fakeWatchCrossref counts per-DOI lookups; DOIs in fail return HTTP 500.
type fakeWatchCrossref struct {
	mu    sync.Mutex
	hits  map[string]int
	fail  map[string]bool
	total int
}

func newFakeWatchCrossref(t *testing.T) *fakeWatchCrossref {
	t.Helper()
	f := &fakeWatchCrossref{hits: map[string]int{}, fail: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doi := strings.TrimPrefix(r.URL.Path, "/works/")
		if doi == r.URL.Path {
			t.Errorf("unexpected CrossRef request path=%q", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		f.mu.Lock()
		f.hits[doi]++
		f.total++
		failing := f.fail[doi]
		f.mu.Unlock()
		if failing {
			http.Error(w, "crossref down", http.StatusInternalServerError)
			return
		}
		if doi == "10.777/ret" {
			_, _ = w.Write([]byte(`{"message":{"updated-by":[{"DOI":"10.777/notice","type":"retraction","label":"Retracted","source":"publisher","updated":{"date-parts":[[2025,1,15]]}}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"message":{}}`))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &crossrefRetractionBaseURL, srv.URL)
	return f
}

func newWatchRetractionMonitor(preset string, kinds []string, on bool) *watchHealthMonitor {
	m := &watchHealthMonitor{enabled: true, preset: preset, kinds: kinds, flags: &rootFlags{timeout: 5 * time.Second}, previous: map[string]Finding{}, acked: map[string]Finding{}}
	m.enableRetractionCheck(on)
	return m
}

func TestWatchHealthCheckRetractionsCachesDOIsAcrossCycles(t *testing.T) {
	seedWatchHealthDefaultStore(t, []json.RawMessage{
		watchRetractionItem("A", "10.777/a", 1),
		watchRetractionItem("B", "10.777/b", 1),
		watchRetractionItem("RET", "10.777/ret", 1),
	})
	crossref := newFakeWatchCrossref(t)
	monitor := newWatchRetractionMonitor("quick", healthPresets["quick"], true)
	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	monitor.run(context.Background(), cmd, time.Now())
	monitor.run(context.Background(), cmd, time.Now())
	if crossref.total != 3 {
		t.Fatalf("CrossRef lookups after two cycles = %d (%v), want 3 (cycle 2 served from cache)", crossref.total, crossref.hits)
	}
	if got := errOut.String(); strings.Contains(got, "retracted_item") || strings.Contains(got, "error") {
		t.Fatalf("stderr = %q, want no retraction skip or error", got)
	}

	upsertWatchHealthDefaultStore(t, []json.RawMessage{watchRetractionItem("D", "10.777/d", 2)})
	report, err := monitor.report(context.Background())
	if err != nil {
		t.Fatalf("cycle 3 report: %v", err)
	}
	if crossref.total != 4 || crossref.hits["10.777/d"] != 1 {
		t.Fatalf("CrossRef lookups after cycle 3 = %d (%v), want one more for the new DOI", crossref.total, crossref.hits)
	}
	var sawRetracted bool
	for _, f := range report.Findings {
		if f.Kind == "retracted_item" && f.ItemKey == "RET" {
			sawRetracted = true
		}
	}
	if !sawRetracted {
		t.Fatalf("findings = %+v, want cached retracted_item for RET", report.Findings)
	}

	// An expired entry is re-requested.
	monitor.retractions.now = func() time.Time { return time.Now().Add(watchRetractionCacheTTL + time.Minute) }
	if _, err := monitor.report(context.Background()); err != nil {
		t.Fatalf("post-TTL report: %v", err)
	}
	if crossref.total != 8 {
		t.Fatalf("CrossRef lookups after TTL = %d, want every DOI re-checked", crossref.total)
	}
}

func watchRetractionItemIn(key, doi, collection string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"key":%q,"version":1,"data":{"key":%q,"itemType":"journalArticle","title":"Work %s","creators":[{"lastName":"Author"}],"date":"2020","publicationTitle":"Journal","DOI":%q,"collections":[%q]}}`, key, key, key, doi, collection))
}

func TestWatchHealthCheckRetractionsScopedCohortSkipsOutOfScopeDOIs(t *testing.T) {
	seedWatchHealthDefaultStore(t, []json.RawMessage{
		watchRetractionItemIn("IN1", "10.777/in1", "C1"),
		watchRetractionItemIn("IN2", "10.777/in2", "C1"),
		watchRetractionItemIn("OUT1", "10.777/out1", "C2"),
		watchRetractionItemIn("OUT2", "10.777/out2", "C2"),
		watchRetractionItemIn("OUT3", "10.777/out3", "C2"),
	})
	crossref := newFakeWatchCrossref(t)
	monitor := newWatchRetractionMonitor("quick", healthPresets["quick"], true)
	if err := monitor.setScope("collection:C1"); err != nil {
		t.Fatalf("setScope: %v", err)
	}
	if _, err := monitor.report(context.Background()); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if crossref.total != 2 || crossref.hits["10.777/in1"] != 1 || crossref.hits["10.777/in2"] != 1 {
		t.Fatalf("cycle 1 CrossRef lookups = %d (%v), want only the 2 in-scope DOIs", crossref.total, crossref.hits)
	}
	if _, err := monitor.report(context.Background()); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if crossref.total != 2 {
		t.Fatalf("cycle 2 CrossRef lookups = %d (%v), want 0 new (cache)", crossref.total, crossref.hits)
	}
}

func TestWatchHealthCheckRetractionsFailedDOISkipsAndRetries(t *testing.T) {
	seedWatchHealthDefaultStore(t, []json.RawMessage{
		watchRetractionItem("RET", "10.777/ret", 1),
		watchRetractionItem("BAD", "10.777/bad", 1),
	})
	crossref := newFakeWatchCrossref(t)
	crossref.fail["10.777/bad"] = true
	monitor := newWatchRetractionMonitor("quick", healthPresets["quick"], true)

	report, err := monitor.report(context.Background())
	if err != nil {
		t.Fatalf("cycle report error = %v, want success despite one failed DOI", err)
	}
	var skip *healthSkip
	for i := range report.Skipped {
		if report.Skipped[i].Kind == "retracted_item" {
			skip = &report.Skipped[i]
		}
	}
	if skip == nil || skip.Precondition != "external_crossref" || !strings.Contains(skip.Detail, "HTTP 500") || !strings.Contains(skip.Detail, "10.777/bad") {
		t.Fatalf("skip = %+v, want external_crossref skip carrying the lookup error", skip)
	}
	var sawRetracted bool
	for _, f := range report.Findings {
		if f.Kind == "retracted_item" && f.ItemKey == "RET" {
			sawRetracted = true
		}
	}
	if !sawRetracted {
		t.Fatalf("findings = %+v, want the resolved DOI's retraction kept alongside the skip", report.Findings)
	}

	if _, err := monitor.report(context.Background()); err != nil {
		t.Fatalf("second report: %v", err)
	}
	if crossref.hits["10.777/bad"] != 2 || crossref.hits["10.777/ret"] != 1 {
		t.Fatalf("lookups = %v, want failed DOI retried and good DOI cached", crossref.hits)
	}
}

// A failed refresh of an expired cache entry must not read as "the retraction
// was withdrawn": the finding stays, nothing is counted resolved, the skip is
// logged, and recovery does not re-announce the same retraction as new.
func TestWatchHealthCheckRetractionsFailedRefreshKeepsLastKnownFinding(t *testing.T) {
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

	seedWatchHealthDefaultStore(t, []json.RawMessage{watchRetractionItem("RET", "10.777/ret", 1)})
	crossref := newFakeWatchCrossref(t)
	monitor := newWatchRetractionMonitor("citation", []string{"missing_citation"}, true)
	monitor.webhook = hook.URL
	clock := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	monitor.retractions.now = func() time.Time { return clock }
	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	retKey := watchHealthFindingKey(Finding{Kind: "retracted_item", ItemKey: "RET"})

	monitor.run(context.Background(), cmd, clock)
	if _, ok := monitor.previous[retKey]; !ok {
		t.Fatalf("cycle 1 findings = %v, want retracted_item RET", monitor.previous)
	}

	clock = clock.Add(watchRetractionCacheTTL + time.Minute)
	crossref.mu.Lock()
	crossref.fail["10.777/ret"] = true
	crossref.mu.Unlock()
	out.Reset()
	errOut.Reset()
	monitor.run(context.Background(), cmd, clock)
	if crossref.hits["10.777/ret"] != 2 {
		t.Fatalf("lookups = %v, want the expired entry re-requested", crossref.hits)
	}
	if _, ok := monitor.previous[retKey]; !ok {
		t.Fatalf("cycle 2 findings = %v, want RET kept from the last successful lookup", monitor.previous)
	}
	if got := out.String(); !strings.Contains(got, "[health] resolved_count 0") || strings.Contains(got, "[health] new") {
		t.Fatalf("cycle 2 stdout = %q, want resolved_count 0 and no new finding", got)
	}
	if got := errOut.String(); !strings.Contains(got, "[health] 2026-07-07T12:01:00Z skipped retracted_item (external_crossref): ") || !strings.Contains(got, "HTTP 500") {
		t.Fatalf("cycle 2 stderr = %q, want the retracted_item lookup-error skip", got)
	}

	crossref.mu.Lock()
	crossref.fail["10.777/ret"] = false
	crossref.mu.Unlock()
	out.Reset()
	errOut.Reset()
	monitor.run(context.Background(), cmd, clock.Add(time.Minute))
	if got := out.String(); !strings.Contains(got, "[health] resolved_count 0") || strings.Contains(got, "[health] new") {
		t.Fatalf("cycle 3 stdout = %q, want no new line for the recovered DOI", got)
	}
	if got := errOut.String(); got != "[health] 2026-07-07T12:02:00Z skip cleared retracted_item\n" {
		t.Fatalf("cycle 3 stderr = %q, want only the skip cleared line", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 3 {
		t.Fatalf("webhook payloads = %d, want 3", len(payloads))
	}
	for i, p := range payloads[1:] {
		if p.ResolvedCount != 0 || len(p.New) != 0 {
			t.Fatalf("cycle %d webhook = %+v, want resolved_count 0 and no new findings", i+2, p)
		}
	}
}

// A skip that persists unchanged is logged once, not every cycle.
func TestWatchHealthLogsPersistentSkipOnce(t *testing.T) {
	seedWatchHealthDefaultStore(t, []json.RawMessage{watchRetractionItem("A", "10.777/a", 1)})
	crossref := newFakeWatchCrossref(t)
	monitor := newWatchRetractionMonitor("citation", []string{"missing_citation", "retracted_item"}, false)
	cmd := &cobra.Command{}
	var errOut bytes.Buffer
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errOut)

	monitor.run(context.Background(), cmd, time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC))
	if got := errOut.String(); !strings.HasPrefix(got, "[health] 2026-07-06T12:00:00Z skipped retracted_item (external_crossref): ") || strings.Count(got, "\n") != 1 {
		t.Fatalf("cycle 1 stderr = %q, want one off-by-default retracted_item skip line", got)
	}
	errOut.Reset()
	monitor.run(context.Background(), cmd, time.Date(2026, 7, 6, 12, 1, 0, 0, time.UTC))
	if errOut.Len() != 0 || crossref.total != 0 {
		t.Fatalf("cycle 2 stderr = %q, lookups = %d; want the unchanged skip not repeated", errOut.String(), crossref.total)
	}
}

// DOIs are case-insensitive: case variants share one cache entry.
func TestWatchHealthCheckRetractionsCaseVariantDOIsShareOneLookup(t *testing.T) {
	seedWatchHealthDefaultStore(t, []json.RawMessage{
		watchRetractionItem("UP", "10.555/Flagged", 1),
		watchRetractionItem("LOW", "10.555/flagged", 1),
	})
	crossref := newFakeWatchCrossref(t)
	monitor := newWatchRetractionMonitor("quick", healthPresets["quick"], true)
	if _, err := monitor.report(context.Background()); err != nil {
		t.Fatalf("report: %v", err)
	}
	if crossref.total != 1 {
		t.Fatalf("CrossRef lookups = %d (%v), want 1 for two case-variant DOIs", crossref.total, crossref.hits)
	}
}

func TestWatchHealthCheckRetractionsNewFindingReportedAsNew(t *testing.T) {
	seedWatchHealthDefaultStore(t, []json.RawMessage{watchRetractionItem("A", "10.777/a", 1)})
	newFakeWatchCrossref(t)
	monitor := newWatchRetractionMonitor("quick", healthPresets["quick"], true)
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	monitor.run(context.Background(), cmd, time.Now())

	upsertWatchHealthDefaultStore(t, []json.RawMessage{watchRetractionItem("RET", "10.777/ret", 2)})
	out.Reset()
	monitor.run(context.Background(), cmd, time.Now())
	if got := out.String(); !strings.Contains(got, "[health] new critical retracted_item RET") {
		t.Fatalf("second cycle output = %q, want new retracted_item line", got)
	}
}

func TestWatchHealthCheckRetractionsOffMakesNoRequests(t *testing.T) {
	seedWatchHealthDefaultStore(t, []json.RawMessage{watchRetractionItem("RET", "10.777/ret", 1)})
	crossref := newFakeWatchCrossref(t)
	monitor := newWatchRetractionMonitor("all", healthPresets["all"], false)

	report, err := monitor.report(context.Background())
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if crossref.total != 0 {
		t.Fatalf("CrossRef lookups = %d, want none with the flag off", crossref.total)
	}
	var skip *healthSkip
	for i := range report.Skipped {
		if report.Skipped[i].Kind == "retracted_item" {
			skip = &report.Skipped[i]
		}
	}
	if skip == nil || !strings.Contains(skip.Detail, "off by default") {
		t.Fatalf("skipped = %+v, want the one-shot off-by-default retracted_item skip", report.Skipped)
	}
}

func TestWatchHealthCheckRetractionsRequiresHealth(t *testing.T) {
	cmd := newWatchCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"--once", "--health-check-retractions"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--health-check-retractions requires --health") {
		t.Fatalf("err = %v, want usage error requiring --health", err)
	}
}
