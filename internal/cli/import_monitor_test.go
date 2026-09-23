// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func runImportMonitorTest(t *testing.T, args ...string) (importMonitorReport, error) {
	t.Helper()
	return runImportMonitorTestWithFlags(t, &rootFlags{asJSON: true, noCache: true, timeout: time.Second}, args...)
}

func runImportMonitorTestWithFlags(t *testing.T, flags *rootFlags, args ...string) (importMonitorReport, error) {
	t.Helper()
	cmd := newImportMonitorCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.Execute()
	var report importMonitorReport
	if err == nil {
		if decodeErr := json.Unmarshal(out.Bytes(), &report); decodeErr != nil {
			t.Fatalf("decode monitor report %q: %v", out.String(), decodeErr)
		}
	}
	return report, err
}

func TestImportMonitorAuthorDedupeAndResolve(t *testing.T) {
	seedImportDiscoverStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"HELD","version":1,"data":{"key":"HELD","itemType":"journalArticle","title":"Held DOI","DOI":"10.5000/held"}}`),
		json.RawMessage(`{"key":"TITLE","version":1,"data":{"key":"TITLE","itemType":"journalArticle","title":"Bayes’ Rule for Graphs"}}`),
	})
	t.Setenv("UNPAYWALL_EMAIL", "research@example.org")
	openAlex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/works" {
			t.Errorf("unexpected OpenAlex path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		v := r.URL.Query()
		if v.Get("filter") != "from_publication_date:2026-01-01,to_publication_date:2026-08-31,author.orcid:https://orcid.org/0000-0002-1825-0097" || v.Get("sort") != "publication_date:desc" || v.Get("search") != "citation graphs" || v.Get("mailto") != "research@example.org" || v.Get("cursor") != "*" {
			t.Errorf("OpenAlex query = %v", v)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]any{"next_cursor": ""}, "results": []map[string]string{
			{"id": "https://openalex.org/W1", "doi": "https://doi.org/10.5000/held", "title": "Held DOI", "publication_date": "2026-06-10"},
			{"id": "https://openalex.org/W2", "doi": "https://doi.org/10.5000/title", "title": "Bayes' Rule for Graphs", "publication_date": "2026-06-09"},
			{"id": "https://openalex.org/W3", "doi": "https://doi.org/10.5000/new", "title": "New citation graph", "publication_date": "2026-06-08"},
			{"id": "https://openalex.org/W4", "title": "Another new work", "publication_date": "2026-06-07"},
		}})
	}))
	t.Cleanup(openAlex.Close)
	withBase(t, &enrichOpenAlexBase, openAlex.URL)
	crossRef := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.EscapedPath(), "10.5000") {
			t.Errorf("unexpected metadata URL %s", r.URL)
		}
		writeCrossRefWork(t, w, "10.5000/new", "New citation graph")
	}))
	t.Cleanup(crossRef.Close)
	withBase(t, &enrichCrossRefBase, crossRef.URL)

	path := filepath.Join(t.TempDir(), "monitor.json")
	report, err := runImportMonitorTest(t, "--author", "0000-0002-1825-0097", "--query", "citation graphs", "--since", "2026-01-01", "--until", "2026-08-31", "--out", path)
	if err != nil {
		t.Fatal(err)
	}
	if report.WorksSeen != 4 || report.Emitted != 1 || report.SkippedAlreadyInLibraryDOI != 1 || report.SkippedAlreadyInLibraryTitle != 1 || report.SkippedWithoutIdentifier != 1 || report.Truncated {
		t.Fatalf("report = %+v", report)
	}
	manifest, err := readImportManifest(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 2 || len(manifest.Entries) != 1 {
		t.Fatalf("manifest = %+v", manifest)
	}
	entry := manifest.Entries[0]
	if entry.Identifier != "10.5000/new" || entry.Status != "unresolved" || entry.Action != "create" || entry.Discovery == nil || entry.Discovery.Provider != "openalex" || entry.Discovery.Mode != "monitor" || entry.Discovery.OpenAlexWorkID != "https://openalex.org/W3" || entry.Discovery.PublicationDate != "2026-06-08" || entry.Discovery.Since != "2026-01-01" || entry.Discovery.Query != "citation graphs" || !reflect.DeepEqual(entry.Discovery.Authors, []string{"https://orcid.org/0000-0002-1825-0097"}) {
		t.Fatalf("entry = %+v", entry)
	}
	resolve := newImportResolveCmd(&rootFlags{noCache: true, timeout: time.Second})
	resolve.SilenceErrors, resolve.SilenceUsage = true, true
	var resolved bytes.Buffer
	resolve.SetOut(&resolved)
	resolve.SetArgs([]string{path})
	if err := resolve.Execute(); err != nil {
		t.Fatalf("import resolve rejected monitor manifest: %v", err)
	}
	var refreshed importManifest
	if err := json.Unmarshal(resolved.Bytes(), &refreshed); err != nil {
		t.Fatal(err)
	}
	if len(refreshed.Entries) != 1 || refreshed.Entries[0].Status != "resolved" || refreshed.Entries[0].Item == nil || !reflect.DeepEqual(refreshed.Entries[0].Discovery, entry.Discovery) {
		t.Fatalf("resolved manifest lost import proposal or provenance: %+v", refreshed)
	}
}

func TestImportMonitorInvalidAuthorAndNoStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.json")
	for _, input := range []string{"Some Name", "https://orcid.org/A5023888391", "http://openalex.org/A5023888391"} {
		_, err := runImportMonitorTest(t, "--author", input, "--since", "2026-01-01", "--out", path)
		if err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "ORCID") || !strings.Contains(err.Error(), "OpenAlex author ID") {
			t.Fatalf("invalid author %q = %v (exit %d)", input, err, ExitCode(err))
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTIO_DATA_DIR", t.TempDir())
	_, err := runImportMonitorTest(t, "--query", "bibliometrics", "--since", "2026-01-01", "--out", path)
	if err == nil || ExitCode(err) != 9 || !strings.Contains(err.Error(), "zotio sync") {
		t.Fatalf("missing local store = %v (exit %d)", err, ExitCode(err))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing store published manifest: %v", err)
	}
}

func TestImportMonitorProviderFailureDoesNotPublish(t *testing.T) {
	seedImportDiscoverStore(t, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	withBase(t, &enrichOpenAlexBase, server.URL)
	path := filepath.Join(t.TempDir(), "m.json")
	_, err := runImportMonitorTest(t, "--query", "graphs", "--since", "2026-01-01", "--out", path)
	if err == nil || ExitCode(err) != 5 || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("provider failure = %v (exit %d)", err, ExitCode(err))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("provider failure published manifest: %v", err)
	}
	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":`))
	}))
	t.Cleanup(malformed.Close)
	withBase(t, &enrichOpenAlexBase, malformed.URL)
	_, err = runImportMonitorTest(t, "--query", "graphs", "--since", "2026-01-01", "--out", path)
	if err == nil || ExitCode(err) != 5 {
		t.Fatalf("malformed provider response = %v (exit %d)", err, ExitCode(err))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("malformed provider response published manifest: %v", err)
	}
}

