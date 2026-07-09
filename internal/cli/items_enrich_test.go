// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// work queue, and mutation-envelope preview of `items enrich`.

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

	"zotio/internal/client"
	"zotio/internal/mutation"
	"zotio/internal/store"
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
	patchErr  error
	postPath  string
	postBody  any
	postErr   error
}

func (f *fakeMutator) Patch(path string, body any) (json.RawMessage, int, error) {
	f.patchPath = path
	f.patchBody, _ = body.(map[string]any)
	if f.patchErr != nil {
		return nil, http.StatusPreconditionFailed, f.patchErr
	}
	return json.RawMessage(`{}`), 200, nil
}

func (f *fakeMutator) Post(path string, body any) (json.RawMessage, int, error) {
	f.postPath = path
	f.postBody = body
	if f.postErr != nil {
		return nil, http.StatusInternalServerError, f.postErr
	}
	return json.RawMessage(`{}`), 200, nil
}

func TestApplyEnrichProposal_PatchIncludesVersionAndProvenance(t *testing.T) {
	f := &fakeMutator{}
	p := enrichProposal{
		Key: "ABC", Category: "missing_doi", Action: enrichActionPatch,
		Source: "CrossRef", Fields: map[string]any{"DOI": "10.1/x"}, version: float64(7),
	}
	status, reason, err := applyEnrichProposal(f, &p, &rootFlags{})
	if err != nil || reason != nil || status != "applied" {
		t.Fatalf("apply = status %q reason %v err %v, want applied without reason/error", status, reason, err)
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
	status, reason, err := applyEnrichProposal(f, &p, &rootFlags{})
	if err != nil || reason != nil || status != "applied" {
		t.Fatalf("apply = status %q reason %v err %v, want applied without reason/error", status, reason, err)
	}
	if f.postPath != "/items" {
		t.Errorf("post path = %q, want /items", f.postPath)
	}
	arr, ok := f.postBody.([]map[string]any)
	if !ok || len(arr) != 1 || arr[0]["linkMode"] != "linked_url" {
		t.Errorf("post body = %v, want one linked_url attachment", f.postBody)
	}
}

func TestApplyEnrichProposal_ConflictStatusIsTyped(t *testing.T) {
	f := &fakeMutator{patchErr: &client.APIError{Method: http.MethodPatch, Path: "/items/ABC", StatusCode: http.StatusPreconditionFailed, Body: "stale"}}
	p := enrichProposal{
		Key: "ABC", Category: "missing_doi", Action: enrichActionPatch,
		Source: "CrossRef", Fields: map[string]any{"DOI": "10.1/x"}, version: float64(7),
	}
	status, reason, err := applyEnrichProposal(f, &p, &rootFlags{})
	if err == nil || status != "conflict" || reason != "stale" {
		t.Fatalf("apply = status %q reason %v err %v, want typed conflict with API body reason", status, reason, err)
	}
}

// seedEnrichStore writes one missing-DOI item to the canonical dbPath under the
// test-isolated HOME.
func seedEnrichStore(t *testing.T) localQueryStore {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	dbPath := defaultDBPath("zotio")
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

	rawDB, err := openStoreForRead(context.Background(), "zotio")
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

	proposals, skipped := buildEnrichProposals(context.Background(), db, http.DefaultClient, "missing_doi", 25, "", nil, "", false, false)
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

// Semantic Scholar is the final exact-title DOI fallback after CrossRef and OpenAlex miss.
func TestBuildEnrichProposals_DOIFromSemanticScholarFallback(t *testing.T) {
	cr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"items":[]}}`))
	}))
	t.Cleanup(cr.Close)
	withBase(t, &enrichCrossRefBase, cr.URL)
	oa := openAlexWorkServer(t, `{"results":[]}`)
	withBase(t, &enrichOpenAlexBase, oa.URL)
	ss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") == "Attention Is All You Need" {
			_, _ = w.Write([]byte(`{"data":[{"title":"Attention Is All You Need","externalIds":{"DOI":"10.555/semantic"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(ss.Close)
	withBase(t, &enrichSemanticScholarBase, ss.URL)
	db := seedEnrichStore(t)

	proposals, skipped := buildEnrichProposals(context.Background(), db, http.DefaultClient, "missing_doi", 25, "", nil, "", true, true)
	if len(proposals) != 1 {
		t.Fatalf("proposals = %d, want 1: %+v (skipped=%+v)", len(proposals), proposals, skipped)
	}
	if proposals[0].Source != "Semantic Scholar" {
		t.Fatalf("source = %q, want Semantic Scholar", proposals[0].Source)
	}
	if proposals[0].Fields["DOI"] != "10.555/semantic" {
		t.Errorf("DOI = %v, want Semantic Scholar DOI", proposals[0].Fields["DOI"])
	}
}

// Collection scoping should filter the local work queue
// before enrichment providers are asked to resolve candidates.
func TestItemsEnrichMissingDOICollectionScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"items":[` +
			`{"title":["In Collection"],"DOI":"10.1/in"},` +
			`{"title":["Outside Collection"],"DOI":"10.1/out"}` +
			`]}}`))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichCrossRefBase, srv.URL)

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "collection.toml"))
	dbPath := defaultDBPath("zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := []json.RawMessage{
		json.RawMessage(`{"key":"KCOL","version":1,"data":{"key":"KCOL","itemType":"journalArticle","title":"In Collection","collections":["COLX"]}}`),
		json.RawMessage(`{"key":"KOUT","version":2,"data":{"key":"KOUT","itemType":"journalArticle","title":"Outside Collection"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = db.Close()

	flags := &rootFlags{asJSON: true}
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--missing-doi", "--collection", "COLX"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if env.Plan.Summary.Planned != 1 || len(env.Plan.Operations) != 1 {
		t.Fatalf("expected one scoped proposal, got summary=%+v ops=%+v", env.Plan.Summary, env.Plan.Operations)
	}
	if env.Plan.Operations[0].Key != "KCOL" {
		t.Errorf("proposal key = %q, want KCOL", env.Plan.Operations[0].Key)
	}
	if env.Journal != nil {
		t.Errorf("unexpected skipped journal from out-of-collection item: %+v", env.Journal)
	}
}

// Exact health remediation should be able to feed
// the specific missing-* item keys to `items enrich`, avoiding broad provider
// calls and broad mutation previews.
func TestItemsEnrichMissingDOIKeysFrom(t *testing.T) {
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query.bibliographic")
		requested = append(requested, q)
		_, _ = w.Write([]byte(`{"message":{"items":[` +
			`{"title":["In Selection"],"DOI":"10.1/in"},` +
			`{"title":["Outside Selection"],"DOI":"10.1/out"}` +
			`]}}`))
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichCrossRefBase, srv.URL)

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "keys-from.toml"))
	dbPath := defaultDBPath("zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := []json.RawMessage{
		json.RawMessage(`{"key":"KIN","version":1,"data":{"key":"KIN","itemType":"journalArticle","title":"In Selection"}}`),
		json.RawMessage(`{"key":"KOUT","version":2,"data":{"key":"KOUT","itemType":"journalArticle","title":"Outside Selection"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = db.Close()

	keysPath := filepath.Join(t.TempDir(), "keys.txt")
	if err := os.WriteFile(keysPath, []byte("KIN\n"), 0o600); err != nil {
		t.Fatalf("write keys: %v", err)
	}

	flags := &rootFlags{asJSON: true}
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--missing-doi", "--keys-from", keysPath})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if env.Plan.Summary.Planned != 1 || len(env.Plan.Operations) != 1 {
		t.Fatalf("expected one exact-key proposal, got summary=%+v ops=%+v", env.Plan.Summary, env.Plan.Operations)
	}
	if env.Plan.Operations[0].Key != "KIN" {
		t.Errorf("proposal key = %q, want KIN", env.Plan.Operations[0].Key)
	}
	if len(requested) != 1 || !strings.Contains(requested[0], "In Selection") {
		t.Errorf("provider calls = %v, want only In Selection", requested)
	}
}

func TestItemsEnrichPreviewEnvelope(t *testing.T) {
	srv := crossRefSearchServer(t, "Attention Is All You Need", "10.1/attention")
	withBase(t, &enrichCrossRefBase, srv.URL)
	_ = seedEnrichStore(t) // sets HOME to the seeded store

	flags := &rootFlags{asJSON: true} // no yes -> preview only
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--missing-doi", "--no-openalex", "--no-semantic-scholar"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if !env.OK || env.Mode != "preview" || env.PreviewReason != "default" || env.Result != nil {
		t.Errorf("expected ordinary preview envelope, got ok=%v mode=%q reason=%q result=%+v", env.OK, env.Mode, env.PreviewReason, env.Result)
	}
	if env.Plan.Summary.Planned != 1 || len(env.Plan.Operations) != 1 {
		t.Fatalf("expected 1 planned proposal, got summary=%+v ops=%+v", env.Plan.Summary, env.Plan.Operations)
	}
	if got := env.Plan.Operations[0].Changes; len(got) != 1 || got[0].Field != "DOI" || got[0].Add != "10.1/attention" {
		t.Errorf("proposal changes = %+v, want DOI add", got)
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

	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1} // apply mode
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--missing-doi", "--no-openalex", "--no-semantic-scholar"})
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

	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if !env.OK || env.Mode != "apply" || env.Result == nil {
		t.Fatalf("expected successful apply envelope, got %+v", env)
	}
	if env.Result.Summary.Applied != 1 || len(env.Result.Items) != 1 || env.Result.Items[0].Status != "applied" {
		t.Errorf("expected one applied result, got %+v", env.Result)
	}
}

func openAlexWorkServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReconstructAbstract(t *testing.T) {
	if got := reconstructAbstract(map[string][]int{"Hello": {0}, "world": {1}, "again": {2}}); got != "Hello world again" {
		t.Errorf("got %q, want 'Hello world again'", got)
	}
	// A word repeated at multiple positions.
	if got := reconstructAbstract(map[string][]int{"the": {0, 2}, "cat": {1}, "sat": {3}}); got != "the cat the sat" {
		t.Errorf("got %q, want 'the cat the sat'", got)
	}
	if reconstructAbstract(nil) != "" {
		t.Error("nil index should reconstruct to empty")
	}
}

func TestReconstructAbstractRejectsHugePosition(t *testing.T) {
	if got := reconstructAbstract(map[string][]int{"x": {2_000_000_000}}); got != "" {
		t.Fatalf("got %q, want empty abstract", got)
	}
}

func TestReconstructAbstractIgnoresNegativePositions(t *testing.T) {
	if got := reconstructAbstract(map[string][]int{"drop": {-1}, "Hello": {0}, "world": {1}}); got != "Hello world" {
		t.Fatalf("got %q, want 'Hello world'", got)
	}
}

func TestReconstructAbstractRejectsTooManyPositions(t *testing.T) {
	positions := make([]int, maxOpenAlexAbstractPairs+1)
	if got := reconstructAbstract(map[string][]int{"x": positions}); got != "" {
		t.Fatalf("got %q, want empty abstract", got)
	}
}

func TestResolveAbstractViaOpenAlex(t *testing.T) {
	srv := openAlexWorkServer(t, `{"results":[{"doi":"https://doi.org/10.1/x","title":"T","abstract_inverted_index":{"We":[0],"propose":[1],"Transformer":[2]}}]}`)
	withBase(t, &enrichOpenAlexBase, srv.URL)
	abstract, ok := resolveAbstractViaOpenAlex(context.Background(), http.DefaultClient, "10.1/x", "")
	if !ok || abstract != "We propose Transformer" {
		t.Errorf("abstract = %q ok=%v, want 'We propose Transformer'", abstract, ok)
	}
}

func TestResolveDOIViaOpenAlex(t *testing.T) {
	srv := openAlexWorkServer(t, `{"results":[{"doi":"https://doi.org/10.9/match","title":"Attention Is All You Need"}]}`)
	withBase(t, &enrichOpenAlexBase, srv.URL)
	data := map[string]any{"title": "attention is all you need"}
	doi, ok := resolveDOIViaOpenAlex(context.Background(), http.DefaultClient, data, "")
	if !ok || doi != "10.9/match" {
		t.Errorf("doi = %q ok=%v, want 10.9/match", doi, ok)
	}
}

// TestEnrichOpenAlexAbstractFallback: CrossRef has no abstract, so the resolver
// falls back to OpenAlex and records the provider in Source; with the fallback
// disabled it skips.
func TestEnrichOpenAlexAbstractFallback(t *testing.T) {
	cr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{}}`))
	}))
	t.Cleanup(cr.Close)
	withBase(t, &enrichCrossRefBase, cr.URL)
	oa := openAlexWorkServer(t, `{"results":[{"doi":"10.1/x","abstract_inverted_index":{"From":[0],"OpenAlex":[1]}}]}`)
	withBase(t, &enrichOpenAlexBase, oa.URL)

	data := map[string]any{"title": "T", "DOI": "10.1/x"}
	prop, reason := resolveEnrichment(context.Background(), http.DefaultClient, "missing_abstract", "K1", float64(1), data, "", true, false)
	if reason != "" {
		t.Fatalf("unexpected skip: %s", reason)
	}
	if prop.Source != "OpenAlex" {
		t.Errorf("source = %q, want OpenAlex", prop.Source)
	}
	if prop.Fields["abstractNote"] != "From OpenAlex" {
		t.Errorf("abstract = %v, want 'From OpenAlex'", prop.Fields["abstractNote"])
	}

	if _, reason := resolveEnrichment(context.Background(), http.DefaultClient, "missing_abstract", "K1", float64(1), data, "", false, false); reason == "" {
		t.Error("expected a skip when the OpenAlex fallback is disabled")
	}
}

