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
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/store"
)

func TestImportDiscoverBackwardManifestSkipsHeldAndTitleDuplicate(t *testing.T) {
	seedImportDiscoverStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"SRC1","version":1,"data":{"key":"SRC1","itemType":"journalArticle","title":"Scope One","DOI":"10.5000/source1","collections":["DISC"],"dateModified":"2026-01-03T00:00:00Z"}}`),
		json.RawMessage(`{"key":"SRC2","version":1,"data":{"key":"SRC2","itemType":"journalArticle","title":"Scope Two","DOI":"10.5000/source2","collections":["DISC"],"dateModified":"2026-01-02T00:00:00Z"}}`),
		json.RawMessage(`{"key":"HAVE","version":1,"data":{"key":"HAVE","itemType":"journalArticle","title":"Already Held","DOI":"10.1000/existing","dateModified":"2026-01-01T00:00:00Z"}}`),
		json.RawMessage(`{"key":"TITLE","version":1,"data":{"key":"TITLE","itemType":"journalArticle","title":"Candidate Y Title","DOI":"10.9999/title-holder","dateModified":"2026-01-01T00:00:00Z"}}`),
	})
	withImportDiscoverProviderMocks(t,
		map[string][]string{
			"10.5000/source1": {"10.1000/existing", "10.1000/titledup", "10.1000/alpha", "10.1000/beta", "10.1000/alpha"},
			"10.5000/source2": {"10.1000/existing", "10.1000/titledup", "10.1000/alpha", "10.1000/beta"},
		},
		map[string]string{
			"10.1000/alpha":    "Alpha Candidate",
			"10.1000/beta":     "Beta Candidate",
			"10.1000/titledup": "Candidate Y Title",
		},
	)

	manifestPath := filepath.Join(t.TempDir(), "discover.json")
	manifest, report := runImportDiscoverTestCmd(t, &rootFlags{asJSON: true, noCache: true, timeout: 5 * time.Second},
		"--scope", "collection:DISC", "--out", manifestPath, "--limit", "10", "--min-count", "2")

	if manifest.SchemaVersion != 2 {
		t.Fatalf("schema_version = %d, want 2", manifest.SchemaVersion)
	}
	if report.Summary.ItemsScanned != 2 || report.Summary.ReferencesSeen != 9 || report.Summary.UniqueCitedDOIs != 4 {
		t.Fatalf("summary = %+v, want two sources, nine fetched references, four unique DOIs", report.Summary)
	}
	if report.Summary.SkippedAlreadyInLibrary != 1 || report.Summary.SkippedTitleDuplicate != 1 {
		t.Fatalf("skip summary = %+v, want one DOI skip and one title skip", report.Summary)
	}

	byDOI := importDiscoverEntriesByDOI(manifest.Entries)
	if _, ok := byDOI["10.1000/existing"]; ok {
		t.Fatalf("already-held DOI was emitted: %+v", byDOI["10.1000/existing"])
	}
	for _, doi := range []string{"10.1000/alpha", "10.1000/beta"} {
		entry, ok := byDOI[doi]
		if !ok {
			t.Fatalf("missing create entry for %s in %+v", doi, manifest.Entries)
		}
		if entry.Action != "create" || entry.Status != "resolved" || entry.Item == nil {
			t.Fatalf("entry %s = %+v, want resolved create", doi, entry)
		}
		if entry.Discovery == nil || entry.Discovery.Direction != "backward" || entry.Discovery.Count != 2 {
			t.Fatalf("entry %s discovery = %+v, want backward count 2", doi, entry.Discovery)
		}
		if !reflect.DeepEqual(entry.Discovery.CitedByKeys, []string{"SRC1", "SRC2"}) {
			t.Fatalf("entry %s cited_by_keys = %v, want [SRC1 SRC2]", doi, entry.Discovery.CitedByKeys)
		}
	}
	dup := byDOI["10.1000/titledup"]
	if dup.Action != "skip" || dup.Item != nil || dup.Note != "title already exists in library" {
		t.Fatalf("title duplicate entry = %+v, want skip with note and no create item", dup)
	}
	if dup.Discovery == nil || dup.Discovery.Direction != "backward" || dup.Discovery.Count != 2 {
		t.Fatalf("title duplicate discovery = %+v, want backward count 2", dup.Discovery)
	}
}

func TestImportDiscoverMinCountAndLimit(t *testing.T) {
	seedImportDiscoverStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"SRC1","version":1,"data":{"key":"SRC1","itemType":"journalArticle","title":"Scope One","DOI":"10.6000/source1","collections":["LIMIT"],"dateModified":"2026-01-03T00:00:00Z"}}`),
		json.RawMessage(`{"key":"SRC2","version":1,"data":{"key":"SRC2","itemType":"journalArticle","title":"Scope Two","DOI":"10.6000/source2","collections":["LIMIT"],"dateModified":"2026-01-02T00:00:00Z"}}`),
	})
	withImportDiscoverProviderMocks(t,
		map[string][]string{
			"10.6000/source1": {"10.2000/alpha", "10.2000/beta", "10.2000/single"},
			"10.6000/source2": {"10.2000/alpha", "10.2000/beta"},
		},
		map[string]string{
			"10.2000/alpha": "Alpha Limited",
			"10.2000/beta":  "Beta Limited",
		},
	)

	manifestPath := filepath.Join(t.TempDir(), "discover.json")
	manifest, report := runImportDiscoverTestCmd(t, &rootFlags{asJSON: true, noCache: true, timeout: 5 * time.Second},
		"--scope", "collection:LIMIT", "--out", manifestPath, "--limit", "1", "--min-count", "2")

	if len(manifest.Entries) != 1 {
		t.Fatalf("entries = %+v, want exactly one due to --limit", manifest.Entries)
	}
	if manifest.Entries[0].Identifier != "10.2000/alpha" {
		t.Fatalf("first entry DOI = %q, want ranked DOI 10.2000/alpha", manifest.Entries[0].Identifier)
	}
	if report.Summary.Entries != 1 || report.Summary.Candidates != 2 {
		t.Fatalf("summary = %+v, want one emitted entry and two min-count candidates before limit stop", report.Summary)
	}
	if _, ok := importDiscoverEntriesByDOI(manifest.Entries)["10.2000/single"]; ok {
		t.Fatalf("single-source DOI passed --min-count: %+v", manifest.Entries)
	}
}