func TestImportMonitorLimitPaginationAndEmpty(t *testing.T) {
	seedImportDiscoverStore(t, nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cursor") {
		case "*":
			_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]any{"next_cursor": "page2"}, "results": []map[string]string{{"id": "W1", "doi": "10.5000/one", "title": "One", "publication_date": "2026-02-02"}, {"id": "W2", "title": "No DOI", "publication_date": "2026-02-01"}}})
		case "page2":
			_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]any{"next_cursor": ""}, "results": []map[string]string{{"id": "W3", "doi": "10.5000/two", "title": "Two", "publication_date": "2026-01-30"}}})
		default:
			t.Errorf("unexpected cursor %s", r.URL.Query().Get("cursor"))
		}
	}))
	t.Cleanup(server.Close)
	withBase(t, &enrichOpenAlexBase, server.URL)
	path := filepath.Join(t.TempDir(), "m.json")
	report, err := runImportMonitorTest(t, "--author", "A5023888391", "--since", "2026-01-01", "--limit", "1", "--out", path)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := readImportManifest(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 1 || manifest.Entries[0].Identifier != "10.5000/one" || report.WorksSeen != 3 || report.SkippedWithoutIdentifier != 1 || !report.Truncated || !strings.Contains(report.TruncationReason, "--limit") {
		t.Fatalf("manifest = %+v; report = %+v", manifest, report)
	}
	// A valid empty feed still publishes the v2 artifact.
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	t.Cleanup(empty.Close)
	withBase(t, &enrichOpenAlexBase, empty.URL)
	emptyPath := filepath.Join(t.TempDir(), "empty.json")
	report, err = runImportMonitorTest(t, "--query", "no results", "--since", "2026-01-01", "--out", emptyPath)
	if err != nil || report.Emitted != 0 || report.Truncated {
		t.Fatalf("empty feed = %+v, %v", report, err)
	}
	manifest, err = readImportManifest(emptyPath, nil)
	if err != nil || manifest.Entries == nil || len(manifest.Entries) != 0 {
		t.Fatalf("empty manifest = %+v, %v", manifest, err)
	}
}

