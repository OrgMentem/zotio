// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase4 identifier-imports): Cover PMID, arXiv, and ISBN import adapters.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// PATCH(glean roadmap-phase4 identifier-imports): Verify PubMed eSummary records map to Zotero journalArticle fields.
func TestPubMedItemFromSummary(t *testing.T) {
	item := pubmedItemFromSummary(map[string]any{
		"title":           "PubMed Title",
		"authors":         []any{map[string]any{"name": "Lovelace A"}},
		"pubdate":         "1843",
		"fulljournalname": "Proceedings of Computation",
		"volume":          "1",
		"issue":           "2",
		"pages":           "3-9",
		"articleids":      []any{map[string]any{"idtype": "doi", "value": "10.1000/pmid"}},
	})

	if item["itemType"] != "journalArticle" || item["title"] != "PubMed Title" {
		t.Fatalf("pubmed item = %v", item)
	}
	if item["DOI"] != "10.1000/pmid" || item["date"] != "1843" {
		t.Errorf("pubmed DOI/date = %v/%v", item["DOI"], item["date"])
	}
	creators, ok := item["creators"].([]map[string]any)
	if !ok || len(creators) != 1 {
		t.Fatalf("creators = %v, want one creator", item["creators"])
	}
	if creators[0]["lastName"] != "Lovelace" || creators[0]["firstName"] != "A" {
		t.Errorf("creator[0] = %v", creators[0])
	}
}

// PATCH(glean roadmap-phase4 identifier-imports): Verify arXiv Atom entries map to Zotero preprint fields.
func TestArxivItemFromEntry(t *testing.T) {
	item := arxivItemFromEntry(arxivEntry{
		Title:     "\n  ArXiv   Title  \n",
		Summary:   "\nAbstract text.\n",
		Published: "2024-01-02T03:04:05Z",
		Authors:   []arxivAuthor{{Name: "Ada Lovelace"}},
		DOI:       "10.48550/arXiv.2401.00001",
	}, "2401.00001")

	if item["itemType"] != "preprint" || item["title"] != "ArXiv Title" {
		t.Fatalf("arxiv item = %v", item)
	}
	if item["abstractNote"] != "Abstract text." || item["date"] != "2024-01-02" {
		t.Errorf("arxiv abstract/date = %v/%v", item["abstractNote"], item["date"])
	}
	if item["DOI"] != "10.48550/arXiv.2401.00001" || item["extra"] != "arXiv: 2401.00001" {
		t.Errorf("arxiv DOI/extra = %v/%v", item["DOI"], item["extra"])
	}
	creators, ok := item["creators"].([]map[string]any)
	if !ok || len(creators) != 1 {
		t.Fatalf("creators = %v, want one creator", item["creators"])
	}
}

// PATCH(glean roadmap-phase4 identifier-imports): Verify Open Library records map to Zotero book fields.
func TestOpenLibraryItemFromData(t *testing.T) {
	item := openLibraryItemFromData(map[string]any{
		"title":           "Book Title",
		"authors":         []any{map[string]any{"name": "Grace Hopper"}},
		"publish_date":    "1952",
		"publishers":      []any{map[string]any{"name": "Compiler Press"}},
		"number_of_pages": float64(256),
	}, "9781234567890")

	if item["itemType"] != "book" || item["title"] != "Book Title" {
		t.Fatalf("isbn item = %v", item)
	}
	if item["ISBN"] != "9781234567890" || item["publisher"] != "Compiler Press" || item["numPages"] != float64(256) {
		t.Errorf("isbn mapped fields = %v", item)
	}
	creators, ok := item["creators"].([]map[string]any)
	if !ok || len(creators) != 1 {
		t.Fatalf("creators = %v, want one creator", item["creators"])
	}
}

