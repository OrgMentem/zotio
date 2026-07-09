// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

type importDiscoverForwardSeedItem struct {
	Key          string
	Title        string
	DOI          string
	DateModified string
}

func seedImportDiscoverForwardStore(t *testing.T, items []importDiscoverForwardSeedItem) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0")
	t.Setenv("ZOTERO_QUEUE_TAG", "to-read")
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })

	dbPath := defaultDBPath("zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir store dir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	raw := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		dateModified := item.DateModified
		if dateModified == "" {
			dateModified = "2026-01-01T00:00:00Z"
		}
		body := map[string]any{
			"key":     item.Key,
			"version": 1,
			"data": map[string]any{
				"key":          item.Key,
				"itemType":     "journalArticle",
				"title":        item.Title,
				"DOI":          item.DOI,
				"dateModified": dateModified,
			},
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal seed item %s: %v", item.Key, err)
		}
		raw = append(raw, json.RawMessage(encoded))
	}
	if _, _, err := db.UpsertBatch("items", raw); err != nil {
		_ = db.Close()
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}
	return dbPath
}

func runImportDiscoverForwardCommand(t *testing.T, flags *rootFlags, args ...string) []byte {
	t.Helper()
	cmd := newImportDiscoverCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import discover %v: %v; stderr=%s stdout=%s", args, err, stderr.String(), stdout.String())
	}
	outPath := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--out" {
			outPath = args[i+1]
			break
		}
	}
	if outPath == "" {
		t.Fatalf("test bug: --out missing from args %v", args)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read manifest %s: %v", outPath, err)
	}
	return data
}

func decodeImportDiscoverForwardManifest(t *testing.T, data []byte) importManifest {
	t.Helper()
	var manifest importManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode manifest %q: %v", string(data), err)
	}
	return manifest
}

