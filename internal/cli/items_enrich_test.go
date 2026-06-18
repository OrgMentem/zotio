// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean dk33): cover the enrichment providers, apply step, store-backed
// work queue, and dry-run preview of `items enrich`.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"zotero-pp-cli/internal/store"
)

// crossRefSearchServer serves a CrossRef bibliographic search whose result set
// contains one wrong-title candidate and one exact-title match.
func crossRefSearchServer(t *testing.T, matchTitle, matchDOI string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"message":{"items":[` +
			`{"title":["A Completely Unrelated Paper"],"DOI":"10.0/wrong"},` +
			`{"title":["` + matchTitle + `"],"DOI":"` + matchDOI + `"}` +
			`]}}`
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func withBase(t *testing.T, target *string, value string) {
	t.Helper()
	saved := *target
	*target = value
	t.Cleanup(func() { *target = saved })
}

func TestResolveDOIViaCrossRef_ExactTitleMatch(t *testing.T) {
	srv := crossRefSearchServer(t, "Attention Is All You Need", "10.1/attention")
	withBase(t, &enrichCrossRefBase, srv.URL)

	data := map[string]any{"title": "attention is all you need", "creators": []any{map[string]any{"lastName": "Vaswani"}}}
	doi, _, ok := resolveDOIViaCrossRef(context.Background(), http.DefaultClient, data)
	if !ok {
		t.Fatal("expected a confident match")
	}
	if doi != "10.1/attention" {
		t.Errorf("doi = %q, want 10.1/attention", doi)
	}
}

func TestResolveDOIViaCrossRef_NoConfidentMatch(t *testing.T) {
	srv := crossRefSearchServer(t, "Some Other Title", "10.2/other")
	withBase(t, &enrichCrossRefBase, srv.URL)

	data := map[string]any{"title": "a title that matches nothing returned"}
	if _, _, ok := resolveDOIViaCrossRef(context.Background(), http.DefaultClient, data); ok {
		t.Error("expected no match for a non-matching title")
	}
}

func TestResolveAbstractViaCrossRef_StripsJATS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"abstract":"<jats:p>Hello <jats:italic>world</jats:italic> &amp; more.</jats:p>"}}`))
	}))
	defer srv.Close()
	withBase(t, &enrichCrossRefBase, srv.URL)

	abstract, ok := resolveAbstractViaCrossRef(context.Background(), http.DefaultClient, "10.1/x")
	if !ok {
		t.Fatal("expected an abstract")
	}
	if abstract != "Hello world & more." {
		t.Errorf("abstract = %q, want stripped JATS", abstract)
	}
}

func TestResolvePDFViaUnpaywall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("email") == "" {
			t.Error("Unpaywall request missing email")
		}
		_, _ = w.Write([]byte(`{"best_oa_location":{"url_for_pdf":"https://oa.example/p.pdf","url":"https://oa.example/landing"}}`))
	}))
	defer srv.Close()
	withBase(t, &enrichUnpaywallBase, srv.URL)

	pdf, ok := resolvePDFViaUnpaywall(context.Background(), http.DefaultClient, "10.1/x", "me@example.com")
	if !ok || pdf != "https://oa.example/p.pdf" {
		t.Errorf("pdf = %q ok=%v, want url_for_pdf", pdf, ok)
	}
}

