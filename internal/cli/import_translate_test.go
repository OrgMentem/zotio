// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean 37jv): cover URL/DOI import metadata translation and dry-run.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const citationHTML = `<!doctype html><html><head>
<title>Publisher Page Title</title>
<meta name="citation_title" content="Attention Is All You Need">
<meta name="citation_author" content="Vaswani, Ashish">
<meta name="citation_author" content="Shazeer, Noam">
<meta name="citation_doi" content="10.5555/attention">
<meta name="citation_journal_title" content="NeurIPS">
<meta name="citation_publication_date" content="2017/06/12">
<meta name="citation_abstract" content="The dominant sequence models use recurrence.">
<meta property="og:description" content="ignored when citation_abstract present">
</head><body>...</body></html>`

func TestExtractDOIFromURL(t *testing.T) {
	cases := map[string]string{
		"https://doi.org/10.1234/abc.def":                  "10.1234/abc.def",
		"https://publisher.example/articles/10.5555/x.pdf": "10.5555/x",
		"https://example.com/no-doi-here":                  "",
	}
	for in, want := range cases {
		if got := extractDOIFromURL(in); got != want {
			t.Errorf("extractDOIFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseHTMLMetaAndItem(t *testing.T) {
	metas, title := parseHTMLMeta(citationHTML)
	if title != "Publisher Page Title" {
		t.Errorf("title = %q", title)
	}
	if len(metas["citation_author"]) != 2 {
		t.Errorf("expected 2 authors, got %v", metas["citation_author"])
	}

	item := itemFromEmbeddedMeta(metas, title, "https://x/article", "2026-06-16")
	if item == nil {
		t.Fatal("expected an item")
	}
	if item["title"] != "Attention Is All You Need" {
		t.Errorf("title = %v", item["title"])
	}
	if item["itemType"] != "journalArticle" {
		t.Errorf("itemType = %v, want journalArticle", item["itemType"])
	}
	if item["DOI"] != "10.5555/attention" {
		t.Errorf("DOI = %v", item["DOI"])
	}
	if item["publicationTitle"] != "NeurIPS" {
		t.Errorf("publicationTitle = %v", item["publicationTitle"])
	}
	if item["abstractNote"] != "The dominant sequence models use recurrence." {
		t.Errorf("abstractNote = %v", item["abstractNote"])
	}
	creators, ok := item["creators"].([]map[string]any)
	if !ok || len(creators) != 2 {
		t.Fatalf("creators = %v, want 2", item["creators"])
	}
	if creators[0]["lastName"] != "Vaswani" || creators[0]["firstName"] != "Ashish" {
		t.Errorf("creator[0] = %v", creators[0])
	}
}

func TestItemFromEmbeddedMeta_NoTitleReturnsNil(t *testing.T) {
	if item := itemFromEmbeddedMeta(map[string][]string{"og:image": {"x"}}, "", "https://x", "2026-06-16"); item != nil {
		t.Errorf("expected nil for no usable title, got %v", item)
	}
}

func TestBuildImportItemFromURL_DOIInURL(t *testing.T) {
	crsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"type":"journal-article","title":["Resolved Title"],"DOI":"10.1234/abc","container-title":["Nature"],"abstract":"<jats:p>From CrossRef.</jats:p>"}}`))
	}))
	defer crsrv.Close()
	withBase(t, &enrichCrossRefBase, crsrv.URL)

	rawURL := "https://publisher.example/doi/10.1234/abc"
	item, source := buildImportItemFromURL(context.Background(), http.DefaultClient, rawURL)
	if source == "" || item["title"] != "Resolved Title" {
		t.Fatalf("source=%q item=%v, want CrossRef title", source, item)
	}
	if item["url"] != rawURL {
		t.Errorf("url = %v, want original URL preserved", item["url"])
	}
	if item["abstractNote"] != "From CrossRef." {
		t.Errorf("abstractNote = %v", item["abstractNote"])
	}
}

func TestBuildImportItemFromURL_PrivateHostFallsBack(t *testing.T) {
	pagesrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(citationHTML))
	}))
	defer pagesrv.Close()

	item, source := buildImportItemFromURL(context.Background(), http.DefaultClient, pagesrv.URL+"/article")
	if source != "fallback (no metadata)" {
		t.Fatalf("source = %q, want fallback for private test host", source)
	}
	if item["title"] != pagesrv.URL+"/article" || item["itemType"] != "webpage" {
		t.Errorf("item = %v", item)
	}
}

func TestExternalHTTPClientRejectsRedirectToPrivateHost(t *testing.T) {
	targetHit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHit = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/private", http.StatusFound)
	}))
	defer redirector.Close()

	req, err := http.NewRequest(http.MethodGet, redirector.URL+"/metadata", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := externalHTTPClient(redirector.Client(), false).Do(req)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("redirect to private host succeeded, want rejection")
	}
	if targetHit {
		t.Fatal("redirect target was reached; private redirect should be blocked before follow-up request")
	}
}

func TestBuildImportItemFromURL_Fallback(t *testing.T) {
	pdfsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.7 ..."))
	}))
	defer pdfsrv.Close()

	rawURL := pdfsrv.URL + "/file.pdf"
	item, source := buildImportItemFromURL(context.Background(), http.DefaultClient, rawURL)
	if source != "fallback (no metadata)" {
		t.Errorf("source = %q, want fallback", source)
	}
	if item["itemType"] != "webpage" || item["title"] != rawURL {
		t.Errorf("fallback item = %v", item)
	}
}

func TestImportURLDryRun(t *testing.T) {
	pagesrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(citationHTML))
	}))
	defer pagesrv.Close()

	flags := &rootFlags{asJSON: true, dryRun: true, timeout: 5 * time.Second}
	cmd := newImportUrlCmd(flags)
	cmd.SetArgs([]string{pagesrv.URL + "/article", "--collection", "COL9"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import url --dry-run: %v", err)
	}

	var env struct {
		DryRun bool           `json:"dry_run"`
		Source string         `json:"source"`
		Item   map[string]any `json:"item"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if !env.DryRun || env.Source != "fallback (no metadata)" {
		t.Errorf("dry_run=%v source=%q", env.DryRun, env.Source)
	}
	if env.Item["title"] != pagesrv.URL+"/article" {
		t.Errorf("item title = %v", env.Item["title"])
	}
	// --collection assignment preserved in the previewed body.
	if _, ok := env.Item["collections"]; !ok {
		t.Errorf("collections not preserved: %v", env.Item)
	}
}

func TestImportDoiDryRun(t *testing.T) {
	crsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"type":"journal-article","title":["DOI Title"],"DOI":"10.1/x","abstract":"<jats:p>Abs.</jats:p>"}}`))
	}))
	defer crsrv.Close()
	withBase(t, &enrichCrossRefBase, crsrv.URL)

	flags := &rootFlags{asJSON: true, dryRun: true, timeout: 5 * time.Second}
	cmd := newImportDoiCmd(flags)
	cmd.SetArgs([]string{"10.1/x"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import doi --dry-run: %v", err)
	}
	var env struct {
		DryRun bool           `json:"dry_run"`
		Item   map[string]any `json:"item"`
	}
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if !env.DryRun || env.Item["title"] != "DOI Title" || env.Item["abstractNote"] != "Abs." {
		t.Errorf("doi dry-run item = %v (dry_run=%v)", env.Item, env.DryRun)
	}
}
