// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//
// Pins DOI resolution across both registration agencies. The regression these
// guard is dev/field-report-2026-08-22-papio-arxiv.md finding 1: every
// 10.48550/arXiv.* DOI resolved to "unresolved" because the import path asked
// CrossRef only, and that prefix is registered with DataCite. The fixture
// bodies below are trimmed copies of the live responses measured on
// 2026-08-22 for 10.48550/arXiv.2605.10930.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const dataCiteArxivBody = `{"data":{"attributes":{
	"doi":"10.48550/arxiv.2605.10930",
	"titles":[{"title":"Evaluating the False Trust Engendered by LLM Explanations"}],
	"creators":[
		{"name":"Palod, Vardhan","nameType":"Personal","givenName":"Vardhan","familyName":"Palod"},
		{"name":"Kambhampati, Subbarao","nameType":"Personal","givenName":"Subbarao","familyName":"Kambhampati"}],
	"publisher":"arXiv",
	"published":"2026",
	"publicationYear":2026,
	"types":{"resourceTypeGeneral":"Preprint"},
	"url":"https://arxiv.org/abs/2605.10930",
	"descriptions":[{"descriptionType":"Abstract","description":"An abstract."}]
}}}`

// registryServers stands up a CrossRef and a DataCite seam. crossRefStatus
// lets a test choose how CrossRef declines: 404 is "no such record", 503 is
// "cannot answer", and the two must route differently.
func registryServers(t *testing.T, crossRefStatus int, dataCiteBody string) (crossRefHits, dataCiteHits *int) {
	t.Helper()
	var crHits, dcHits int

	crossRef := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		crHits++
		http.Error(w, "Resource not found.", crossRefStatus)
	}))
	t.Cleanup(crossRef.Close)
	withBase(t, &enrichCrossRefBase, crossRef.URL)

	dataCite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dcHits++
		if !strings.HasPrefix(r.URL.EscapedPath(), "/dois/") {
			t.Errorf("DataCite path = %q, want /dois/<doi>", r.URL.EscapedPath())
		}
		if dataCiteBody == "" {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(dataCiteBody))
	}))
	t.Cleanup(dataCite.Close)
	withBase(t, &enrichDataCiteBase, dataCite.URL)

	return &crHits, &dcHits
}

// TestFetchDOIItemFallsBackToDataCiteOnCrossRefMiss is the field report's
// headline: the DOI is well-formed and registered, only the registry was wrong.
func TestFetchDOIItemFallsBackToDataCiteOnCrossRefMiss(t *testing.T) {
	crHits, dcHits := registryServers(t, http.StatusNotFound, dataCiteArxivBody)

	item, err := fetchDOIItemWithCache(context.Background(), http.DefaultClient, "10.48550/arXiv.2605.10930", nil)
	if err != nil {
		t.Fatalf("fetchDOIItemWithCache: %v", err)
	}
	if *crHits != 1 || *dcHits != 1 {
		t.Fatalf("crossref hits = %d, datacite hits = %d, want 1 and 1", *crHits, *dcHits)
	}
	if item["itemType"] != "preprint" {
		t.Errorf("itemType = %v, want preprint", item["itemType"])
	}
	if item["title"] != "Evaluating the False Trust Engendered by LLM Explanations" {
		t.Errorf("title = %v", item["title"])
	}
	// The caller's spelling survives DataCite's lower-casing, so a manifest
	// entry's Identifier still equals the resolved item's DOI.
	if item["DOI"] != "10.48550/arXiv.2605.10930" {
		t.Errorf("DOI = %v, want the requested spelling", item["DOI"])
	}
	if item["abstractNote"] != "An abstract." || item["date"] != "2026" {
		t.Errorf("abstract/date = %v/%v", item["abstractNote"], item["date"])
	}
	// arXiv self-DOIs must land on the same fields as an arXiv-ID import.
	if item["archiveID"] != "arXiv:2605.10930" || item["repository"] != "arXiv" || item["extra"] != "arXiv: 2605.10930" {
		t.Errorf("arxiv fields = %v/%v/%v", item["archiveID"], item["repository"], item["extra"])
	}
	creators, ok := item["creators"].([]map[string]any)
	if !ok || len(creators) != 2 {
		t.Fatalf("creators = %#v, want 2", item["creators"])
	}
	if creators[0]["lastName"] != "Palod" || creators[0]["firstName"] != "Vardhan" {
		t.Errorf("creator[0] = %#v", creators[0])
	}
}

// TestFetchDOIItemDoesNotFallBackWhenCrossRefIsUnavailable keeps a transient
// CrossRef outage from being reported as a DOI that does not exist. DataCite
// must not be asked a question CrossRef never answered.
func TestFetchDOIItemDoesNotFallBackWhenCrossRefIsUnavailable(t *testing.T) {
	crHits, dcHits := registryServers(t, http.StatusServiceUnavailable, dataCiteArxivBody)

	_, err := fetchDOIItemWithCache(context.Background(), http.DefaultClient, "10.1234/demo", nil)
	if err == nil {
		t.Fatal("want an error when CrossRef is unavailable")
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("error = %v, want the CrossRef 503 surfaced", err)
	}
	if *dcHits != 0 {
		t.Errorf("datacite hits = %d, want 0: a 5xx is not 'no such record'", *dcHits)
	}
	if *crHits != 1 {
		t.Errorf("crossref hits = %d, want 1", *crHits)
	}
}

// TestFetchDOIItemBothRegistriesMissNamesBoth pins the diagnostic. The
// single-registry message sent a downstream consumer looking for a malformed
// DOI when the DOI was fine.
func TestFetchDOIItemBothRegistriesMissNamesBoth(t *testing.T) {
	if _, _ = registryServers(t, http.StatusNotFound, ""); true {
		_, err := fetchDOIItemWithCache(context.Background(), http.DefaultClient, "10.9999/nowhere", nil)
		if err == nil {
			t.Fatal("want an error when neither registry has the DOI")
		}
		msg := err.Error()
		for _, want := range []string{"CrossRef", "DataCite"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q does not name %s", msg, want)
			}
		}
	}
}

// TestFetchDOIItemPrefersCrossRefWhenItHasTheRecord keeps the happy path free
// of a second request.
func TestFetchDOIItemPrefersCrossRefWhenItHasTheRecord(t *testing.T) {
	var dcHits int
	crossRef := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"type":"journal-article","title":["CrossRef Owned"],"DOI":"10.1234/demo"}}`))
	}))
	t.Cleanup(crossRef.Close)
	withBase(t, &enrichCrossRefBase, crossRef.URL)

	dataCite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dcHits++
	}))
	t.Cleanup(dataCite.Close)
	withBase(t, &enrichDataCiteBase, dataCite.URL)

	item, err := fetchDOIItemWithCache(context.Background(), http.DefaultClient, "10.1234/demo", nil)
	if err != nil {
		t.Fatalf("fetchDOIItemWithCache: %v", err)
	}
	if item["title"] != "CrossRef Owned" || item["itemType"] != "journalArticle" {
		t.Errorf("item = %#v, want the CrossRef record", item)
	}
	if dcHits != 0 {
		t.Errorf("datacite hits = %d, want 0 when CrossRef resolves", dcHits)
	}
}

