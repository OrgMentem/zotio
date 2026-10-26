package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
	"zotio/internal/store"
)

func TestDataHealthAutoRejectsUnsupportedItemScope(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	c := client.New(&config.Config{BaseURL: "http://127.0.0.1:1/api/users/0"}, time.Second, 0)
	for _, flag := range []string{"since", "format"} {
		data, _, err := resolveRead(context.Background(), c, &rootFlags{dataSource: "auto", noCache: true}, "items", true, "/items", map[string]string{flag: "1", "tag": "ml"}, nil)
		if err == nil || ExitCode(err) != 9 || data != nil || !strings.Contains(err.Error(), flag) {
			t.Fatalf("%s: data=%s err=%v; want named precondition", flag, data, err)
		}
	}
}

func TestDataHealthMissingResourcesArePreconditions(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	for _, tc := range []struct{ resource, path string }{
		{"tags", "/tags"}, {"tags", "/tags/missing"}, {"items-trash", "/items/trash"}, {"searches", "/searches"}, {"collections", "/collections"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			_, _, err := resolveRead(context.Background(), nil, &rootFlags{dataSource: "local"}, tc.resource, true, tc.path, nil, nil)
			if ExitCode(classifyAPIError(err, &rootFlags{})) != 9 {
				t.Fatalf("error=%v; want precondition", err)
			}
		})
	}
}

func TestDataHealthAutoCacheSuppressesPermanentDeletion(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ReapMirroredItem("ABCD2345"); err != nil {
		t.Fatal(err)
	}
	row := `{"key":"ABCD2345","version":1,"data":{"key":"ABCD2345","itemType":"book","title":"Deleted"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/ABCD2345") {
			_, _ = w.Write([]byte(row))
		} else {
			_, _ = w.Write([]byte("[" + row + "]"))
		}
	}))
	defer srv.Close()
	c := client.New(&config.Config{BaseURL: srv.URL}, time.Second, 0)
	c.NoCache = true
	for _, tc := range []struct{ resource, path string }{
		{"items", "/items/ABCD2345"}, {"items", "/items"}, {"items-trash", "/items/trash"},
	} {
		if _, _, err := resolveRead(context.Background(), c, &rootFlags{dataSource: "auto"}, tc.resource, true, tc.path, nil, nil); err != nil {
			t.Fatal(err)
		}
		for _, resource := range []string{"items", "items-trash"} {
			raw, err := db.Get(resource, "ABCD2345")
			if err == nil && len(raw) != 0 {
				t.Fatalf("%s resurrected %s", tc.path, raw)
			}
			marked, err := db.PendingDeletion(resource, "ABCD2345")
			if err != nil || !marked {
				t.Fatalf("marker %s = %v, %v", resource, marked, err)
			}
		}
	}
}

func TestDataHealthBaselineSkipDegradesBadgeAndExit(t *testing.T) {
	report := healthReport{Baseline: &healthBaselineReport{Established: true}, baselineGate: "high", Skipped: []healthSkip{{Kind: "retracted_item"}}}
	if err := healthNewGateExitError(report); ExitCode(err) != 9 {
		t.Fatalf("error=%v; want exit 9", err)
	}
	if badge := healthBadgeForReport(report, "health"); badge.Color == "brightgreen" {
		t.Fatalf("badge=%+v", badge)
	}
	report.baselineGate = ""
	if err := healthNewGateExitError(report); err != nil {
		t.Fatal(err)
	}
}

func TestDataHealthPartialRetractionsRemainInReport(t *testing.T) {
	seedRetractionDefaultStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"ABCD2345","version":1,"data":{"key":"ABCD2345","itemType":"journalArticle","title":"Retracted","DOI":"10.777/retracted"}}`),
		json.RawMessage(`{"key":"ABCD2346","version":1,"data":{"key":"ABCD2346","itemType":"journalArticle","title":"Unknown","DOI":"10.777/failed"}}`),
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/works/10.777%2Fretracted":
			_, _ = w.Write([]byte(`{"message":{"updated-by":[{"DOI":"10.777/notice","type":"retraction","label":"Retracted","source":"publisher"}]}}`))
		case "/works/10.777%2Ffailed":
			http.Error(w, "unavailable", http.StatusTooManyRequests)
		default:
			_, _ = w.Write([]byte(`{"message":{}}`))
		}
	}))
	defer srv.Close()
	withBase(t, &crossrefRetractionBaseURL, srv.URL)
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	report, err := assembleHealthReport(localQueryStore{db}, &healthContext{preset: "all", checkRetractions: true, flags: &rootFlags{timeout: time.Second}, limit: 100}, "all", []string{"retracted_item"}, "", scopeResult{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Skipped) != 1 || len(report.allFindings) != 1 || report.Summary.Critical != 1 {
		t.Fatalf("report=%+v findings=%+v", report, report.allFindings)
	}
	report.Baseline = &healthBaselineReport{Established: true, New: report.allFindings}
	report.baselineGate = "high"
	if err := healthNewGateExitError(report); ExitCode(err) != 9 {
		t.Fatalf("error=%v; want degraded exit", err)
	}
	if badge := healthBadgeForReport(report, "health"); badge.Color == "brightgreen" {
		t.Fatalf("badge=%+v", badge)
	}
}
