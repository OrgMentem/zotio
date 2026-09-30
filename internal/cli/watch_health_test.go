// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/client"
)

func seedWatchHealthDefaultStore(t *testing.T, items []json.RawMessage) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cliTestSeedItemsStore(t, items)
}

func upsertWatchHealthDefaultStore(t *testing.T, items []json.RawMessage) {
	t.Helper()
	cliTestSeedItemsStore(t, items)
}

func TestWatchHealthFindingKeyUsesStableTaxonomyIdentity(t *testing.T) {
	itemBefore := Finding{Kind: "missing_citation", ItemKey: "ITEM1", Title: "Before", Evidence: map[string]any{"missing": "date"}}
	itemAfter := Finding{Kind: "missing_citation", ItemKey: "ITEM1", Title: "After", Evidence: map[string]any{"missing": "creators,date"}}
	if watchHealthFindingKey(itemBefore) != watchHealthFindingKey(itemAfter) {
		t.Fatalf("same kind+item_key should be a stable identity across evidence changes")
	}
	if got := watchHealthFindingDisplayKey(itemBefore); got != "ITEM1" {
		t.Fatalf("item display key = %q, want ITEM1", got)
	}

	groupBefore := Finding{Kind: "duplicate_candidates", Evidence: map[string]any{"group": "doi", "value": "10/example", "count": 2}}
	groupAfter := Finding{Kind: "duplicate_candidates", Evidence: map[string]any{"group": "doi", "value": "10/example", "count": 3}}
	if watchHealthFindingKey(groupBefore) != watchHealthFindingKey(groupAfter) {
		t.Fatalf("same grouped duplicate should remain stable when its count changes")
	}
	if got := watchHealthFindingDisplayKey(groupBefore); got != "doi:10/example" {
		t.Fatalf("group display key = %q, want doi:10/example", got)
	}
	if got := watchHealthFindingTitle(groupBefore); got != "doi=10/example" {
		t.Fatalf("group title = %q, want doi=10/example", got)
	}

	canonical := Finding{Kind: "tag_drift", Evidence: map[string]any{"canonical": "ai"}}
	if got := watchHealthFindingDisplayKey(canonical); got != "ai" {
		t.Fatalf("canonical display key = %q, want ai", got)
	}
	if got := watchHealthFindingTitle(canonical); got != "canonical tag ai" {
		t.Fatalf("canonical title = %q, want canonical tag ai", got)
	}
}

// One item can own several broken attachments, and the finding is keyed by the
// PARENT so the repair command can consume it. Without the attachment in the
// identity, watch and the baseline diff lose one of the two.
func TestWatchHealthFindingKeySeparatesTwoBrokenAttachmentsOfOneParent(t *testing.T) {
	first := Finding{Kind: "broken_attachment_file", ItemKey: "P1", Evidence: map[string]any{"attachment": "ATT1", "reason": "missing"}}
	second := Finding{Kind: "broken_attachment_file", ItemKey: "P1", Evidence: map[string]any{"attachment": "ATT2", "reason": "missing"}}
	if watchHealthFindingKey(first) == watchHealthFindingKey(second) {
		t.Fatal("two broken children of one parent collapsed into one identity")
	}
	if got := FindingIdentities([]Finding{first, second}); len(got) != 2 {
		t.Fatalf("baseline identities = %v, want both broken attachments", got)
	}
	// The reason changing must not invent a new finding.
	moved := Finding{Kind: "broken_attachment_file", ItemKey: "P1", Evidence: map[string]any{"attachment": "ATT1", "reason": "unresolved"}}
	if watchHealthFindingKey(first) != watchHealthFindingKey(moved) {
		t.Fatal("identity must stay stable when only the failure reason changes")
	}
}