func TestImportManifestV2DiscoveryCompat(t *testing.T) {
	var buf bytes.Buffer
	m := importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Entries: []importManifestEntry{{
			Action: "create",
			Status: "resolved",
			Title:  "Discovery Create",
			Item: map[string]any{
				"itemType": "journalArticle",
				"title":    "Discovery Create",
			},
			Discovery: &importDiscovery{Direction: "backward", Count: 2, CitedByKeys: []string{"SRC1", "SRC2"}},
		}},
	}
	if err := writeImportManifest(&buf, m); err != nil {
		t.Fatalf("writeImportManifest: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("decode written manifest: %v", err)
	}
	if raw["schema_version"] != float64(2) {
		t.Fatalf("schema_version = %v, want 2", raw["schema_version"])
	}

	v1Path := filepath.Join(t.TempDir(), "v1.json")
	v1Fixture := `{"schema_version":1,"dir":"/tmp","entries":[{"path":"/tmp/p.pdf","classification":"new","action":"create","identifier_type":"doi","identifier":"10.1/v1","title":"V1 Paper","item":{"itemType":"journalArticle","title":"V1 Paper"},"status":"resolved"}]}`
	if err := os.WriteFile(v1Path, []byte(v1Fixture), 0o600); err != nil {
		t.Fatalf("write v1 fixture: %v", err)
	}
	got, err := readImportManifest(v1Path)
	if err != nil {
		t.Fatalf("readImportManifest(v1): %v", err)
	}
	if got.SchemaVersion != 1 || got.Entries[0].Identifier != "10.1/v1" || got.Entries[0].Discovery != nil {
		t.Fatalf("v1 manifest = %+v, want unchanged v1 entry without discovery", got)
	}

	manifestPath := writeImportApplyTestManifest(t, m)
	env, stderr, err := runImportApplyTestCmd(t, []string{manifestPath})
	if err != nil {
		t.Fatalf("import apply preview with discovery: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "preview" || env.Plan.Summary.Planned != 1 || len(env.Plan.Operations) != 1 || env.Plan.Operations[0].Kind != "import_create" {
		t.Fatalf("env = %+v ops=%+v, want one planned create preview", env, env.Plan.Operations)
	}
}

func TestImportDiscoverProviderCacheReusesAndNoCacheBypasses(t *testing.T) {
	seedImportDiscoverStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"SRC1","version":1,"data":{"key":"SRC1","itemType":"journalArticle","title":"Scope One","DOI":"10.8000/source1","collections":["CACHE"],"dateModified":"2026-01-03T00:00:00Z"}}`),
		json.RawMessage(`{"key":"SRC2","version":1,"data":{"key":"SRC2","itemType":"journalArticle","title":"Scope Two","DOI":"10.8000/source2","collections":["CACHE"],"dateModified":"2026-01-02T00:00:00Z"}}`),
	})
	counters := withImportDiscoverProviderMocks(t,
		map[string][]string{
			"10.8000/source1": {"10.3000/alpha", "10.3000/beta"},
			"10.8000/source2": {"10.3000/alpha", "10.3000/beta"},
		},
		map[string]string{
			"10.3000/alpha": "Alpha Cached",
			"10.3000/beta":  "Beta Cached",
		},
	)

	runImportDiscoverTestCmd(t, &rootFlags{asJSON: true, timeout: 5 * time.Second},
		"--scope", "collection:CACHE", "--out", filepath.Join(t.TempDir(), "first.json"), "--limit", "10", "--min-count", "2")
	firstCOCI, firstCrossRef, firstS2 := counters.coci.Load(), counters.crossref.Load(), counters.semanticScholar.Load()
	if firstCOCI == 0 || firstCrossRef == 0 || firstS2 != 0 {
		t.Fatalf("first counters coci=%d crossref=%d s2=%d, want COCI/CrossRef hits and no S2", firstCOCI, firstCrossRef, firstS2)
	}

	runImportDiscoverTestCmd(t, &rootFlags{asJSON: true, timeout: 5 * time.Second},
		"--scope", "collection:CACHE", "--out", filepath.Join(t.TempDir(), "second.json"), "--limit", "10", "--min-count", "2")
	if got := []int64{counters.coci.Load(), counters.crossref.Load(), counters.semanticScholar.Load()}; !reflect.DeepEqual(got, []int64{firstCOCI, firstCrossRef, firstS2}) {
		t.Fatalf("cached second run counters = %v, want unchanged [%d %d %d]", got, firstCOCI, firstCrossRef, firstS2)
	}

	runImportDiscoverTestCmd(t, &rootFlags{asJSON: true, noCache: true, timeout: 5 * time.Second},
		"--scope", "collection:CACHE", "--out", filepath.Join(t.TempDir(), "third.json"), "--limit", "10", "--min-count", "2")
	if counters.coci.Load() <= firstCOCI || counters.crossref.Load() <= firstCrossRef {
		t.Fatalf("--no-cache counters coci=%d crossref=%d, want increments beyond coci=%d crossref=%d", counters.coci.Load(), counters.crossref.Load(), firstCOCI, firstCrossRef)
	}
}

func TestImportDiscoverSemanticScholarTruncationInJSONSummary(t *testing.T) {
	seedImportDiscoverStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"SRC1","version":1,"data":{"key":"SRC1","itemType":"journalArticle","title":"Scope One","DOI":"10.7000/source","collections":["TRUNC"],"dateModified":"2026-01-03T00:00:00Z"}}`),
	})

	coci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(coci.Close)
	withBase(t, &collectionGapsOpenCitationsBase, coci.URL)

	ss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoded, err := url.PathUnescape(r.URL.EscapedPath())
		if err != nil || decoded != "/paper/DOI:10.7000/source/references" {
			t.Errorf("Semantic Scholar path = escaped %q decoded %q err %v", r.URL.EscapedPath(), decoded, err)
			http.Error(w, "unexpected Semantic Scholar path", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"next": 1000,
			"data": []map[string]any{{
				"citedPaper": map[string]any{
					"externalIds": map[string]string{"DOI": "10.7000/truncated"},
					"title":       "Truncated Candidate",
				},
			}},
		})
	}))
	t.Cleanup(ss.Close)
	withBase(t, &enrichSemanticScholarBase, ss.URL)

	crossref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoded, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/works/"))
		if err != nil || decoded != "10.7000/truncated" {
			t.Errorf("CrossRef path = %q decoded %q err %v", r.URL.EscapedPath(), decoded, err)
			http.Error(w, "unexpected CrossRef DOI", http.StatusNotFound)
			return
		}
		writeCrossRefWork(t, w, decoded, "Truncated Candidate")
	}))
	t.Cleanup(crossref.Close)
	withBase(t, &enrichCrossRefBase, crossref.URL)

	_, report := runImportDiscoverTestCmd(t, &rootFlags{asJSON: true, noCache: true, timeout: 5 * time.Second},
		"--scope", "collection:TRUNC", "--out", filepath.Join(t.TempDir(), "truncated.json"), "--limit", "10", "--min-count", "1")
	if len(report.Summary.Sources) != 1 {
		t.Fatalf("sources = %+v, want one source", report.Summary.Sources)
	}
	source := report.Summary.Sources[0]
	if !source.Truncated || source.Refs != 1 || source.Key != "SRC1" || source.DOI != "10.7000/source" {
		t.Fatalf("source summary = %+v, want truncated SRC1 with one ref", source)
	}
}