// TestImportResolveRefreshResolvesArxivDOI drives the actual reported command
// path: a manifest entry left unresolved by CrossRef now resolves.
func TestImportResolveRefreshResolvesArxivDOI(t *testing.T) {
	registryServers(t, http.StatusNotFound, dataCiteArxivBody)

	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	manifest := importManifest{SchemaVersion: importManifestSchemaVersion, Entries: []importManifestEntry{{
		Path:           "/tmp/paper.pdf",
		Classification: "new",
		Action:         "create",
		IdentifierType: "doi",
		Identifier:     "10.48550/arXiv.2605.10930",
		Status:         "unresolved",
		Note:           "fetching CrossRef metadata: HTTP 404: Resource not found",
	}}}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	var out bytes.Buffer
	cmd := newImportResolveCmd(&rootFlags{asJSON: true, timeout: 5 * time.Second})
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{manifestPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import resolve: %v", err)
	}

	var got importManifest
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", out.String(), err)
	}
	if len(got.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(got.Entries))
	}
	entry := got.Entries[0]
	if entry.Status != "resolved" {
		t.Fatalf("status = %q note = %q, want resolved", entry.Status, entry.Note)
	}
	if entry.Note != "" {
		t.Errorf("note = %q, want cleared", entry.Note)
	}
	if entry.Item["title"] != "Evaluating the False Trust Engendered by LLM Explanations" {
		t.Errorf("item title = %v", entry.Item["title"])
	}
}