// A finding that advertises a command must appear in the plan, and one that
// only offers manual prose must not: a plan step that no-ops on its own keys
// is worse than no step.
func TestHealthRemediationPlanCoversTheNewFixers(t *testing.T) {
	steps := buildHealthRemediationPlan([]Finding{
		{Kind: "missing_tags", ItemKey: "T1", RecommendedAction: &RecommendedAction{Command: "zotio items enrich --missing-subjects --keys-from -"}},
		{Kind: "broken_attachment_file", ItemKey: "P1", Evidence: map[string]any{"attachment": "ATT1"}, RecommendedAction: &RecommendedAction{Command: "zotio items enrich --repair-pdf --keys-from -"}},
		{Kind: "broken_attachment_file", ItemKey: "P1", Evidence: map[string]any{"attachment": "ATT2"}, RecommendedAction: &RecommendedAction{Command: "zotio items enrich --repair-pdf --keys-from -"}},
		{Kind: "broken_attachment_file", ItemKey: "PNODOI", Evidence: map[string]any{"attachment": "ATT3"}, RecommendedAction: &RecommendedAction{Text: "Re-link the file in Zotero or re-download the attachment"}},
		// A stale mirror row DOES carry a command, just a different one.
		// Bucketing by kind alone would pipe PSTALE into --repair-pdf, which
		// re-attaches nothing for an attachment the desktop no longer has.
		{Kind: "broken_attachment_file", ItemKey: "PSTALE", Evidence: map[string]any{"attachment": "ATT4", "reason": brokenAttachmentStaleMirror}, RecommendedAction: &RecommendedAction{Command: "zotio sync"}},
	})
	byKind := map[string]healthRemediationPlanStep{}
	for _, s := range steps {
		byKind[s.Kind] = s
	}
	tags, ok := byKind["missing_tags"]
	if !ok || len(tags.Keys) != 1 || tags.Keys[0] != "T1" {
		t.Fatalf("missing_tags step = %+v, want T1", tags)
	}
	broken, ok := byKind["broken_attachment_file"]
	if !ok {
		t.Fatal("no broken_attachment_file plan step; the finding advertises a command")
	}
	// One repair covers the parent, so two broken children collapse to one
	// key here — the opposite of the watch identity, and correct.
	if len(broken.Keys) != 1 || broken.Keys[0] != "P1" {
		t.Fatalf("broken step keys = %v, want P1 once and PNODOI excluded", broken.Keys)
	}
	for _, k := range broken.Keys {
		if k == "PSTALE" {
			t.Errorf("broken step keys = %v; a stale-mirror finding recommends sync, not --repair-pdf", broken.Keys)
		}
	}
}

func TestWatchHealthRunReportsBaselineThenOnlyNewAndResolvedFindings(t *testing.T) {
	seedWatchHealthDefaultStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"OLD","version":1,"data":{"key":"OLD","itemType":"journalArticle","title":"Old Missing","dateAdded":"2026-01-01T00:00:00Z"}}`),
	})
	monitor := &watchHealthMonitor{
		enabled:  true,
		preset:   "citation",
		kinds:    []string{"missing_citation"},
		flags:    &rootFlags{},
		previous: map[string]Finding{},
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	monitor.run(context.Background(), cmd, time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC))
	if errOut.Len() != 0 {
		t.Fatalf("baseline stderr = %q, want none", errOut.String())
	}
	if got := out.String(); !strings.Contains(got, "[health] baseline citation total=1") {
		t.Fatalf("baseline output = %q, want total=1", got)
	}

	upsertWatchHealthDefaultStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"OLD","version":2,"data":{"key":"OLD","itemType":"journalArticle","title":"Old Complete","creators":[{"lastName":"Doe"}],"date":"2020","publicationTitle":"Journal","dateAdded":"2026-01-01T00:00:00Z"}}`),
		json.RawMessage(`{"key":"NEW","version":3,"data":{"key":"NEW","itemType":"journalArticle","title":"New Missing","dateAdded":"2026-01-02T00:00:00Z"}}`),
	})
	out.Reset()
	errOut.Reset()

	monitor.run(context.Background(), cmd, time.Date(2026, 7, 6, 12, 1, 0, 0, time.UTC))
	if errOut.Len() != 0 {
		t.Fatalf("second cycle stderr = %q, want none", errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, `[health] new high missing_citation NEW "New Missing"`) {
		t.Fatalf("second cycle output = %q, want only NEW finding", got)
	}
	if strings.Contains(got, "OLD") {
		t.Fatalf("second cycle output = %q, OLD should be resolved, not reported as new", got)
	}
	if !strings.Contains(got, "[health] resolved_count 1") {
		t.Fatalf("second cycle output = %q, want one resolved finding", got)
	}
}

