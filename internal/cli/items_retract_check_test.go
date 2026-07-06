// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"zotio/internal/store"
)

func seedRetractionDefaultStore(t *testing.T, items []json.RawMessage) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		_ = db.Close()
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func TestItemsRetractCheckJSONReportsFindingsSummaryAndUnregistered(t *testing.T) {
	seedRetractionDefaultStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"FLAG","version":1,"data":{"key":"FLAG","itemType":"journalArticle","title":"Flagged Work","DOI":"https://doi.org/10.555/Flagged","dateAdded":"2026-01-02T00:00:00Z"}}`),
		json.RawMessage(`{"key":"MISS","version":2,"data":{"key":"MISS","itemType":"journalArticle","title":"Missing DOI Registration","DOI":"10.555/missing","dateAdded":"2026-01-01T00:00:00Z"}}`),
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() == "/works/10.555%2FFlagged" {
			_, _ = w.Write([]byte(`{"message":{"updated-by":[` +
				`{"DOI":"10.555/retract-notice","type":"retraction","label":"Retracted","source":"publisher","updated":{"date-parts":[[2024,5,1]]}},` +
				`{"DOI":"10.555/concern-notice","type":"expression_of_concern","label":"Expression of concern","source":"crossmark","updated":{"date-parts":[[2023,7]]}},` +
				`{"DOI":"10.555/correction-notice","type":"correction","label":"Correction","source":"publisher","updated":{"date-parts":[[2022]]}}` +
				`]}}`))
			return
		}
		if r.URL.EscapedPath() == "/works/10.555%2Fmissing" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "unexpected CrossRef request", http.StatusNotFound)
		t.Errorf("unexpected CrossRef request path %q escaped %q", r.URL.Path, r.URL.EscapedPath())
	}))
	t.Cleanup(srv.Close)
	withBase(t, &crossrefRetractionBaseURL, srv.URL)

	flags := &rootFlags{asJSON: true, timeout: time.Second}
	cmd := newItemsRetractCheckCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("items retract-check: %v", err)
	}
	var report retractionCheckReport
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &report); err != nil {
		t.Fatalf("decode report %q: %v", out.String(), err)
	}
	if report.Summary.Checked != 2 || report.Summary.Flagged != 3 || report.Summary.Unregistered != 1 || report.Summary.Errors != 0 {
		t.Fatalf("summary = %+v, want checked=2 flagged=3 unregistered=1 errors=0", report.Summary)
	}
	if len(report.Findings) != 3 {
		t.Fatalf("findings = %+v, want 3 CrossRef update notices", report.Findings)
	}
	statuses := map[string]retractionCheckFinding{}
	for _, f := range report.Findings {
		statuses[f.Status] = f
		if f.ItemKey != "FLAG" || f.DOI != "10.555/Flagged" {
			t.Errorf("finding identity = key %q doi %q, want normalized flagged item", f.ItemKey, f.DOI)
		}
	}
	if statuses["retracted"].UpdateType != "retraction" || statuses["retracted"].NoticeDOI != "10.555/retract-notice" || statuses["retracted"].UpdateDate != "2024-05-01" {
		t.Errorf("retraction finding = %+v", statuses["retracted"])
	}
	if statuses["concern"].UpdateType != "expression_of_concern" || statuses["concern"].UpdateDate != "2023-07" {
		t.Errorf("concern finding = %+v", statuses["concern"])
	}
	if statuses["correction"].UpdateType != "correction" || statuses["correction"].UpdateDate != "2022" {
		t.Errorf("correction finding = %+v", statuses["correction"])
	}
}

func TestLookupCrossrefRetractionNoticesTreats404AsUnregistered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoded, err := url.PathUnescape(r.URL.EscapedPath())
		if err != nil || decoded != "/works/10.404/not-registered" {
			t.Fatalf("request path = escaped %q decoded %q err %v", r.URL.EscapedPath(), decoded, err)
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	withBase(t, &crossrefRetractionBaseURL, srv.URL)

	notices, registered, err := lookupCrossrefRetractionNotices(context.Background(), http.DefaultClient, "10.404/not-registered")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if registered || len(notices) != 0 {
		t.Fatalf("registered=%v notices=%+v, want unregistered with no findings", registered, notices)
	}
}