func TestDataCiteItemTypeMapsResourceTypes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Preprint", "preprint"},
		{"JournalArticle", "journalArticle"},
		{"ConferencePaper", "conferencePaper"},
		{"Dataset", "dataset"},
		{"Software", "computerProgram"},
		{"ComputationalNotebook", "computerProgram"},
		{"BookChapter", "bookSection"},
		{"Dissertation", "thesis"},
		{"Report", "report"},
		{"Text", "document"},
		{"", "document"},
		{"SomethingNew", "document"},
	} {
		if got := dataCiteItemType(tc.in); got != tc.want {
			t.Errorf("dataCiteItemType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDataCitePublisherTolerationAcceptsBothShapes covers the schema 4.5
// change. An object-form publisher must not fail the whole decode, which would
// turn a lost field into a lost resolution.
func TestDataCitePublisherTolerationAcceptsBothShapes(t *testing.T) {
	for name, body := range map[string]string{
		"string": `{"data":{"attributes":{"titles":[{"title":"T"}],"publisher":"Zenodo","types":{"resourceTypeGeneral":"Dataset"}}}}`,
		"object": `{"data":{"attributes":{"titles":[{"title":"T"}],"publisher":{"name":"Zenodo"},"types":{"resourceTypeGeneral":"Dataset"}}}}`,
		"null":   `{"data":{"attributes":{"titles":[{"title":"T"}],"publisher":null,"types":{"resourceTypeGeneral":"Dataset"}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var decoded dataCiteResponse
			if err := json.Unmarshal([]byte(body), &decoded); err != nil {
				t.Fatalf("decode %s publisher: %v", name, err)
			}
			item := dataCiteItemFromAttributes(decoded.Data.Attributes, "10.5281/zenodo.1")
			if item["itemType"] != "dataset" || item["title"] != "T" {
				t.Fatalf("item = %#v", item)
			}
			wantPublisher := "Zenodo"
			if name == "null" {
				wantPublisher = ""
			}
			got, _ := item["publisher"].(string)
			if got != wantPublisher {
				t.Errorf("publisher = %q, want %q", got, wantPublisher)
			}
		})
	}
}

func TestDataCiteCreatorsHandleOrganisationsAndUnsplitNames(t *testing.T) {
	creators := dataCiteCreators([]dataCiteCreator{
		{Name: "CERN", NameType: "Organizational"},
		{Name: "Lovelace, Ada"},
		{Name: "Prince"},
		{Name: "ignored", NameType: "Organizational", GivenName: "x"},
		{},
	})
	if len(creators) != 4 {
		t.Fatalf("creators = %#v, want 4", creators)
	}
	// An organisation is one indivisible name, never split into first/last.
	if creators[0]["name"] != "CERN" {
		t.Errorf("organisation = %#v", creators[0])
	}
	if _, split := creators[0]["lastName"]; split {
		t.Errorf("organisation was split: %#v", creators[0])
	}
	if creators[1]["lastName"] != "Lovelace" || creators[1]["firstName"] != "Ada" {
		t.Errorf("comma name = %#v", creators[1])
	}
	// A mononym has no given name; it must not lose the name entirely.
	if creators[2]["lastName"] != "Prince" {
		t.Errorf("mononym = %#v", creators[2])
	}
}

func TestArxivIDFromSelfDOIFoldsCase(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"10.48550/arXiv.2605.10930", "2605.10930"},
		{"10.48550/arxiv.2605.10930", "2605.10930"},
		{"10.1234/not-arxiv", ""},
		{"", ""},
	} {
		if got := arxivIDFromSelfDOI(tc.in); got != tc.want {
			t.Errorf("arxivIDFromSelfDOI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