func TestWatchHealthRunLogsHealthErrorsWithoutAborting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	monitor := &watchHealthMonitor{enabled: true, preset: "quick", kinds: []string{"missing_citation"}, flags: &rootFlags{}, previous: map[string]Finding{}}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	monitor.run(context.Background(), cmd, time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC))
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want no health report when store is unavailable", out.String())
	}
	if !strings.Contains(errOut.String(), "[health]") || !strings.Contains(errOut.String(), "check error") {
		t.Fatalf("stderr = %q, want logged non-fatal health check error", errOut.String())
	}
}

func TestWatchHealthScopeReportsOnlyCohortFindings(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllowPrivateOutbound) })
	var mu sync.Mutex
	var bodies [][]byte
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)

	seedWatchHealthDefaultStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"IN1","version":1,"data":{"key":"IN1","itemType":"journalArticle","title":"In Missing","collections":["C1"],"dateAdded":"2026-01-01T00:00:00Z"}}`),
		json.RawMessage(`{"key":"OUT1","version":1,"data":{"key":"OUT1","itemType":"journalArticle","title":"Out Missing","collections":["C2"],"dateAdded":"2026-01-01T00:00:00Z"}}`),
	})
	monitor := &watchHealthMonitor{enabled: true, preset: "citation", kinds: []string{"missing_citation"}, webhook: hook.URL, flags: &rootFlags{}, previous: map[string]Finding{}, acked: map[string]Finding{}}
	if err := monitor.setScope("collection:C1"); err != nil {
		t.Fatalf("setScope: %v", err)
	}
	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	monitor.run(context.Background(), cmd, time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC))
	if errOut.Len() != 0 {
		t.Fatalf("baseline stderr = %q", errOut.String())
	}
	if got := out.String(); !strings.Contains(got, "[health] baseline citation total=1") {
		t.Fatalf("baseline output = %q, want only the C1 finding", got)
	}

	upsertWatchHealthDefaultStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"IN2","version":2,"data":{"key":"IN2","itemType":"journalArticle","title":"In New","collections":["C1"],"dateAdded":"2026-01-02T00:00:00Z"}}`),
		json.RawMessage(`{"key":"OUT2","version":2,"data":{"key":"OUT2","itemType":"journalArticle","title":"Out New","collections":["C2"],"dateAdded":"2026-01-02T00:00:00Z"}}`),
	})
	out.Reset()
	monitor.run(context.Background(), cmd, time.Date(2026, 7, 6, 12, 1, 0, 0, time.UTC))
	got := out.String()
	if !strings.Contains(got, `[health] new high missing_citation IN2 "In New"`) || strings.Contains(got, "OUT") {
		t.Fatalf("second cycle output = %q, want IN2 only", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("webhook posts = %d, want 2", len(bodies))
	}
	if !strings.Contains(string(bodies[1]), `"scope":"collection:C1"`) {
		t.Fatalf("webhook payload = %s, want scope collection:C1", bodies[1])
	}
}