// --validate is read-only and reports CrossRef discrepancies for DOI-bearing local items.
func TestItemsEnrichValidateReportsCrossRefTitleDiscrepancy(t *testing.T) {
	cr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"title":["Provider Title"],"DOI":"10.1/validate","published":{"date-parts":[[2024]]}}}`))
	}))
	t.Cleanup(cr.Close)
	withBase(t, &enrichCrossRefBase, cr.URL)
	oc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"doi":"10.1/validate"}]`))
	}))
	t.Cleanup(oc.Close)
	withBase(t, &enrichOpenCitationsBase, oc.URL)

	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "validate.toml"))
	dbPath := defaultDBPath("zotio")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := []json.RawMessage{
		json.RawMessage(`{"key":"KVAL","version":1,"data":{"key":"KVAL","itemType":"journalArticle","title":"Stored Title","date":"2024","DOI":"10.1/validate"}}`),
		json.RawMessage(`{"key":"KNODOI","version":1,"data":{"key":"KNODOI","itemType":"journalArticle","title":"No DOI"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = db.Close()

	flags := &rootFlags{asJSON: true}
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs([]string{"--validate"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	var report enrichValidationReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if report.Validated != 1 {
		t.Fatalf("validated = %d, want 1", report.Validated)
	}
	if len(report.Discrepancies) != 1 {
		t.Fatalf("discrepancies = %+v, want one title discrepancy", report.Discrepancies)
	}
	got := report.Discrepancies[0]
	if got.Key != "KVAL" || got.Field != "title" || got.Stored != "Stored Title" || got.Provider != "Provider Title" || got.Source != "CrossRef" {
		t.Errorf("discrepancy = %+v, want CrossRef title mismatch", got)
	}
	if len(report.UnverifiedDOIs) != 0 {
		t.Errorf("unverified DOIs = %+v, want none", report.UnverifiedDOIs)
	}
}