func importDiscoverForwardCrossRefServer(t *testing.T, titles map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.EscapedPath(), "/works/") {
			t.Errorf("CrossRef unexpected path %s", r.URL.String())
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		doi, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/works/"))
		if err != nil {
			t.Errorf("CrossRef unescape path %q: %v", r.URL.EscapedPath(), err)
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		title, ok := titles[doi]
		if !ok {
			t.Errorf("CrossRef lookup for unexpected DOI %q", doi)
			http.Error(w, "unexpected DOI", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]any{
				"DOI":   doi,
				"type":  "journal-article",
				"title": []string{title},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestImportDiscoverForwardCOCICitationsWritesForwardDiscoveryCounts(t *testing.T) {
	seedImportDiscoverForwardStore(t, []importDiscoverForwardSeedItem{
		{Key: "SRC1", Title: "Source One", DOI: "10.1100/source-one", DateModified: "2026-01-02T00:00:00Z"},
		{Key: "SRC2", Title: "Source Two", DOI: "10.1100/source-two", DateModified: "2026-01-01T00:00:00Z"},
	})

	coci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoded, err := url.PathUnescape(r.URL.EscapedPath())
		if err != nil {
			t.Errorf("COCI path = %q: %v", r.URL.EscapedPath(), err)
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		switch decoded {
		case "/citations/10.1100/source-one":
			_, _ = w.Write([]byte(`[{"citing":"10.1100/shared-citer"},{"citing":"10.1100/source-one-only"}]`))
		case "/citations/10.1100/source-two":
			_, _ = w.Write([]byte(`[{"citing":"https://doi.org/10.1100/shared-citer"}]`))
		default:
			t.Errorf("COCI unexpected path %q", decoded)
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(coci.Close)
	withBase(t, &collectionGapsOpenCitationsBase, coci.URL)

	crossref := importDiscoverForwardCrossRefServer(t, map[string]string{"10.1100/shared-citer": "Shared Citing Paper"})
	withBase(t, &enrichCrossRefBase, crossref.URL)

	outPath := filepath.Join(t.TempDir(), "manifest.json")
	data := runImportDiscoverForwardCommand(t, &rootFlags{asJSON: true, noCache: true, timeout: time.Second},
		"--scope", "library", "--out", outPath, "--direction", "forward", "--limit", "10", "--min-count", "2")
	manifest := decodeImportDiscoverForwardManifest(t, data)
	if len(manifest.Entries) != 1 {
		t.Fatalf("manifest entries = %+v, want exactly the shared forward candidate", manifest.Entries)
	}
	entry := manifest.Entries[0]
	if entry.Identifier != "10.1100/shared-citer" {
		t.Fatalf("identifier = %q, want normalized shared citing DOI", entry.Identifier)
	}
	if entry.Status != "resolved" || entry.Title != "Shared Citing Paper" {
		t.Fatalf("entry status/title = %q/%q, want resolved CrossRef title", entry.Status, entry.Title)
	}
	if entry.Discovery == nil {
		t.Fatal("entry discovery = nil")
	}
	if entry.Discovery.Direction != importDiscoverDirectionForward {
		t.Errorf("discovery direction = %q, want forward", entry.Discovery.Direction)
	}
	if entry.Discovery.Provider != providerCOCI {
		t.Errorf("discovery provider = %q, want coci", entry.Discovery.Provider)
	}
	if entry.Discovery.Count != 2 {
		t.Errorf("discovery count = %d, want 2 citing source counts", entry.Discovery.Count)
	}
	if !reflect.DeepEqual(entry.Discovery.CitedByKeys, []string{"SRC1", "SRC2"}) {
		t.Errorf("cited_by_keys = %v, want [SRC1 SRC2] in source ordering", entry.Discovery.CitedByKeys)
	}
}

func TestFetchIncomingReferencesOpenAlexFallbackPaginatesAndTruncatesAtCap(t *testing.T) {
	cases := []struct {
		name       string
		cociStatus int
		cociBody   string
		pages      map[string]struct {
			next string
			dois []string
		}
		wantDOIs      []string
		wantTruncated bool
		wantCursors   []string
	}{
		{
			name:       "empty COCI falls back through cursor pages and caps advertised overflow",
			cociStatus: http.StatusOK,
			cociBody:   `[]`,
			pages: func() map[string]struct {
				next string
				dois []string
			} {
				pages := make(map[string]struct {
					next string
					dois []string
				}, 5)
				cursor := "*"
				for page := 0; page < 5; page++ {
					dois := make([]string, 0, openAlexForwardPageSize)
					for i := 0; i < openAlexForwardPageSize; i++ {
						dois = append(dois, fmt.Sprintf("https://doi.org/10.4200/citing-%03d", page*openAlexForwardPageSize+i))
					}
					next := fmt.Sprintf("c%d", page+2)
					pages[cursor] = struct {
						next string
						dois []string
					}{next: next, dois: dois}
					cursor = next
				}
				return pages
			}(),
			wantDOIs:      []string{"10.4200/citing-000", "10.4200/citing-199", "10.4200/citing-200", "10.4200/citing-999"},
			wantTruncated: true,
			wantCursors:   []string{"*", "c2", "c3", "c4", "c5"},
		},
		{
			name:       "COCI error still falls back and collects multiple cursor pages",
			cociStatus: http.StatusServiceUnavailable,
			cociBody:   `temporary COCI outage`,
			pages: map[string]struct {
				next string
				dois []string
			}{
				"*":    {next: "next", dois: []string{"10.4300/first-page"}},
				"next": {next: "", dois: []string{"https://doi.org/10.4300/second-page"}},
			},
			wantDOIs:      []string{"10.4300/first-page", "10.4300/second-page"},
			wantTruncated: false,
			wantCursors:   []string{"*", "next"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			coci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				decoded, err := url.PathUnescape(r.URL.EscapedPath())
				if err != nil || decoded != "/citations/10.4200/source" {
					t.Errorf("COCI path = escaped %q decoded %q err %v", r.URL.EscapedPath(), decoded, err)
					http.Error(w, "unexpected path", http.StatusNotFound)
					return
				}
				w.WriteHeader(tc.cociStatus)
				_, _ = w.Write([]byte(tc.cociBody))
			}))
			t.Cleanup(coci.Close)
			withBase(t, &collectionGapsOpenCitationsBase, coci.URL)

			var gotCursors []string
			openAlex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/works/https://doi.org/"):
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "W-SOURCE"})
				case r.URL.Path == "/works":
					if got := r.URL.Query().Get("filter"); got != "cites:W-SOURCE" {
						t.Errorf("OpenAlex filter = %q, want cites:W-SOURCE", got)
					}
					if got := r.URL.Query().Get("per-page"); got != fmt.Sprintf("%d", openAlexForwardPageSize) {
						t.Errorf("OpenAlex per-page = %q, want %d", got, openAlexForwardPageSize)
					}
					cursor := r.URL.Query().Get("cursor")
					gotCursors = append(gotCursors, cursor)
					page, ok := tc.pages[cursor]
					if !ok {
						t.Errorf("OpenAlex unexpected cursor %q", cursor)
						http.Error(w, "unexpected cursor", http.StatusNotFound)
						return
					}
					results := make([]map[string]string, 0, len(page.dois))
					for _, doi := range page.dois {
						results = append(results, map[string]string{"doi": doi})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]any{"next_cursor": page.next}, "results": results})
				default:
					t.Errorf("OpenAlex unexpected path %s", r.URL.String())
					http.Error(w, "unexpected path", http.StatusNotFound)
				}
			}))
			t.Cleanup(openAlex.Close)
			withBase(t, &enrichOpenAlexBase, openAlex.URL)

			refs, err := fetchIncomingReferences(context.Background(), http.DefaultClient, "10.4200/source", referenceFetchOptions{Cache: newProviderJSONCache(true)})
			if err != nil {
				t.Fatalf("fetchIncomingReferences: %v", err)
			}
			if refs.Provider != providerOpenAlex {
				t.Fatalf("provider = %q, want openalex", refs.Provider)
			}
			if refs.Truncated != tc.wantTruncated {
				t.Fatalf("truncated = %v, want %v", refs.Truncated, tc.wantTruncated)
			}
			for _, want := range tc.wantDOIs {
				if !containsString(refs.DOIs, want) {
					t.Fatalf("OpenAlex DOIs missing %q; got len=%d first=%q last=%q", want, len(refs.DOIs), refs.DOIs[0], refs.DOIs[len(refs.DOIs)-1])
				}
			}
			if tc.wantTruncated && len(refs.DOIs) != openAlexForwardCap {
				t.Fatalf("DOI count = %d, want capped %d", len(refs.DOIs), openAlexForwardCap)
			}
			if !reflect.DeepEqual(gotCursors, tc.wantCursors) {
				t.Fatalf("OpenAlex cursors = %v, want %v", gotCursors, tc.wantCursors)
			}
		})
	}
}

