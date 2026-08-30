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

	report, err := buildCollectionGapsReportWithCache(context.Background(), db, http.DefaultClient, nil, "COL", 10, 2)
	if err != nil {
		t.Fatalf("buildCollectionGapsReportWithCache: %v", err)
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
			refs, err := fetchOutgoingReferences(context.Background(), http.DefaultClient, "10.2000/source", referenceFetchOptions{})
			if err != nil {
				t.Fatalf("fetchOutgoingReferences: %v", err)
			}
			if !reflect.DeepEqual(refs.DOIs, []string{"10.2000/ref"}) {
				t.Fatalf("refs = %v, want Semantic Scholar DOI fallback", refs.DOIs)
			}
			if refs.Titles["10.2000/ref"] != "Fallback Reference" {
				t.Fatalf("titles = %v, want Semantic Scholar reference title", refs.Titles)
			}
		})
	}
}

func runCollectionsGapsTestCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newCollectionsGapsCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// The command handler owns two flag bounds that buildCollectionGapsReportWithCache
// never sees: a negative --limit would make the scan query nonsensical and a
// --top below 1 would slice an empty ranking. Both must surface as usage errors
// (exit 2), which is what shells and CI branch on to tell a caller mistake apart
// from a real failure.
func TestCollectionsGapsCmdRejectsOutOfRangeFlags(t *testing.T) {
	tests := []struct {
		name string
		flag string
		want string
	}{
		{name: "negative_limit", flag: "--limit=-1", want: "--limit must be >= 0"},
		{name: "zero_top", flag: "--top=0", want: "--top must be >= 1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := runCollectionsGapsTestCmd(t, tt.flag, "COL"); err == nil {
				t.Fatalf("%s = nil error, want a usage error", tt.flag)
			} else if got := ExitCode(err); got != 2 {
				t.Fatalf("exit code = %d, want 2 (usage); err=%v", got, err)
			} else if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want it to name %q", err.Error(), tt.want)
			}
		})
	}
}

// No sync has run, so openStoreForRead finds no database file and returns a nil
// store. That must reach the caller as a precondition (exit 9) carrying the sync
// remediation, not as a nil-store panic inside the report builder: exit 9 is the
// code that tells CI to run sync and retry.
func TestCollectionsGapsCmdRequiresASyncedStore(t *testing.T) {
	restore := SnapshotGlobals()
	defer restore()
	setActiveGroupID("")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTIO_DEMO", "")
	t.Setenv("ZOTERO_HOME", "")
	t.Setenv("ZOTERO_DATA_DIR", "")
	dbPath, pathErr := defaultDBPath("zotio")
	if pathErr != nil {
		t.Fatalf("defaultDBPath: %v", pathErr)
	}
	wantDBPath := filepath.Join(os.Getenv("HOME"), ".local", "share", "zotio", "data.db")
	if dbPath != wantDBPath {
		t.Fatalf("defaultDBPath = %q, want %q under the test HOME", dbPath, wantDBPath)
	}

	_, err := runCollectionsGapsTestCmd(t, "COL")
	if err == nil {
		t.Fatal("gaps against an unsynced store = nil error, want a precondition error")
	}
	if got := ExitCode(err); got != 9 {
		t.Fatalf("exit code = %d, want 9 (precondition); err=%v", got, err)
	}
	if !strings.Contains(err.Error(), "zotio sync") {
		t.Fatalf("error = %q, want the sync remediation", err.Error())
	}
}
