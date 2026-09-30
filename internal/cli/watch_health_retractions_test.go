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
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want none", errOut.String())
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
