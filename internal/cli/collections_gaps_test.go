// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"zotio/internal/store"
)

func seedCollectionGapsStore(t *testing.T) localQueryStore {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	items := []json.RawMessage{
		json.RawMessage(`{"key":"SRC","version":1,"data":{"key":"SRC","itemType":"journalArticle","title":"Seed Paper","DOI":"10.1000/source","collections":["COL"],"dateModified":"2026-01-02T00:00:00Z"}}`),
		json.RawMessage(`{"key":"HAVE","version":2,"data":{"key":"HAVE","itemType":"journalArticle","title":"Already Held","DOI":"10.1000/existing","dateModified":"2026-01-01T00:00:00Z"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	// The store persists top-level items with parent_key = '' — the query must
	// match that shape as-written; no fixture normalization (regression guard
	// for the parent_key IS NULL bug caught in review).
	return localQueryStore{db}
}

func TestBuildCollectionGapsReportRanksExcludesAndLooksUpOnlyTopTitles(t *testing.T) {
	db := seedCollectionGapsStore(t)

	coci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoded, err := url.PathUnescape(r.URL.EscapedPath())
		if err != nil || decoded != "/references/10.1000/source" {
			http.Error(w, "unexpected COCI path", http.StatusNotFound)
			t.Errorf("COCI path = escaped %q decoded %q err %v", r.URL.EscapedPath(), decoded, err)
			return
		}
		_, _ = w.Write([]byte(`[` +
			`{"cited":"10.1000/existing"},` +
			`{"cited":"10.1000/BETA"},` +
			`{"cited":"https://doi.org/10.1000/beta"},` +
			`{"cited":"10.1000/alpha"},` +
			`{"cited":"10.1000/gamma"}` +
			`]`))
	}))
	t.Cleanup(coci.Close)
	withBase(t, &collectionGapsOpenCitationsBase, coci.URL)

	ss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Semantic Scholar should not be queried when COCI returns references; got %s", r.URL.String())
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(ss.Close)
	withBase(t, &enrichSemanticScholarBase, ss.URL)

	var crossrefDOIs []string
	crossref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoded, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/works/"))
		if err != nil {
			t.Errorf("CrossRef path = %q: %v", r.URL.EscapedPath(), err)
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		crossrefDOIs = append(crossrefDOIs, decoded)
		title := map[string]string{
			"10.1000/beta":  "Beta Gap Title",
			"10.1000/alpha": "Alpha Gap Title",
		}[decoded]
		if title == "" {
			t.Errorf("CrossRef title lookup for non-top DOI %q", decoded)
			http.Error(w, "unexpected DOI", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"title": []string{title}}})
	}))
	t.Cleanup(crossref.Close)
	withBase(t, &enrichCrossRefBase, crossref.URL)

	report, err := buildCollectionGapsReport(context.Background(), db, http.DefaultClient, "COL", 10, 2)
	if err != nil {
		t.Fatalf("buildCollectionGapsReport: %v", err)
	}
	wantSummary := collectionGapsSummary{ItemsScanned: 1, ReferencesSeen: 5, UniqueCitedDOIs: 4, AlreadyInLibrary: 1, Gaps: 3}
	if report.Summary != wantSummary {
		t.Fatalf("summary = %+v, want %+v", report.Summary, wantSummary)
	}
	if len(report.Rows) != 2 {
		t.Fatalf("rows = %+v, want top 2", report.Rows)
	}
	wantRows := []collectionGapRow{
		{Rank: 1, DOI: "10.1000/beta", Count: 2, Title: "Beta Gap Title", Action: "zotio import doi 10.1000/beta"},
		{Rank: 2, DOI: "10.1000/alpha", Count: 1, Title: "Alpha Gap Title", Action: "zotio import doi 10.1000/alpha"},
	}
	if !reflect.DeepEqual(report.Rows, wantRows) {
		t.Fatalf("rows = %+v, want %+v", report.Rows, wantRows)
	}
	if !reflect.DeepEqual(crossrefDOIs, []string{"10.1000/beta", "10.1000/alpha"}) {
		t.Fatalf("CrossRef title lookups = %v, want top rows only", crossrefDOIs)
	}
}

func TestFetchOutgoingReferenceDOIsFallsBackToSemanticScholarOnCOCIEmptyOrError(t *testing.T) {
	cases := []struct {
		name       string
		cociStatus int
		cociBody   string
	}{
		{name: "empty COCI", cociStatus: http.StatusOK, cociBody: `[]`},
		{name: "COCI error", cociStatus: http.StatusServiceUnavailable, cociBody: `temporarily unavailable`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			coci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.cociStatus)
				_, _ = w.Write([]byte(tc.cociBody))
			}))
			t.Cleanup(coci.Close)
			withBase(t, &collectionGapsOpenCitationsBase, coci.URL)

			ss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				decoded, err := url.PathUnescape(r.URL.EscapedPath())
				if err != nil || !strings.HasPrefix(decoded, "/paper/DOI:10.2000/source/references") {
					http.Error(w, "unexpected Semantic Scholar path", http.StatusNotFound)
					t.Errorf("Semantic Scholar path = escaped %q decoded %q err %v", r.URL.EscapedPath(), decoded, err)
					return
				}
				_, _ = w.Write([]byte(`{"data":[{"citedPaper":{"externalIds":{"DOI":"10.2000/ref"},"title":"Fallback Reference"}}]}`))
			}))
			t.Cleanup(ss.Close)
			withBase(t, &enrichSemanticScholarBase, ss.URL)

			refs, titles, err := fetchOutgoingReferenceDOIs(context.Background(), http.DefaultClient, "10.2000/source")
			if err != nil {
				t.Fatalf("fetchOutgoingReferenceDOIs: %v", err)
			}
			if !reflect.DeepEqual(refs, []string{"10.2000/ref"}) {
				t.Fatalf("refs = %v, want Semantic Scholar DOI fallback", refs)
			}
			if titles["10.2000/ref"] != "Fallback Reference" {
				t.Fatalf("titles = %v, want Semantic Scholar reference title", titles)
			}
		})
	}
}