func TestBuildReferenceAggregateForDirectionsDegradesPerSourceProviderError(t *testing.T) {
	coci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoded, err := url.PathUnescape(r.URL.EscapedPath())
		if err != nil {
			t.Errorf("COCI path = %q: %v", r.URL.EscapedPath(), err)
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		switch decoded {
		case "/references/10.9100/failing":
			http.Error(w, "oversized provider response", http.StatusInternalServerError)
		case "/references/10.9100/success":
			_, _ = w.Write([]byte(`[{"cited":"10.9100/candidate"}]`))
		default:
			t.Errorf("COCI unexpected path %q", decoded)
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(coci.Close)
	withBase(t, &collectionGapsOpenCitationsBase, coci.URL)

	semanticScholar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "semantic scholar also unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(semanticScholar.Close)
	withBase(t, &enrichSemanticScholarBase, semanticScholar.URL)

	agg, err := buildReferenceAggregateForDirections(context.Background(), http.DefaultClient, []referenceSourceItem{
		{Key: "SRC_FAIL", Title: "Failing Source", DOI: "10.9100/failing"},
		{Key: "SRC_OK", Title: "Successful Source", DOI: "10.9100/success"},
	}, []string{importDiscoverDirectionBackward}, referenceFetchOptions{CountUniquePerSource: true, Cache: newProviderJSONCache(true)})
	if err != nil {
		t.Fatalf("buildReferenceAggregateForDirections returned error for one failed source: %v", err)
	}
	if len(agg.Sources) != 2 {
		t.Fatalf("sources = %+v, want failed and successful source summaries", agg.Sources)
	}
	if agg.Sources[0].Key != "SRC_FAIL" || agg.Sources[0].Error == "" {
		t.Fatalf("first source summary = %+v, want recorded provider error", agg.Sources[0])
	}
	if agg.Sources[1].Key != "SRC_OK" || agg.Sources[1].Error != "" || agg.Sources[1].Refs != 1 {
		t.Fatalf("second source summary = %+v, want successful one-ref source", agg.Sources[1])
	}
	if len(agg.Candidates) != 1 {
		t.Fatalf("candidates = %+v, want only source-two candidate", agg.Candidates)
	}
	candidate := agg.Candidates[0]
	if candidate.DOI != "10.9100/candidate" || candidate.Count != 1 || !reflect.DeepEqual(candidate.CitedByKeys, []string{"SRC_OK"}) {
		t.Fatalf("candidate = %+v, want 10.9100/candidate cited by SRC_OK", candidate)
	}
}

func TestImportDiscoverCommandErrorsWhenAllSourcesFail(t *testing.T) {
	seedImportDiscoverStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"SRC_FAIL1","version":1,"data":{"key":"SRC_FAIL1","itemType":"journalArticle","title":"Failing Source One","DOI":"10.9200/failing-one","collections":["FAIL"],"dateModified":"2026-01-03T00:00:00Z"}}`),
		json.RawMessage(`{"key":"SRC_FAIL2","version":1,"data":{"key":"SRC_FAIL2","itemType":"journalArticle","title":"Failing Source Two","DOI":"10.9200/failing-two","collections":["FAIL"],"dateModified":"2026-01-02T00:00:00Z"}}`),
	})

	coci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "provider outage", http.StatusInternalServerError)
	}))
	t.Cleanup(coci.Close)
	withBase(t, &collectionGapsOpenCitationsBase, coci.URL)

	semanticScholar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "provider outage", http.StatusInternalServerError)
	}))
	t.Cleanup(semanticScholar.Close)
	withBase(t, &enrichSemanticScholarBase, semanticScholar.URL)

	cmd := newImportDiscoverCmd(&rootFlags{asJSON: true, noCache: true, timeout: time.Second})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--scope", "collection:FAIL", "--out", filepath.Join(t.TempDir(), "manifest.json"), "--limit", "10", "--min-count", "1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("import discover succeeded when every source provider fetch failed")
	}
}

type importDiscoverProviderCounters struct {
	coci            atomic.Int64
	crossref        atomic.Int64
	semanticScholar atomic.Int64
}

func seedImportDiscoverStore(t *testing.T, items []json.RawMessage) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "zotio.toml"))
	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		_ = db.Close()
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}
}

func withImportDiscoverProviderMocks(t *testing.T, references map[string][]string, works map[string]string) *importDiscoverProviderCounters {
	t.Helper()
	counters := &importDiscoverProviderCounters{}

	coci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counters.coci.Add(1)
		decoded, err := url.PathUnescape(r.URL.EscapedPath())
		if err != nil || !strings.HasPrefix(decoded, "/references/") {
			t.Errorf("COCI path = escaped %q decoded %q err %v", r.URL.EscapedPath(), decoded, err)
			http.Error(w, "unexpected COCI path", http.StatusNotFound)
			return
		}
		doi := strings.TrimPrefix(decoded, "/references/")
		refs, ok := references[doi]
		if !ok {
			t.Errorf("unexpected COCI DOI %q", doi)
			http.Error(w, "unexpected DOI", http.StatusNotFound)
			return
		}
		rows := make([]map[string]string, 0, len(refs))
		for _, ref := range refs {
			rows = append(rows, map[string]string{"cited": ref})
		}
		if err := json.NewEncoder(w).Encode(rows); err != nil {
			t.Errorf("encode COCI response: %v", err)
		}
	}))
	t.Cleanup(coci.Close)
	withBase(t, &collectionGapsOpenCitationsBase, coci.URL)

	ss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counters.semanticScholar.Add(1)
		t.Errorf("Semantic Scholar should not be queried when COCI has references; got %s", r.URL.String())
		http.Error(w, "unexpected Semantic Scholar call", http.StatusInternalServerError)
	}))
	t.Cleanup(ss.Close)
	withBase(t, &enrichSemanticScholarBase, ss.URL)

	crossref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counters.crossref.Add(1)
		decoded, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/works/"))
		if err != nil {
			t.Errorf("CrossRef path = %q: %v", r.URL.EscapedPath(), err)
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		title, ok := works[decoded]
		if !ok {
			t.Errorf("unexpected CrossRef DOI %q", decoded)
			http.Error(w, "unexpected DOI", http.StatusNotFound)
			return
		}
		writeCrossRefWork(t, w, decoded, title)
	}))
	t.Cleanup(crossref.Close)
	withBase(t, &enrichCrossRefBase, crossref.URL)

	return counters
}

func runImportDiscoverTestCmd(t *testing.T, flags *rootFlags, args ...string) (importManifest, importDiscoverReport) {
	t.Helper()
	cmd := newImportDiscoverCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import discover: %v; stderr=%s; stdout=%s", err, errOut.String(), out.String())
	}
	var report importDiscoverReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode discover report %q: %v", out.String(), err)
	}
	manifestPath := ""
	for i := range len(args) - 1 {
		if args[i] == "--out" {
			manifestPath = args[i+1]
			break
		}
	}
	if manifestPath == "" {
		t.Fatalf("test command args missing --out: %v", args)
	}
	manifest, err := readImportManifest(manifestPath)
	if err != nil {
		t.Fatalf("read discover manifest: %v", err)
	}
	return manifest, report
}

func importDiscoverEntriesByDOI(entries []importManifestEntry) map[string]importManifestEntry {
	out := make(map[string]importManifestEntry, len(entries))
	for _, entry := range entries {
		out[entry.Identifier] = entry
	}
	return out
}

func writeCrossRefWork(t *testing.T, w http.ResponseWriter, doi, title string) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{
		"title":           []string{title},
		"DOI":             doi,
		"type":            "journal-article",
		"published":       map[string]any{"date-parts": [][]int{{2026}}},
		"container-title": []string{"Journal of Tests"},
	}}); err != nil {
		t.Errorf("encode CrossRef response: %v", err)
	}
}