func TestImportMonitorPageCapReportsIncompleteFeed(t *testing.T) {
	seedImportDiscoverStore(t, nil)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		want := fmt.Sprintf("p%d", requests-1)
		if requests == 1 {
			want = "*"
		}
		if got := r.URL.Query().Get("cursor"); got != want {
			t.Errorf("cursor = %q on request %d, want %q", got, requests, want)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]string{"next_cursor": fmt.Sprintf("p%d", requests)}, "results": []map[string]string{{"id": fmt.Sprintf("W%d", requests), "doi": fmt.Sprintf("10.5000/%d", requests), "title": fmt.Sprintf("Work %d", requests), "publication_date": "2026-02-01"}}})
	}))
	t.Cleanup(server.Close)
	withBase(t, &enrichOpenAlexBase, server.URL)
	path := filepath.Join(t.TempDir(), "cap.json")
	report, err := runImportMonitorTest(t, "--query", "many works", "--since", "2026-01-01", "--out", path)
	if err != nil || requests != 20 || report.WorksSeen != requests || !report.Truncated || !strings.Contains(report.TruncationReason, "page cap") {
		t.Fatalf("page-cap report = %+v; requests=%d; err=%v", report, requests, err)
	}
}

func TestImportMonitorFreshOnRepeatedScheduledRun(t *testing.T) {
	seedImportDiscoverStore(t, nil)
	if newProviderJSONCache(false).store == nil {
		t.Fatal("the test requires the normal provider cache to be available")
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.URL.Query().Get("per_page"); got != "100" || r.URL.Query().Has("per-page") {
			t.Errorf("OpenAlex page size = %q; want per_page=100 without legacy per-page", got)
		}
		works := []map[string]string{{"id": "W1", "doi": "10.5000/first", "title": "First", "publication_date": "2026-09-01"}}
		if requests > 1 {
			works = append(works, map[string]string{"id": "W2", "doi": "10.5000/new", "title": "New", "publication_date": "2026-09-02"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": works})
	}))
	t.Cleanup(server.Close)
	withBase(t, &enrichOpenAlexBase, server.URL)
	path := filepath.Join(t.TempDir(), "feed.json")
	flags := &rootFlags{asJSON: true, timeout: time.Second} // Cache enabled, as on a scheduled run.
	args := []string{"--query", "graphs", "--since", "2026-08-01", "--out", path}
	first, err := runImportMonitorTestWithFlags(t, flags, args...)
	if err != nil || first.Emitted != 1 {
		t.Fatalf("first run = %+v, %v", first, err)
	}
	second, err := runImportMonitorTestWithFlags(t, flags, args...)
	if err != nil || second.Emitted != 2 || requests != 2 {
		t.Fatalf("second run = %+v, requests=%d, err=%v; want the newly indexed work", second, requests, err)
	}
	manifest, err := readImportManifest(path, nil)
	if err != nil || len(manifest.Entries) != 2 || manifest.Entries[0].Identifier != "10.5000/new" {
		t.Fatalf("second manifest = %+v, err=%v", manifest, err)
	}
}
