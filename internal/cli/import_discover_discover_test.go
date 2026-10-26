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
	"testing"
	"time"

	"zotio/internal/store"
)

// A partial provider outage must still publish the manifest and exit with the
// degraded code (13), naming the failed source in both human and JSON output.
func TestImportDiscoverDiscoverPartialFailureDegrades(t *testing.T) {
	seedImportDiscoverStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"SRC_OK","version":1,"data":{"key":"SRC_OK","itemType":"journalArticle","title":"Good Source","DOI":"10.9300/good","collections":["PART"],"dateModified":"2026-01-03T00:00:00Z"}}`),
		json.RawMessage(`{"key":"SRC_BAD","version":1,"data":{"key":"SRC_BAD","itemType":"journalArticle","title":"Bad Source","DOI":"10.9300/bad","collections":["PART"],"dateModified":"2026-01-02T00:00:00Z"}}`),
	})

	coci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoded, err := url.PathUnescape(r.URL.EscapedPath())
		if err != nil {
			t.Errorf("COCI path = %q: %v", r.URL.EscapedPath(), err)
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		switch decoded {
		case "/references/10.9300/good":
			_, _ = w.Write([]byte(`[{"cited":"10.9300/shared"},{"cited":"10.9300/only-good"}]`))
		default:
			http.Error(w, "provider outage", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(coci.Close)
	withBase(t, &collectionGapsOpenCitationsBase, coci.URL)

	ss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoded, err := url.PathUnescape(r.URL.EscapedPath())
		if err != nil {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		if strings.Contains(decoded, "10.9300/bad") {
			http.Error(w, "fallback down", http.StatusServiceUnavailable)
			return
		}
		if !strings.Contains(decoded, "10.9300/good") {
			http.Error(w, "unexpected DOI", http.StatusNotFound)
			return
		}
		http.Error(w, "unexpected S2 call", http.StatusInternalServerError)
	}))
	t.Cleanup(ss.Close)
	withBase(t, &enrichSemanticScholarBase, ss.URL)

	crossref := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoded, err := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/works/"))
		if err != nil {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		switch decoded {
		case "10.9300/bad", "10.9300/good":
			http.Error(w, "crossref down", http.StatusServiceUnavailable)
		case "10.9300/shared", "10.9300/only-good":
			writeCrossRefWork(t, w, decoded, "Candidate "+decoded)
		default:
			http.Error(w, "unexpected DOI", http.StatusNotFound)
		}
	}))
	t.Cleanup(crossref.Close)
	withBase(t, &enrichCrossRefBase, crossref.URL)

	manifestPath := filepath.Join(t.TempDir(), "partial.json")
	cmd := newImportDiscoverCmd(&rootFlags{asJSON: false, noCache: true, timeout: 5 * time.Second, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--scope", "collection:PART", "--output", manifestPath, "--limit", "10", "--min-count", "1"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("import discover with one failed source succeeded, want exit 13")
	}
	if got := ExitCode(err); got != 13 {
		t.Fatalf("exit code = %d, want 13 (degraded); err=%v", got, err)
	}
	body := out.String()
	if !strings.Contains(body, "failed=1") {
		t.Fatalf("human output %q does not report the failed count", body)
	}
	if !strings.Contains(body, "SRC_BAD") || !strings.Contains(body, "10.9300/bad") {
		t.Fatalf("human output %q does not name the failed source", body)
	}
	data, readErr := os.ReadFile(manifestPath)
	if readErr != nil {
		t.Fatalf("read manifest: %v", readErr)
	}
	var manifest importManifest
	if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.Entries) == 0 {
		t.Fatalf("manifest = %s err = %v, want the surviving candidates still published", string(data), err)
	}

	// Same outage under --json: the report carries the failed source and exit 13.
	manifestPath2 := filepath.Join(t.TempDir(), "partial-json.json")
	jcmd := newImportDiscoverCmd(&rootFlags{asJSON: true, noCache: true, timeout: 5 * time.Second, maxChanges: -1})
	jcmd.SilenceErrors, jcmd.SilenceUsage = true, true
	var jout bytes.Buffer
	jcmd.SetOut(&jout)
	jcmd.SetErr(&bytes.Buffer{})
	jcmd.SetArgs([]string{"--scope", "collection:PART", "--output", manifestPath2, "--limit", "10", "--min-count", "1"})
	jerr := jcmd.Execute()
	if jerr == nil || ExitCode(jerr) != 13 {
		t.Fatalf("json run err = %v, want exit 13", jerr)
	}
	var report importDiscoverReport
	if err := json.Unmarshal(jout.Bytes(), &report); err != nil {
		t.Fatalf("decode discover report %q: %v", jout.String(), err)
	}
	if report.Summary.ItemsFailed != 1 || !report.Summary.Incomplete {
		t.Fatalf("summary = %+v, want one failed source and incomplete", report.Summary)
	}
	found := false
	for _, source := range report.Summary.Sources {
		if source.Key == "SRC_BAD" && source.Error != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sources = %+v, want SRC_BAD with its provider error", report.Summary.Sources)
	}
}

// A COCI outage followed by an empty fallback answer must mark the source
// failed instead of scanning clean: the answer is missing, not empty.
func TestImportDiscoverDiscoverCociErrorEmptyFallbackFails(t *testing.T) {
	for _, direction := range []struct {
		name string
		dir  []string
		call func(ctx context.Context, client *http.Client, doi string, opts referenceFetchOptions) (referenceFetchResult, error)
	}{
		{name: "backward", dir: []string{importDiscoverDirectionBackward}, call: fetchOutgoingReferences},
		{name: "forward", dir: []string{importDiscoverDirectionForward}, call: fetchIncomingReferences},
	} {
		t.Run(direction.name, func(t *testing.T) {
			var fallback http.HandlerFunc
			if direction.name == "backward" {
				fallback = func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(`{"data":[]}`))
				}
				coci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Error(w, "COCI down", http.StatusServiceUnavailable)
				}))
				t.Cleanup(coci.Close)
				withBase(t, &collectionGapsOpenCitationsBase, coci.URL)
				ss := httptest.NewServer(fallback)
				t.Cleanup(ss.Close)
				withBase(t, &enrichSemanticScholarBase, ss.URL)
			} else {
				fallback = func(w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(r.URL.Path, "/works/https://doi.org/") {
						_ = json.NewEncoder(w).Encode(map[string]any{"id": "W-SOURCE"})
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"meta":    map[string]any{},
						"results": []map[string]string{},
					})
				}
				coci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Error(w, "COCI down", http.StatusServiceUnavailable)
				}))
				t.Cleanup(coci.Close)
				withBase(t, &collectionGapsOpenCitationsBase, coci.URL)
				oa := httptest.NewServer(fallback)
				t.Cleanup(oa.Close)
				withBase(t, &enrichOpenAlexBase, oa.URL)
			}
			_, err := direction.call(context.Background(), http.DefaultClient, "10.9300/source", referenceFetchOptions{Cache: newProviderJSONCache(true)})
			if err == nil || !strings.Contains(err.Error(), "503") {
				t.Fatalf("%s fetch err = %v, want the COCI HTTP 503", direction.name, err)
			}
			agg, aggErr := buildReferenceAggregateForDirections(context.Background(), http.DefaultClient, []referenceSourceItem{
				{Key: "SRC", Title: "Source", DOI: "10.9300/source"},
			}, direction.dir, referenceFetchOptions{Cache: newProviderJSONCache(true)})
			_ = agg
			if aggErr == nil {
				t.Fatalf("%s aggregate succeeded, want the all-failed error", direction.name)
			}
		})
	}
}

// The bare webpage fallback is for a reachable page without metadata, not for
// a page that answered 429/5xx/401/403 in any content type, and not for a DNS
// preflight that timed out.
func TestBuildImportItemFromURLDiscoverFailureContentTypes(t *testing.T) {
	allowPrivateMetadataProviderServers(t)
	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		body        string
	}{
		{name: "503 text/plain", status: http.StatusServiceUnavailable, contentType: "text/plain", body: "no healthy upstream"},
		{name: "429 json", status: http.StatusTooManyRequests, contentType: "application/json", body: `{"error":"throttled"}`},
		{name: "403 json", status: http.StatusForbidden, contentType: "application/json", body: `{"error":"denied"}`},
		{name: "401 json", status: http.StatusUnauthorized, contentType: "application/json", body: `{"error":"auth"}`},
		{name: "500 empty content type", status: http.StatusInternalServerError, contentType: "", body: "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			}))
			defer crsrv.Close()
			withBase(t, &enrichCrossRefBase, crsrv.URL)
			pagesrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer pagesrv.Close()
			item, _, err := buildImportItemFromURL(context.Background(), http.DefaultClient, pagesrv.URL+"/article")
			if err == nil || !strings.Contains(err.Error(), "HTTP") || ExitCode(err) != 5 {
				t.Fatalf("err = %v (exit %d), want an HTTP page error", err, ExitCode(err))
			}
			if item != nil {
				t.Fatalf("item = %v, want none", item)
			}
		})
	}
}

func TestBuildImportItemFromURLDiscoverDNSTimeoutIsFailure(t *testing.T) {
	oldAllow := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(false)
	oldLookup := publicOutboundIPLookup
	publicOutboundIPLookup = func(ctx context.Context, host string) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	t.Cleanup(func() {
		publicOutboundIPLookup = oldLookup
		allowPrivateOutboundForTests.Store(oldAllow)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	item, _, err := buildImportItemFromURL(ctx, http.DefaultClient, "https://example-timeout.test/article")
	if err == nil {
		t.Fatalf("item = %v, want a preflight timeout error not the fallback", item)
	}
	if item != nil {
		t.Fatalf("item = %v, want none", item)
	}
}

func TestBuildImportItemFromURLDiscoverLargePDFRemainsAbsent(t *testing.T) {
	allowPrivateMetadataProviderServers(t)
	body := strings.Repeat("x", (1<<20)+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	rawURL := server.URL + "/article.pdf"
	item, source, err := buildImportItemFromURL(context.Background(), server.Client(), rawURL)
	if err != nil {
		t.Fatalf("large PDF lookup failed: %v", err)
	}
	if source != "fallback (no metadata)" || item["itemType"] != "webpage" || item["url"] != rawURL {
		t.Fatalf("item=%v source=%q, want the bare webpage fallback", item, source)
	}
}

func TestImportDiscoverDiscoverMixedDirectionFailuresCountDistinctItems(t *testing.T) {
	dbPath := seedImportDiscoverForwardStore(t, []importDiscoverForwardSeedItem{
		{Key: "ABCD2345", Title: "First source", DOI: "10.9400/first"},
		{Key: "BCDE2345", Title: "Second source", DOI: "10.9400/second"},
	})
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSyncState("items", "", 1); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/references/10.9400/first" {
			_, _ = w.Write([]byte(`[{"cited":"10.9400/candidate"}]`))
			return
		}
		http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	withBase(t, &collectionGapsOpenCitationsBase, server.URL)
	withBase(t, &enrichSemanticScholarBase, server.URL)
	withBase(t, &enrichCrossRefBase, server.URL)
	withBase(t, &enrichOpenAlexBase, server.URL)

	cmd := newImportDiscoverCmd(&rootFlags{asJSON: true, noCache: true, timeout: time.Second, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--scope", "library", "--direction", "both", "--min-count", "2",
		"--output", filepath.Join(t.TempDir(), "mixed.json")})
	err = cmd.Execute()
	if err == nil || ExitCode(err) != 13 || !strings.Contains(err.Error(), "2 of 2 source item(s) failed") {
		t.Fatalf("err=%v, want exit 13 naming two distinct failed items", err)
	}
	var report importDiscoverReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("decode report %q: %v", output.String(), err)
	}
	if report.Summary.ItemsFailed != 2 || report.Summary.ItemsScanned != 0 {
		t.Fatalf("summary=%+v, want two failed items and zero fully scanned items", report.Summary)
	}
	failedDirections, successfulDirections := 0, 0
	for _, source := range report.Summary.Sources {
		if source.Error != "" {
			failedDirections++
		} else {
			successfulDirections++
		}
	}
	if failedDirections != 3 || successfulDirections != 1 {
		t.Fatalf("sources=%+v, want three failed directions and one successful direction", report.Summary.Sources)
	}
}
