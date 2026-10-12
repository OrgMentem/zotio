// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

func seedRetractionDefaultStore(t *testing.T, items []json.RawMessage) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cliTestSeedItemsStore(t, items)
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

// renderOnTTY runs fn with a pseudo-terminal as its output and returns what
// fn wrote. The human table branch only runs when the output is a terminal.
// The slave end stays open until a sentinel written after fn returns has
// come back through the master end, so no output is lost to an early close.
func renderOnTTY(t *testing.T, fn func(out *os.File) error) string {
	t.Helper()
	master, slave, err := openTTYPair()
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	t.Cleanup(func() {
		slave.Close()
		master.Close()
	})

	var mu sync.Mutex
	var captured bytes.Buffer
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			mu.Lock()
			captured.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	if err := fn(slave); err != nil {
		t.Fatalf("render: %v", err)
	}
	const sentinel = "<<tty-done>>"
	if _, err := slave.WriteString(sentinel); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := captured.String()
		mu.Unlock()
		if i := strings.Index(got, sentinel); i >= 0 {
			return got[:i]
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out reading the rendered output; got %q", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRenderRetractionCheckReportHumanTable(t *testing.T) {
	t.Cleanup(snapshotCLIGlobals())
	noColor = true

	report := retractionCheckReport{
		Findings: []retractionCheckFinding{{
			ItemKey:    "FLAGKEY1",
			Status:     "retracted",
			UpdateType: "retraction",
			NoticeDOI:  "10.555/retract-notice",
			Title:      "Flagged Work",
		}},
		Summary: retractionCheckSummary{Checked: 3, Flagged: 1, Errors: 2},
	}
	got := renderOnTTY(t, func(out *os.File) error {
		cmd := &cobra.Command{}
		cmd.SetOut(out)
		return renderRetractionCheckReport(cmd, &rootFlags{}, report)
	})
	for _, want := range []string{"KEY", "STATUS", "TITLE", "FLAGKEY1", "retracted", "10.555/retract-notice", "Flagged Work"} {
		if !strings.Contains(got, want) {
			t.Errorf("table lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "Checked 3") {
		t.Errorf("output lacks the summary line:\n%s", got)
	}
	if !strings.Contains(got, "2 CrossRef lookup error") {
		t.Errorf("output lacks the lookup-error notice for Summary.Errors=2:\n%s", got)
	}
}

func TestRenderRetractionCheckReportHumanOmitsErrorNoticeWithoutErrors(t *testing.T) {
	t.Cleanup(snapshotCLIGlobals())
	noColor = true

	got := renderOnTTY(t, func(out *os.File) error {
		cmd := &cobra.Command{}
		cmd.SetOut(out)
		return renderRetractionCheckReport(cmd, &rootFlags{}, retractionCheckReport{Summary: retractionCheckSummary{Checked: 1}})
	})
	if !strings.Contains(got, "Checked 1") {
		t.Errorf("output = %q, want the summary line", got)
	}
	if strings.Contains(got, "lookup error") || strings.Contains(got, "KEY") {
		t.Errorf("output = %q, want no table and no lookup-error notice", got)
	}
}

func TestLookupCrossrefRetractionNoticesTreats404AsUnregistered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoded, err := url.PathUnescape(r.URL.EscapedPath())
		if err != nil || decoded != "/works/10.404/not-registered" {
			t.Errorf("request path = escaped %q decoded %q err %v", r.URL.EscapedPath(), decoded, err)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
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

func TestLookupCrossrefRetractionNoticesRedirectPolicy(t *testing.T) {
	t.Run("refuses cross-origin 307 before target request", func(t *testing.T) {
		targetContact := make(chan struct{}, 1)
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetContact <- struct{}{}
			_, _ = w.Write([]byte(`{"message":{"updated-by":[]}}`))
		}))
		t.Cleanup(target.Close)
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/result", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(source.Close)
		withBase(t, &crossrefRetractionBaseURL, source.URL)

		if _, _, err := lookupCrossrefRetractionNotices(context.Background(), http.DefaultClient, "10.1/redirect"); err == nil {
			t.Fatal("expected cross-origin redirect error")
		}
		select {
		case <-targetContact:
			t.Fatal("cross-origin redirect target was contacted")
		default:
		}
	})

	t.Run("allows same-origin redirect", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/result" {
				http.Redirect(w, r, "/result", http.StatusTemporaryRedirect)
				return
			}
			_, _ = w.Write([]byte(`{"message":{"updated-by":[]}}`))
		}))
		t.Cleanup(srv.Close)
		withBase(t, &crossrefRetractionBaseURL, srv.URL)

		notices, registered, err := lookupCrossrefRetractionNotices(context.Background(), http.DefaultClient, "10.1/redirect")
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if !registered || len(notices) != 0 {
			t.Fatalf("registered=%v notices=%+v, want registered with no notices", registered, notices)
		}
	})
}

func TestProbeCrossrefRetractionAPIRedirectPolicy(t *testing.T) {
	t.Run("refuses cross-origin redirect before target request", func(t *testing.T) {
		targetContact := make(chan struct{}, 1)
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetContact <- struct{}{}
			_, _ = w.Write([]byte(`{}`))
		}))
		t.Cleanup(target.Close)
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/result", http.StatusFound)
		}))
		t.Cleanup(source.Close)
		withBase(t, &crossrefRetractionBaseURL, source.URL)

		if err := probeCrossrefRetractionAPI(context.Background(), http.DefaultClient); err == nil {
			t.Fatal("expected cross-origin redirect error")
		}
		select {
		case <-targetContact:
			t.Fatal("cross-origin redirect target was contacted")
		default:
		}
	})

	t.Run("allows same-origin redirect", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/result" {
				http.Redirect(w, r, "/result", http.StatusFound)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		}))
		t.Cleanup(srv.Close)
		withBase(t, &crossrefRetractionBaseURL, srv.URL)

		if err := probeCrossrefRetractionAPI(context.Background(), http.DefaultClient); err != nil {
			t.Fatalf("probe: %v", err)
		}
	})
}
