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
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/store"
)

func seedWatchHealthDefaultStore(t *testing.T, items []json.RawMessage) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	upsertWatchHealthDefaultStore(t, items)
}

func upsertWatchHealthDefaultStore(t *testing.T, items []json.RawMessage) {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		_ = db.Close()
		t.Fatalf("upsert items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
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

func TestWatchHealthWebhookPostsDriftPayload(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = oldAllowPrivateOutbound })

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
	monitor.deliverWebhook(context.Background(), cmd, cycleAt, []Finding{{Kind: "missing_citation", Severity: sevHigh, ItemKey: "K1", Title: "Missing"}}, 2, healthSummary{High: 1, Total: 1})
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
}