// PATCH(glean roadmap-phase4 identifier-imports): Smoke-test PubMed import --dry-run against a capped httptest response.
func TestImportPmidDryRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/esummary.fcgi" || r.URL.Query().Get("id") != "314159" {
			t.Errorf("PubMed request URL = %s", r.URL.String())
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "zotero-pp-cli/1.0.0" {
			t.Errorf("PubMed headers Accept=%q User-Agent=%q", r.Header.Get("Accept"), r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"uids":["314159"],"314159":{"title":"PubMed Dry Title","authors":[{"name":"Curie M"}],"pubdate":"1911","fulljournalname":"Journal of Radium","articleids":[{"idtype":"doi","value":"10.1000/radium"}]}}}`))
	}))
	defer srv.Close()
	withBase(t, &importPubMedBase, srv.URL)

	flags := &rootFlags{asJSON: true, timeout: 5 * time.Second}
	cmd := newImportPmidCmd(flags)
	cmd.SetArgs([]string{"314159", "--dry-run"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import pmid --dry-run: %v", err)
	}

	env := decodeIdentifierDryRun(t, out.Bytes())
	if !env.DryRun || env.Item["itemType"] != "journalArticle" || env.Item["title"] != "PubMed Dry Title" {
		t.Fatalf("pubmed dry-run = %+v", env)
	}
	if env.Item["DOI"] != "10.1000/radium" || env.Item["date"] != "1911" {
		t.Errorf("pubmed dry-run DOI/date = %v/%v", env.Item["DOI"], env.Item["date"])
	}
	assertIdentifierDryRunCreator(t, env.Item)
}

// PATCH(glean roadmap-phase4 identifier-imports): Smoke-test arXiv import --dry-run against a capped httptest response.
func TestImportArxivDryRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/query" || r.URL.Query().Get("id_list") != "2401.00001" {
			t.Errorf("arXiv request URL = %s", r.URL.String())
		}
		if r.Header.Get("User-Agent") != "zotero-pp-cli/1.0.0" {
			t.Errorf("arXiv User-Agent = %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:arxiv="http://arxiv.org/schemas/atom">
  <entry>
    <id>http://arxiv.org/abs/2401.00001v1</id>
    <title>ArXiv Dry Title</title>
    <summary>
      Dry abstract.
    </summary>
    <published>2024-01-02T03:04:05Z</published>
    <author><name>Ada Lovelace</name></author>
    <arxiv:doi>10.48550/arXiv.2401.00001</arxiv:doi>
  </entry>
</feed>`))
	}))
	defer srv.Close()
	withBase(t, &importArxivBase, srv.URL)

	flags := &rootFlags{asJSON: true, timeout: 5 * time.Second}
	cmd := newImportArxivCmd(flags)
	cmd.SetArgs([]string{"2401.00001", "--dry-run"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import arxiv --dry-run: %v", err)
	}

	env := decodeIdentifierDryRun(t, out.Bytes())
	if !env.DryRun || env.Item["itemType"] != "preprint" || env.Item["title"] != "ArXiv Dry Title" {
		t.Fatalf("arxiv dry-run = %+v", env)
	}
	if env.Item["DOI"] != "10.48550/arXiv.2401.00001" || env.Item["date"] != "2024-01-02" {
		t.Errorf("arxiv dry-run DOI/date = %v/%v", env.Item["DOI"], env.Item["date"])
	}
	assertIdentifierDryRunCreator(t, env.Item)
}

// PATCH(glean roadmap-phase4 identifier-imports): Smoke-test ISBN import --dry-run against a capped httptest response.
func TestImportIsbnDryRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/books" || r.URL.Query().Get("bibkeys") != "ISBN:9781234567890" {
			t.Errorf("Open Library request URL = %s", r.URL.String())
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "zotero-pp-cli/1.0.0" {
			t.Errorf("Open Library headers Accept=%q User-Agent=%q", r.Header.Get("Accept"), r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ISBN:9781234567890":{"title":"ISBN Dry Title","authors":[{"name":"Octavia Butler"}],"publish_date":"1979","publishers":[{"name":"Doubleday"}],"number_of_pages":264}}`))
	}))
	defer srv.Close()
	withBase(t, &importOpenLibraryBase, srv.URL)

	flags := &rootFlags{asJSON: true, timeout: 5 * time.Second}
	cmd := newImportIsbnCmd(flags)
	cmd.SetArgs([]string{"9781234567890", "--dry-run"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import isbn --dry-run: %v", err)
	}

	env := decodeIdentifierDryRun(t, out.Bytes())
	if !env.DryRun || env.Item["itemType"] != "book" || env.Item["title"] != "ISBN Dry Title" {
		t.Fatalf("isbn dry-run = %+v", env)
	}
	if env.Item["ISBN"] != "9781234567890" {
		t.Errorf("isbn dry-run ISBN = %v", env.Item["ISBN"])
	}
	assertIdentifierDryRunCreator(t, env.Item)
}

type identifierDryRunEnvelope struct {
	DryRun bool           `json:"dry_run"`
	Source string         `json:"source"`
	Item   map[string]any `json:"item"`
}

// PATCH(glean roadmap-phase4 identifier-imports): Decode shared dry-run envelopes emitted by identifier import commands.
func decodeIdentifierDryRun(t *testing.T, data []byte) identifierDryRunEnvelope {
	t.Helper()
	var env identifierDryRunEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("decode %q: %v", string(data), err)
	}
	return env
}

// PATCH(glean roadmap-phase4 identifier-imports): Assert dry-run JSON preserved at least one mapped Zotero creator.
func assertIdentifierDryRunCreator(t *testing.T, item map[string]any) {
	t.Helper()
	creators, ok := item["creators"].([]any)
	if !ok || len(creators) == 0 {
		t.Fatalf("creators = %v, want at least one creator", item["creators"])
	}
}