func TestWatchHealthScopeErrorSkipsCycleAndKeepsBaseline(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllowPrivateOutbound) })
	var posts atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)
	oldLive := savedSearchLiveClient
	savedSearchLiveClient = func(context.Context, *rootFlags) (*client.Client, string, error) {
		return nil, "Zotero desktop local API is not reachable", nil
	}
	t.Cleanup(func() { savedSearchLiveClient = oldLive })

	seedWatchHealthDefaultStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"A","version":1,"data":{"key":"A","itemType":"journalArticle","title":"A Missing","dateAdded":"2026-01-01T00:00:00Z"}}`),
	})
	if err := (&watchHealthMonitor{}).setScope("bogus"); err == nil {
		t.Fatal("setScope(bogus) = nil, want usage error")
	}
	monitor := &watchHealthMonitor{enabled: true, preset: "citation", kinds: []string{"missing_citation"}, webhook: hook.URL, flags: &rootFlags{}, previous: map[string]Finding{}, acked: map[string]Finding{}}
	if err := monitor.setScope("saved-search:X"); err != nil {
		t.Fatalf("setScope: %v", err)
	}
	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	monitor.run(context.Background(), cmd, time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC))
	if !strings.Contains(errOut.String(), "[health] 2026-07-06T12:00:00Z scope error: saved-search:X: Zotero desktop local API is not reachable") {
		t.Fatalf("stderr = %q, want scope error line", errOut.String())
	}
	if out.Len() != 0 || posts.Load() != 0 || monitor.baseline || monitor.ackedBaseline {
		t.Fatalf("scope error cycle reported %q, posted %d, baseline=%v acked=%v; want nothing", out.String(), posts.Load(), monitor.baseline, monitor.ackedBaseline)
	}
}

func TestWatchHealthWebhookPostsDriftPayload(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllowPrivateOutbound) })

	var received []byte
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("webhook method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)

	monitor := &watchHealthMonitor{preset: "citation", webhook: hook.URL}
	cmd := &cobra.Command{}
	var errOut bytes.Buffer
	cmd.SetErr(&errOut)
	cycleAt := time.Date(2026, 7, 6, 12, 30, 0, 0, time.UTC)
	if err := monitor.deliverWebhook(context.Background(), cmd, cycleAt, []Finding{{Kind: "missing_citation", Severity: sevHigh, ItemKey: "K1", Title: "Missing"}}, 2, healthSummary{High: 1, Total: 1}); err != nil {
		t.Fatalf("deliverWebhook: %v", err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("webhook stderr = %q, want none", errOut.String())
	}
	if len(received) == 0 {
		t.Fatal("webhook received no payload")
	}
	var payload watchHealthWebhookPayload
	if err := json.Unmarshal(received, &payload); err != nil {
		t.Fatalf("decode webhook payload %q: %v", string(received), err)
	}
	if !payload.CycleAt.Equal(cycleAt) || payload.Preset != "citation" || payload.ResolvedCount != 2 || payload.Totals.High != 1 || payload.Totals.Total != 1 {
		t.Fatalf("payload metadata = %+v, want cycle_at/preset/resolved_count/totals", payload)
	}
	if len(payload.New) != 1 || payload.New[0].Kind != "missing_citation" || payload.New[0].ItemKey != "K1" {
		t.Fatalf("payload new findings = %+v, want missing_citation K1", payload.New)
	}
	if err := monitor.deliverWebhook(context.Background(), cmd, cycleAt, nil, 0, healthSummary{}); err != nil {
		t.Fatalf("deliverWebhook without new findings: %v", err)
	}
	var emptyPayload struct {
		New json.RawMessage `json:"new"`
	}
	if err := json.Unmarshal(received, &emptyPayload); err != nil {
		t.Fatalf("decode empty webhook payload %q: %v", string(received), err)
	}
	if string(emptyPayload.New) != "[]" {
		t.Fatalf("empty webhook new = %s, want []", emptyPayload.New)
	}
}

// A failed webhook POST must not consume a new-finding alert: the
// acknowledged baseline advances only on 2xx, so the next cycle retries the
// same transition.
func TestWatchHealthWebhookRetriesAfterFailure(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllowPrivateOutbound) })

	var mu struct {
		sync.Mutex
		payloads []watchHealthWebhookPayload
	}
	var calls atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload watchHealthWebhookPayload
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		mu.payloads = append(mu.payloads, payload)
		mu.Unlock()
		if calls.Add(1) == 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(hook.Close)

	seedWatchHealthDefaultStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"BASE","version":1,"data":{"key":"BASE","itemType":"journalArticle","title":"Base Complete","creators":[{"lastName":"Doe"}],"date":"2020","publicationTitle":"Journal","dateAdded":"2026-01-01T00:00:00Z"}}`),
	})
	monitor := &watchHealthMonitor{
		enabled:  true,
		preset:   "citation",
		kinds:    []string{"missing_citation"},
		flags:    &rootFlags{},
		previous: map[string]Finding{},
		acked:    map[string]Finding{},
		webhook:  hook.URL,
	}
	newCycleCmd := func() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
		cmd := &cobra.Command{}
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		return cmd, &out, &errOut
	}

	cmd, _, _ := newCycleCmd()
	monitor.run(context.Background(), cmd, time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC))
	if !monitor.ackedBaseline {
		t.Fatal("baseline cycle did not establish the webhook baseline")
	}

	upsertWatchHealthDefaultStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"NEW","version":2,"data":{"key":"NEW","itemType":"journalArticle","title":"New Missing","dateAdded":"2026-01-02T00:00:00Z"}}`),
	})
	cmd, _, _ = newCycleCmd()
	monitor.run(context.Background(), cmd, time.Date(2026, 7, 6, 12, 1, 0, 0, time.UTC))
	ackedAfterFailure := len(monitor.acked)

	cmd, _, _ = newCycleCmd()
	monitor.run(context.Background(), cmd, time.Date(2026, 7, 6, 12, 2, 0, 0, time.UTC))

	mu.Lock()
	defer mu.Unlock()
	if len(mu.payloads) != 3 {
		t.Fatalf("webhook payloads = %d, want 3 (baseline, failed new, retried new)", len(mu.payloads))
	}
	if len(mu.payloads[1].New) != 1 || mu.payloads[1].New[0].ItemKey != "NEW" {
		t.Fatalf("failed payload new = %+v, want NEW", mu.payloads[1].New)
	}
	if len(mu.payloads[2].New) != 1 || mu.payloads[2].New[0].ItemKey != "NEW" {
		t.Fatalf("retried payload new = %+v, want NEW again after 503", mu.payloads[2].New)
	}
	if ackedAfterFailure != 0 {
		t.Fatalf("acked size after failed POST = %d, want unchanged empty baseline", ackedAfterFailure)
	}
	if len(monitor.acked) != 1 {
		t.Fatalf("acked size after recovered POST = %d, want NEW", len(monitor.acked))
	}
}

func TestWatchHealthWebhookFailedBaselineKeepsNewFinding(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllowPrivateOutbound) })

	payloads := make(chan watchHealthWebhookPayload, 2)
	var calls atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload watchHealthWebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decoding webhook: %v", err)
		}
		payloads <- payload
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()

	seedWatchHealthDefaultStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"BASE","version":1,"data":{"key":"BASE","itemType":"journalArticle","title":"Complete","creators":[{"lastName":"Doe"}],"date":"2020","publicationTitle":"Journal"}}`),
	})
	monitor := &watchHealthMonitor{
		enabled: true, preset: "citation", kinds: []string{"missing_citation"},
		flags: &rootFlags{}, webhook: hook.URL,
	}
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	monitor.run(context.Background(), cmd, time.Now())
	if monitor.ackedBaseline {
		t.Fatal("failed baseline POST was marked acknowledged")
	}
	upsertWatchHealthDefaultStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"NEW","version":2,"data":{"key":"NEW","itemType":"journalArticle","title":"Missing"}}`),
	})
	monitor.run(context.Background(), cmd, time.Now())
	for i := range 2 {
		select {
		case payload := <-payloads:
			if i == 1 && (len(payload.New) != 1 || payload.New[0].ItemKey != "NEW") {
				t.Fatalf("recovered webhook new = %+v, want NEW", payload.New)
			}
		case <-time.After(time.Second):
			t.Fatal("webhook did not receive both cycles")
		}
	}
	if !monitor.ackedBaseline {
		t.Fatal("successful drift POST did not acknowledge the baseline")
	}
}