func TestImportDiscoverBothMergesForwardAndBackwardCandidates(t *testing.T) {
	seedImportDiscoverForwardStore(t, []importDiscoverForwardSeedItem{
		{Key: "SRC", Title: "Bidirectional Source", DOI: "10.5100/source", DateModified: "2026-02-01T00:00:00Z"},
	})

	coci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoded, err := url.PathUnescape(r.URL.EscapedPath())
		if err != nil {
			t.Errorf("COCI path = %q: %v", r.URL.EscapedPath(), err)
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		switch decoded {
		case "/references/10.5100/source":
			_, _ = w.Write([]byte(`[{"cited":"10.5100/merged"},{"cited":"10.5100/backward-only"}]`))
		case "/citations/10.5100/source":
			_, _ = w.Write([]byte(`[{"citing":"https://doi.org/10.5100/merged"}]`))
		default:
			t.Errorf("COCI unexpected path %q", decoded)
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(coci.Close)
	withBase(t, &collectionGapsOpenCitationsBase, coci.URL)

	crossref := importDiscoverForwardCrossRefServer(t, map[string]string{
		"10.5100/merged":        "Merged Candidate",
		"10.5100/backward-only": "Backward Only Candidate",
	})
	withBase(t, &enrichCrossRefBase, crossref.URL)

	outPath := filepath.Join(t.TempDir(), "manifest.json")
	data := runImportDiscoverForwardCommand(t, &rootFlags{asJSON: true, noCache: true, timeout: time.Second},
		"--scope", "item:SRC", "--out", outPath, "--direction", "both", "--limit", "10", "--min-count", "1")
	manifest := decodeImportDiscoverForwardManifest(t, data)
	if len(manifest.Entries) != 2 {
		t.Fatalf("manifest entries = %+v, want merged and backward-only candidates", manifest.Entries)
	}
	byDOI := map[string]importManifestEntry{}
	for _, entry := range manifest.Entries {
		byDOI[entry.Identifier] = entry
	}
	merged := byDOI["10.5100/merged"]
	if merged.Discovery == nil {
		t.Fatal("merged discovery = nil")
	}
	if merged.Discovery.Direction != importDiscoverDirectionBoth || merged.Discovery.Count != 2 {
		t.Fatalf("merged discovery = %+v, want direction both and summed count 2", merged.Discovery)
	}
	if merged.Discovery.Provider != providerCOCI {
		t.Errorf("merged provider = %q, want coci", merged.Discovery.Provider)
	}
	backwardOnly := byDOI["10.5100/backward-only"]
	if backwardOnly.Discovery == nil {
		t.Fatal("backward-only discovery = nil")
	}
	if backwardOnly.Discovery.Direction != importDiscoverDirectionBackward || backwardOnly.Discovery.Count != 1 {
		t.Fatalf("backward-only discovery = %+v, want direction backward and count 1", backwardOnly.Discovery)
	}
}

func TestImportDiscoverBackwardOutputBytesUnchangedByDirectionFlag(t *testing.T) {
	seedImportDiscoverForwardStore(t, []importDiscoverForwardSeedItem{
		{Key: "SRC1", Title: "Backward Source One", DOI: "10.6100/source-one", DateModified: "2026-03-02T00:00:00Z"},
		{Key: "SRC2", Title: "Backward Source Two", DOI: "10.6100/source-two", DateModified: "2026-03-01T00:00:00Z"},
	})

	coci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoded, err := url.PathUnescape(r.URL.EscapedPath())
		if err != nil {
			t.Errorf("COCI path = %q: %v", r.URL.EscapedPath(), err)
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		switch decoded {
		case "/references/10.6100/source-one":
			_, _ = w.Write([]byte(`[{"cited":"10.6100/beta"},{"cited":"10.6100/alpha"}]`))
		case "/references/10.6100/source-two":
			_, _ = w.Write([]byte(`[{"cited":"10.6100/alpha"},{"cited":"10.6100/gamma"}]`))
		default:
			t.Errorf("COCI unexpected path %q", decoded)
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(coci.Close)
	withBase(t, &collectionGapsOpenCitationsBase, coci.URL)

	crossref := importDiscoverForwardCrossRefServer(t, map[string]string{
		"10.6100/alpha": "Alpha Backward Candidate",
		"10.6100/beta":  "Beta Backward Candidate",
		"10.6100/gamma": "Gamma Backward Candidate",
	})
	withBase(t, &enrichCrossRefBase, crossref.URL)

	flags := &rootFlags{asJSON: true, noCache: true, timeout: time.Second}
	defaultBytes := runImportDiscoverForwardCommand(t, flags,
		"--scope", "library", "--out", filepath.Join(t.TempDir(), "default.json"), "--limit", "10", "--min-count", "1")
	explicitBackwardBytes := runImportDiscoverForwardCommand(t, flags,
		"--scope", "library", "--out", filepath.Join(t.TempDir(), "backward.json"), "--direction", "backward", "--limit", "10", "--min-count", "1")
	if !bytes.Equal(defaultBytes, explicitBackwardBytes) {
		t.Fatalf("default backward manifest bytes changed with explicit --direction backward\ndefault: %s\nexplicit: %s", defaultBytes, explicitBackwardBytes)
	}
}

func TestImportDiscoverForwardProviderGETsUseCacheOnRepeatRun(t *testing.T) {
	seedImportDiscoverForwardStore(t, []importDiscoverForwardSeedItem{
		{Key: "SRC", Title: "Cache Source", DOI: "10.7100/source", DateModified: "2026-04-01T00:00:00Z"},
	})

	var phase atomic.Int32
	var secondRunRequests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if phase.Load() == 2 {
			secondRunRequests.Add(1)
		}
		decoded, err := url.PathUnescape(r.URL.EscapedPath())
		if err != nil {
			t.Errorf("provider path = %q: %v", r.URL.EscapedPath(), err)
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		switch decoded {
		case "/citations/10.7100/source":
			_, _ = w.Write([]byte(`[{"citing":"10.7100/cached-citer"}]`))
		case "/works/10.7100/cached-citer":
			_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"DOI": "10.7100/cached-citer", "type": "journal-article", "title": []string{"Cached Citer"}}})
		default:
			t.Errorf("provider unexpected path %q", decoded)
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(provider.Close)
	withBase(t, &collectionGapsOpenCitationsBase, provider.URL)
	withBase(t, &enrichCrossRefBase, provider.URL)

	flags := &rootFlags{asJSON: true, timeout: time.Second}
	phase.Store(1)
	first := decodeImportDiscoverForwardManifest(t, runImportDiscoverForwardCommand(t, flags,
		"--scope", "item:SRC", "--out", filepath.Join(t.TempDir(), "first.json"), "--direction", "forward", "--limit", "10", "--min-count", "1"))
	if len(first.Entries) != 1 || first.Entries[0].Identifier != "10.7100/cached-citer" {
		t.Fatalf("first manifest = %+v, want cached-citer candidate", first.Entries)
	}

	phase.Store(2)
	second := decodeImportDiscoverForwardManifest(t, runImportDiscoverForwardCommand(t, flags,
		"--scope", "item:SRC", "--out", filepath.Join(t.TempDir(), "second.json"), "--direction", "forward", "--limit", "10", "--min-count", "1"))
	if len(second.Entries) != 1 || second.Entries[0].Identifier != "10.7100/cached-citer" {
		t.Fatalf("second manifest = %+v, want cached-citer candidate", second.Entries)
	}
	if !reflect.DeepEqual(first.Entries, second.Entries) {
		t.Fatalf("cached repeat manifest entries = %+v, want first run %+v", second.Entries, first.Entries)
	}
	if got := secondRunRequests.Load(); got != 0 {
		t.Fatalf("second run provider GETs = %d, want 0 cache hits", got)
	}
}