func TestResolvePDFViaUnpaywall_NoOA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"best_oa_location":{}}`))
	}))
	defer srv.Close()
	withBase(t, &enrichUnpaywallBase, srv.URL)
	if _, ok := resolvePDFViaUnpaywall(context.Background(), http.DefaultClient, "10.1/x", "me@example.com"); ok {
		t.Error("expected no OA PDF")
	}
}

type fakeMutator struct {
	patchPath string
	patchBody map[string]any
	postPath  string
	postBody  any
}

func (f *fakeMutator) Patch(path string, body any) (json.RawMessage, int, error) {
	f.patchPath = path
	f.patchBody, _ = body.(map[string]any)
	return json.RawMessage(`{}`), 200, nil
}

func (f *fakeMutator) Post(path string, body any) (json.RawMessage, int, error) {
	f.postPath = path
	f.postBody = body
	return json.RawMessage(`{}`), 200, nil
}

func TestApplyEnrichProposal_PatchIncludesVersionAndProvenance(t *testing.T) {
	f := &fakeMutator{}
	p := enrichProposal{
		Key: "ABC", Category: "missing_doi", Action: enrichActionPatch,
		Source: "CrossRef", Fields: map[string]any{"DOI": "10.1/x"}, version: float64(7),
	}
	status := applyEnrichProposal(f, &p, &rootFlags{})
	if status != "applied" {
		t.Fatalf("status = %q, want applied", status)
	}
	if f.patchPath != "/items/ABC" {
		t.Errorf("patch path = %q", f.patchPath)
	}
	if f.patchBody["DOI"] != "10.1/x" {
		t.Errorf("patch body DOI = %v", f.patchBody["DOI"])
	}
	if f.patchBody["version"] != float64(7) {
		t.Errorf("patch body version = %v, want 7", f.patchBody["version"])
	}
	extra, _ := f.patchBody["extra"].(string)
	if !strings.Contains(extra, "DOI added via CrossRef") {
		t.Errorf("extra provenance missing: %q", extra)
	}
}

func TestApplyEnrichProposal_AttachPostsChild(t *testing.T) {
	f := &fakeMutator{}
	p := enrichProposal{
		Key: "ABC", Category: "missing_pdf", Action: enrichActionAttach, Source: "Unpaywall",
		Attachment: map[string]any{"itemType": "attachment", "linkMode": "linked_url", "url": "https://oa/p.pdf", "parentItem": "ABC"},
	}
	if status := applyEnrichProposal(f, &p, &rootFlags{}); status != "applied" {
		t.Fatalf("status = %q, want applied", status)
	}
	if f.postPath != "/items" {
		t.Errorf("post path = %q, want /items", f.postPath)
	}
	arr, ok := f.postBody.([]map[string]any)
	if !ok || len(arr) != 1 || arr[0]["linkMode"] != "linked_url" {
		t.Errorf("post body = %v, want one linked_url attachment", f.postBody)
	}
}

// seedEnrichStore writes one missing-DOI item to the canonical dbPath under the
// test-isolated HOME.
func seedEnrichStore(t *testing.T) localQueryStore {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	dbPath := defaultDBPath("zotero-pp-cli")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := []json.RawMessage{
		json.RawMessage(`{"key":"K1","version":9,"data":{"key":"K1","itemType":"journalArticle","title":"Attention Is All You Need","creators":[{"lastName":"Vaswani"}]}}`),
		json.RawMessage(`{"key":"K2","version":3,"data":{"key":"K2","itemType":"journalArticle","title":"No Match In CrossRef Here"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = db.Close()

	rawDB, err := openStoreForRead(context.Background(), "zotero-pp-cli")
	if err != nil || rawDB == nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	return localQueryStore{rawDB}
}

func TestBuildEnrichProposals_DOIFromStore(t *testing.T) {
	srv := crossRefSearchServer(t, "Attention Is All You Need", "10.1/attention")
	withBase(t, &enrichCrossRefBase, srv.URL)
	db := seedEnrichStore(t)

	proposals, skipped := buildEnrichProposals(context.Background(), db, http.DefaultClient, "missing_doi", 25, "")
	if len(proposals) != 1 {
		t.Fatalf("proposals = %d, want 1: %+v", len(proposals), proposals)
	}
	if proposals[0].Key != "K1" || proposals[0].Fields["DOI"] != "10.1/attention" {
		t.Errorf("proposal = %+v, want K1 with DOI", proposals[0])
	}
	if proposals[0].version != float64(9) {
		t.Errorf("proposal version = %v, want 9", proposals[0].version)
	}
	// K2's title has no CrossRef match -> skipped, not silently dropped.
	if len(skipped) != 1 || skipped[0].Key != "K2" {
		t.Errorf("skipped = %+v, want K2", skipped)
	}
}

func TestItemsEnrichDryRunPreview(t *testing.T) {
	srv := crossRefSearchServer(t, "Attention Is All You Need", "10.1/attention")
	withBase(t, &enrichCrossRefBase, srv.URL)
	_ = seedEnrichStore(t) // sets HOME to the seeded store

	flags := &rootFlags{asJSON: true} // no yes -> preview only
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--missing-doi"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	var report struct {
		Applied   bool             `json:"applied"`
		DryRun    bool             `json:"dry_run"`
		Proposals []enrichProposal `json:"proposals"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if report.Applied || !report.DryRun {
		t.Errorf("expected dry-run preview, got applied=%v dry_run=%v", report.Applied, report.DryRun)
	}
	if len(report.Proposals) != 1 || report.Proposals[0].Status != "" {
		t.Errorf("expected 1 unapplied proposal, got %+v", report.Proposals)
	}
}

func TestItemsEnrichApplyViaAPI(t *testing.T) {
	crsrv := crossRefSearchServer(t, "Attention Is All You Need", "10.1/attention")
	withBase(t, &enrichCrossRefBase, crsrv.URL)
	_ = seedEnrichStore(t) // sets HOME + ZOTERO_CONFIG to the seeded store

	var gotBody map[string]any
	zsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/items/K1" {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer zsrv.Close()
	t.Setenv("ZOTERO_BASE_URL", zsrv.URL)

	flags := &rootFlags{asJSON: true, yes: true} // apply mode
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--missing-doi"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enrich apply: %v", err)
	}

	if gotBody == nil {
		t.Fatal("Zotero server never received the PATCH")
	}
	if gotBody["DOI"] != "10.1/attention" {
		t.Errorf("patched DOI = %v, want 10.1/attention", gotBody["DOI"])
	}
	if gotBody["version"] != float64(9) {
		t.Errorf("patched version = %v, want 9 (conflict guard)", gotBody["version"])
	}

	var report struct {
		Applied   bool             `json:"applied"`
		Proposals []enrichProposal `json:"proposals"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if !report.Applied || len(report.Proposals) != 1 || report.Proposals[0].Status != "applied" {
		t.Errorf("expected one applied proposal, got %+v", report)
	}
}
