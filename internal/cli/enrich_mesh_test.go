// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Tests for `items enrich --missing-mesh`: PubMed EFetch MeSH descriptors
// proposed as automatic mesh/<descriptor> tags.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

// disablePubMedPacing removes the 3 requests/second limiter so stub-server
// tests do not sleep, and restores it afterwards.
func disablePubMedPacing(t *testing.T) {
	t.Helper()
	saved := enrichPubMedLimiter
	enrichPubMedLimiter = nil
	t.Cleanup(func() { enrichPubMedLimiter = saved })
}

// meshArticleXML renders an EFetch reply for one record. A nested PMID inside
// CommentsCorrections is included so a decoder that matched PMID at any depth
// would pick the wrong identifier.
func meshArticleXML(pmid, doi string, descriptors ...string) string {
	var list strings.Builder
	for _, d := range descriptors {
		list.WriteString(`<MeshHeading><DescriptorName UI="D000001" MajorTopicYN="N">` + d + `</DescriptorName>` +
			`<QualifierName UI="Q000188" MajorTopicYN="Y">drug therapy</QualifierName></MeshHeading>`)
	}
	headings := ""
	if len(descriptors) > 0 {
		headings = `<MeshHeadingList>` + list.String() + `</MeshHeadingList>`
	}
	doiID := ""
	if doi != "" {
		doiID = `<ArticleId IdType="doi">` + doi + `</ArticleId>`
	}
	return `<?xml version="1.0" encoding="UTF-8"?><PubmedArticleSet><PubmedArticle><MedlineCitation Status="MEDLINE">` +
		`<PMID Version="1">` + pmid + `</PMID><Article><ArticleTitle>T</ArticleTitle></Article>` +
		`<CommentsCorrectionsList><CommentsCorrections RefType="Cites"><PMID Version="1">999</PMID></CommentsCorrections></CommentsCorrectionsList>` +
		headings + `</MedlineCitation><PubmedData><ArticleIdList><ArticleId IdType="pubmed">` + pmid + `</ArticleId>` +
		doiID + `</ArticleIdList></PubmedData></PubmedArticle></PubmedArticleSet>`
}

// pubmedStub serves ESearch and EFetch from fixed tables and records every
// request it receives.
type pubmedStub struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []*url.URL

	esearch map[string]string // DOI -> JSON reply
	efetch  map[string]string // PMID -> XML reply
	status  int               // when non-zero, every request answers with it
}

func newPubMedStub(t *testing.T) *pubmedStub {
	t.Helper()
	disablePubMedPacing(t)
	s := &pubmedStub{esearch: map[string]string{}, efetch: map[string]string{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		u := *r.URL
		s.requests = append(s.requests, &u)
		status := s.status
		s.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		switch r.URL.Path {
		case "/esearch.fcgi":
			term := r.URL.Query().Get("term")
			doi := strings.TrimSuffix(strings.TrimPrefix(term, `"`), `"[AID]`)
			body, ok := s.esearch[doi]
			if !ok {
				body = `{"esearchresult":{"count":"0","idlist":[]}}`
			}
			_, _ = w.Write([]byte(body))
		case "/efetch.fcgi":
			body, ok := s.efetch[r.URL.Query().Get("id")]
			if !ok {
				body = `<?xml version="1.0"?><PubmedArticleSet></PubmedArticleSet>`
			}
			_, _ = w.Write([]byte(body))
		default:
			t.Errorf("unexpected PubMed request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.srv.Close)
	withBase(t, &importPubMedBase, s.srv.URL)
	return s
}

func (s *pubmedStub) efetchIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, u := range s.requests {
		if u.Path == "/efetch.fcgi" {
			ids = append(ids, u.Query().Get("id"))
		}
	}
	sort.Strings(ids)
	return ids
}

func seedMeshStore(t *testing.T, items ...string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	raws := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		raws = append(raws, json.RawMessage(item))
	}
	if _, _, err := db.UpsertBatch("items", raws); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func runMeshEnrich(t *testing.T, flags *rootFlags, args ...string) (mutation.Envelope, error) {
	t.Helper()
	cmd := newItemsEnrichCmd(flags)
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	err := cmd.Execute()
	var env mutation.Envelope
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode envelope: %v; out=%s", decodeErr, out.String())
		}
	}
	return env, err
}

func opTags(op mutation.Op) []string {
	var tags []string
	for _, c := range op.Changes {
		if c.Field == "tags" {
			tags = append(tags, fmt.Sprint(c.Add))
		}
	}
	return tags
}

func TestItemsEnrichMissingMeshPlansAutomaticMeshTags(t *testing.T) {
	stub := newPubMedStub(t)
	stub.efetch["111"] = meshArticleXML("111", "10.1/has-pmid", "Humans", "Neoplasms")
	stub.efetch["222"] = meshArticleXML("222", "", "Mice")
	stub.esearch["10.1/doi-only"] = `{"esearchresult":{"count":"1","idlist":["333"]}}`
	stub.efetch["333"] = meshArticleXML("333", "10.1/DOI-only", "Humans", "Aged")

	seedMeshStore(t,
		// PMID in Zotero's own field; already carries one hand-typed tag, which
		// does not exclude it (unlike the untagged-only --missing-subjects).
		`{"key":"KPMID","version":4,"data":{"key":"KPMID","itemType":"journalArticle","title":"By PMID field","PMID":"111","DOI":"10.1/has-pmid","tags":[{"tag":"mine"}],"dateAdded":"2026-01-04T00:00:00Z"}}`,
		// Legacy PMID line in Extra.
		`{"key":"KEXTRA","version":4,"data":{"key":"KEXTRA","itemType":"journalArticle","title":"By Extra line","extra":"Citation Key: a2020\nPMID: 222","tags":[],"dateAdded":"2026-01-03T00:00:00Z"}}`,
		// DOI only: the PMID has to come from ESearch.
		`{"key":"KDOI","version":4,"data":{"key":"KDOI","itemType":"journalArticle","title":"By DOI","DOI":"https://doi.org/10.1/doi-only","tags":[],"dateAdded":"2026-01-02T00:00:00Z"}}`,
		// Already carries a mesh/ tag: finished by an earlier run.
		`{"key":"KDONE","version":4,"data":{"key":"KDONE","itemType":"journalArticle","title":"Done","PMID":"444","tags":[{"tag":"mesh/Humans"}],"dateAdded":"2026-01-05T00:00:00Z"}}`,
		// No identifier PubMed can resolve.
		`{"key":"KNOID","version":4,"data":{"key":"KNOID","itemType":"book","title":"No id","extra":"PMCID: PMC123","tags":[],"dateAdded":"2026-01-06T00:00:00Z"}}`,
		// A child attachment is never a candidate.
		`{"key":"KATT","version":4,"data":{"key":"KATT","itemType":"attachment","parentItem":"KPMID","PMID":"555","title":"pdf"}}`,
	)

	env, err := runMeshEnrich(t, &rootFlags{asJSON: true, maxChanges: -1}, "--missing-mesh", "--email", "me@example.com")
	if err != nil {
		t.Fatalf("enrich --missing-mesh: %v", err)
	}
	if env.Result != nil {
		t.Fatalf("result = %+v, want a preview with no apply", env.Result)
	}
	got := map[string][]string{}
	for _, op := range env.Plan.Operations {
		got[op.Key] = opTags(op)
		for _, c := range op.Changes {
			// Automatic type 1 keeps managed terms apart from typed tags.
			if c.Field != "tags" || c.TagType != 1 {
				t.Errorf("%s: change %+v, want only automatic tag additions", op.Key, c)
			}
		}
	}
	want := map[string][]string{
		"KPMID":  {"mesh/Humans", "mesh/Neoplasms"},
		"KEXTRA": {"mesh/Mice"},
		"KDOI":   {"mesh/Humans", "mesh/Aged"},
	}
	if len(got) != len(want) {
		t.Fatalf("planned = %v, want exactly %v", got, want)
	}
	for key, tags := range want {
		if strings.Join(got[key], "|") != strings.Join(tags, "|") {
			t.Errorf("%s tags = %v, want %v (descriptors only, no qualifiers)", key, got[key], tags)
		}
	}

	// KDONE, KNOID and the attachment never reach PubMed; 999 is the nested
	// CommentsCorrections PMID and must never be requested either.
	if ids := stub.efetchIDs(); strings.Join(ids, ",") != "111,222,333" {
		t.Errorf("EFetch ids = %v, want 111,222,333", ids)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for _, u := range stub.requests {
		q := u.Query()
		if q.Get("tool") != "zotio" || q.Get("email") != "me@example.com" {
			t.Errorf("request %s lacks the NCBI tool/email identification", u)
		}
		if u.Path == "/esearch.fcgi" && q.Get("term") != `"10.1/doi-only"[AID]` {
			t.Errorf("ESearch term = %q, want the normalized DOI in the [AID] field", q.Get("term"))
		}
	}
}

func TestItemsEnrichMissingMeshAppliesOnlyWithYes(t *testing.T) {
	stub := newPubMedStub(t)
	stub.efetch["111"] = meshArticleXML("111", "", "Humans", "Neoplasms")
	seedMeshStore(t,
		`{"key":"KPMID","version":4,"data":{"key":"KPMID","itemType":"journalArticle","title":"T","PMID":"111","tags":[]}}`)

	var mu sync.Mutex
	var patches []map[string]any
	var precondition string
	zsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			// The live item gained tags since the last sync, one of them a
			// term this run would otherwise propose.
			w.Header().Set("Last-Modified-Version", "11")
			_, _ = w.Write([]byte(`{"key":"KPMID","version":11,"data":{"key":"KPMID","itemType":"journalArticle","extra":"Citation Key: k2020","tags":[{"tag":"typed-by-hand"},{"tag":"mesh/Humans","type":1}]}}`))
		case http.MethodPatch:
			precondition = r.Header.Get("If-Unmodified-Since-Version")
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			patches = append(patches, body)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(zsrv.Close)
	t.Setenv("ZOTERO_BASE_URL", zsrv.URL)

	// Preview, then --dry-run with --yes: neither may write.
	for _, flags := range []*rootFlags{
		{asJSON: true, maxChanges: -1},
		{asJSON: true, yes: true, dryRun: true, maxChanges: -1},
	} {
		env, err := runMeshEnrich(t, flags, "--missing-mesh")
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		if len(env.Plan.Operations) != 1 || env.Result != nil {
			t.Fatalf("preview plan=%+v result=%+v, want one planned op and no result", env.Plan, env.Result)
		}
	}
	mu.Lock()
	if len(patches) != 0 {
		t.Fatalf("preview wrote %d patch(es); a write needs --yes", len(patches))
	}
	mu.Unlock()

	env, err := runMeshEnrich(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--missing-mesh")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("result = %+v, want one applied write", env.Result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(patches) != 1 {
		t.Fatalf("patches = %d, want exactly one", len(patches))
	}
	if precondition != "11" {
		t.Errorf("If-Unmodified-Since-Version = %q, want the write plane's 11", precondition)
	}
	rawTags, _ := patches[0]["tags"].([]any)
	types := map[string]float64{}
	count := map[string]int{}
	for _, raw := range rawTags {
		obj, _ := raw.(map[string]any)
		name, _ := obj["tag"].(string)
		typ, _ := obj["type"].(float64)
		types[name] = typ
		count[name]++
	}
	if _, kept := types["typed-by-hand"]; !kept {
		t.Errorf("PATCH tags = %v, want the live hand-typed tag preserved", types)
	}
	if count["mesh/Humans"] != 1 {
		t.Errorf("mesh/Humans appears %d times, want the live copy kept without a duplicate", count["mesh/Humans"])
	}
	if got, ok := types["mesh/Neoplasms"]; !ok || got != 1 {
		t.Errorf("PATCH tags = %v, want mesh/Neoplasms added as automatic type 1", types)
	}
	extra := fmt.Sprint(patches[0]["extra"])
	if !strings.Contains(extra, "Citation Key: k2020") {
		t.Errorf("extra = %q, want the live Extra preserved", extra)
	}
	// One term was added; the provenance must credit PubMed and the PMID, and
	// must not claim the term that was already on the live item.
	if !strings.Contains(extra, "MeSH tags (1)") || !strings.Contains(extra, "via PubMed (PMID 111)") {
		t.Errorf("extra = %q, want MeSH provenance naming PubMed and the PMID", extra)
	}
}

func TestItemsEnrichMissingMeshProviderFailureIsAnError(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			stub := newPubMedStub(t)
			stub.status = status
			seedMeshStore(t,
				`{"key":"KPMID","version":4,"data":{"key":"KPMID","itemType":"journalArticle","title":"T","PMID":"111","tags":[]}}`)

			env, err := runMeshEnrich(t, &rootFlags{asJSON: true, maxChanges: -1}, "--missing-mesh")
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) {
				t.Fatalf("err = %v, want the provider's HTTP %d reported as an error", err, status)
			}
			if !strings.Contains(err.Error(), "missing_mesh") {
				t.Errorf("err = %v, want it to name the failing category", err)
			}
			if len(env.Plan.Operations) != 0 {
				t.Errorf("plan = %+v, want no proposal for a failed lookup", env.Plan.Operations)
			}
			// The failure is an error entry on the envelope (ok:false) and the
			// upstream exit code, not a benign journal skip.
			if env.OK || env.Error == nil || !strings.Contains(env.Error.Message, fmt.Sprintf("HTTP %d", status)) {
				t.Errorf("envelope ok=%v error=%+v, want ok:false with the provider failure", env.OK, env.Error)
			}
			if ExitCode(err) != 5 {
				t.Errorf("ExitCode = %d, want 5 (upstream)", ExitCode(err))
			}
			if journal, _ := env.Journal.(map[string]any); journal["skipped"] != nil {
				t.Errorf("journal = %+v, want the failure out of the benign skip list", env.Journal)
			}
		})
	}
}

func TestResolveMeshViaPubMedOutcomes(t *testing.T) {
	ctx := context.Background()
	stub := newPubMedStub(t)
	stub.efetch["111"] = meshArticleXML("111", "", "Humans", "Neoplasms", "Humans", "  ")
	stub.efetch["112"] = meshArticleXML("112", "")
	stub.efetch["113"] = `<?xml version="1.0"?><eFetchResult><ERROR>UID=113: cannot get document summary</ERROR></eFetchResult>`
	stub.efetch["114"] = `<?xml version="1.0"?><eFetchResult><ERROR>Backend failed</ERROR></eFetchResult>`
	stub.efetch["115"] = `<html>not xml`
	stub.efetch["333"] = meshArticleXML("333", "10.9/other", "Humans")
	stub.esearch["10.1/ambiguous"] = `{"esearchresult":{"count":"2","idlist":["1","2"]}}`
	stub.esearch["10.1/mismatch"] = `{"esearchresult":{"count":"1","idlist":["333"]}}`
	stub.esearch["10.1/err"] = `{"esearchresult":{"ERROR":"Invalid query"}}`
	stub.esearch["10.1/ratelimit"] = `{"error":"API rate limit exceeded"}`

	cases := []struct {
		name       string
		data       map[string]any
		wantTags   string
		wantReason string
		wantErr    string
	}{
		{name: "indexed record, dedup, blanks dropped", data: map[string]any{"PMID": "111"}, wantTags: "mesh/Humans,mesh/Neoplasms"},
		{name: "existing tag not re-proposed", data: map[string]any{"PMID": "111", "tags": []any{map[string]any{"tag": "mesh/Humans"}}}, wantTags: "mesh/Neoplasms"},
		{name: "every term already present", data: map[string]any{"PMID": "111", "tags": []any{map[string]any{"tag": "mesh/Humans"}, map[string]any{"tag": "mesh/Neoplasms"}}}, wantReason: "already carries every MeSH tag"},
		{name: "a plain Humans tag is not a mesh tag", data: map[string]any{"PMID": "111", "tags": []any{map[string]any{"tag": "Humans"}}}, wantTags: "mesh/Humans,mesh/Neoplasms"},
		{name: "not yet indexed", data: map[string]any{"PMID": "112"}, wantReason: "no MeSH headings"},
		{name: "unknown PMID is a no-match", data: map[string]any{"PMID": "999999"}, wantReason: "no record for PMID 999999"},
		{name: "cannot get document summary is a no-match", data: map[string]any{"PMID": "113"}, wantReason: "no record for PMID 113"},
		{name: "other NCBI error is an error", data: map[string]any{"PMID": "114"}, wantErr: "Backend failed"},
		{name: "undecodable body is an error", data: map[string]any{"PMID": "115"}, wantErr: "parsing XML"},
		{name: "no identifiers", data: map[string]any{"title": "x"}, wantReason: "no PMID or DOI"},
		{name: "non-numeric PMID falls back to DOI", data: map[string]any{"PMID": "abc"}, wantReason: "no PMID or DOI"},
		{name: "DOI with no PubMed record", data: map[string]any{"DOI": "10.1/missing"}, wantReason: "no record for this DOI"},
		{name: "ambiguous DOI", data: map[string]any{"DOI": "10.1/ambiguous"}, wantReason: "more than one PubMed record"},
		{name: "DOI the record does not list", data: map[string]any{"DOI": "10.1/mismatch"}, wantReason: "does not list DOI"},
		{name: "ESearch ERROR is an error", data: map[string]any{"DOI": "10.1/err"}, wantErr: "Invalid query"},
		{name: "ESearch 200 rate-limit body is an error", data: map[string]any{"DOI": "10.1/ratelimit"}, wantErr: "rate limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tags, _, reason, err := resolveMeshViaPubMed(ctx, http.DefaultClient, tc.data, "")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want a clean outcome", err)
			}
			if tc.wantReason != "" {
				if len(tags) != 0 || !strings.Contains(reason, tc.wantReason) {
					t.Fatalf("tags=%v reason=%q, want a skip containing %q", tags, reason, tc.wantReason)
				}
				return
			}
			if reason != "" || strings.Join(tags, ",") != tc.wantTags {
				t.Fatalf("tags=%v reason=%q, want %s", tags, reason, tc.wantTags)
			}
		})
	}
}

func TestResolveMeshViaPubMedTransportAndStatusErrors(t *testing.T) {
	ctx := context.Background()
	data := map[string]any{"PMID": "111"}

	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		stub := newPubMedStub(t)
		stub.status = status
		if _, _, reason, err := resolveMeshViaPubMed(ctx, http.DefaultClient, data, ""); err == nil || reason != "" {
			t.Errorf("HTTP %d: err=%v reason=%q, want an error and no skip reason", status, err, reason)
		}
		// The DOI route fails the same way at ESearch.
		if _, _, reason, err := resolveMeshViaPubMed(ctx, http.DefaultClient, map[string]any{"DOI": "10.1/x"}, ""); err == nil || reason != "" {
			t.Errorf("HTTP %d via DOI: err=%v reason=%q, want an error and no skip reason", status, err, reason)
		}
	}

	// 404 and 410 are PubMed saying the record does not exist.
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		stub := newPubMedStub(t)
		stub.status = status
		tags, _, reason, err := resolveMeshViaPubMed(ctx, http.DefaultClient, data, "")
		if err != nil || len(tags) != 0 || reason == "" {
			t.Errorf("HTTP %d: tags=%v reason=%q err=%v, want a clean no-match", status, tags, reason, err)
		}
	}

	// A refused connection and a cancelled context cannot answer either.
	withBase(t, &importPubMedBase, "http://127.0.0.1:1")
	if _, _, _, err := resolveMeshViaPubMed(ctx, http.DefaultClient, data, ""); err == nil {
		t.Error("refused connection: want an error")
	}
	stub := newPubMedStub(t)
	stub.efetch["111"] = meshArticleXML("111", "", "Humans")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, _, err := resolveMeshViaPubMed(cancelled, http.DefaultClient, data, ""); err == nil {
		t.Error("cancelled context: want an error")
	}
}

func TestPubMedIDFromItem(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
		want string
	}{
		{"native field", map[string]any{"PMID": " 123 "}, "123"},
		{"field wins over Extra", map[string]any{"PMID": "1", "extra": "PMID: 2"}, "1"},
		{"extra line", map[string]any{"extra": "Citation Key: x\nPMID: 456\nfoo"}, "456"},
		{"extra no space", map[string]any{"extra": "PMID:789"}, "789"},
		{"PMCID is not a PMID", map[string]any{"extra": "PMCID: PMC12345"}, ""},
		{"prefixed label", map[string]any{"extra": "xPMID: 55"}, ""},
		{"non numeric", map[string]any{"PMID": "n/a"}, ""},
	}
	for _, tc := range cases {
		if got := pubmedIDFromItem(tc.data); got != tc.want {
			t.Errorf("%s: pubmedIDFromItem = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The queue is a filter, not a second definition of "missing": scope,
// collection, key set, and limit must all narrow the same predicate.
func TestMissingMeshQueueHonorsScopeKeysAndLimit(t *testing.T) {
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	items := []json.RawMessage{
		json.RawMessage(`{"key":"KA","version":1,"data":{"key":"KA","itemType":"journalArticle","title":"A","PMID":"1","collections":["COLX"],"dateAdded":"2026-01-03T00:00:00Z"}}`),
		json.RawMessage(`{"key":"KB","version":1,"data":{"key":"KB","itemType":"journalArticle","title":"B","DOI":"10.1/b","collections":["COLX"],"dateAdded":"2026-01-02T00:00:00Z"}}`),
		json.RawMessage(`{"key":"KC","version":1,"data":{"key":"KC","itemType":"journalArticle","title":"C","extra":"PMID: 3","collections":["COLY"],"dateAdded":"2026-01-01T00:00:00Z"}}`),
		json.RawMessage(`{"key":"KD","version":1,"data":{"key":"KD","itemType":"journalArticle","title":"D","PMID":"4","tags":[{"tag":"mesh/Humans"}],"collections":["COLX"],"dateAdded":"2026-01-04T00:00:00Z"}}`),
		json.RawMessage(`{"key":"KE","version":1,"data":{"key":"KE","itemType":"journalArticle","title":"E","tags":[],"collections":["COLX"],"dateAdded":"2026-01-05T00:00:00Z"}}`),
		json.RawMessage(`{"key":"KN","version":1,"data":{"key":"KN","itemType":"note","title":"N","PMID":"9","dateAdded":"2026-01-06T00:00:00Z"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	q := localQueryStore{db}
	keysOf := func(rows []map[string]any) string {
		var keys []string
		for _, row := range rows {
			keys = append(keys, sqlStringValue(row["key"]))
		}
		return strings.Join(keys, ",")
	}

	all, err := enrichWorkQueue(q, "missing_mesh", 0, "")
	if err != nil {
		t.Fatalf("enrichWorkQueue: %v", err)
	}
	// KD already has a mesh/ tag, KE has no identifier, KN is a note.
	if got := keysOf(all); got != "KA,KB,KC" {
		t.Fatalf("queue = %s, want KA,KB,KC newest first", got)
	}
	limited, _ := enrichWorkQueue(q, "missing_mesh", 2, "")
	if got := keysOf(limited); got != "KA,KB" {
		t.Errorf("limited queue = %s, want KA,KB", got)
	}
	scoped, _ := enrichWorkQueue(q, "missing_mesh", 0, "COLX")
	if got := keysOf(scoped); got != "KA,KB" {
		t.Errorf("collection queue = %s, want KA,KB", got)
	}
	keyed, err := enrichWorkQueueForKeys(q, "missing_mesh", 0, "", map[string]bool{"KB": true, "KC": true, "KD": true, "KE": true})
	if err != nil {
		t.Fatalf("enrichWorkQueueForKeys: %v", err)
	}
	if got := keysOf(keyed); got != "KB,KC" {
		t.Errorf("keyed queue = %s, want KB,KC (naming KD or KE does not admit them)", got)
	}
	keyedLimited, _ := enrichWorkQueueForKeys(q, "missing_mesh", 1, "COLX", map[string]bool{"KA": true, "KB": true, "KC": true})
	if got := keysOf(keyedLimited); got != "KA" {
		t.Errorf("keyed+collection+limit queue = %s, want KA", got)
	}
}

func TestItemsEnrichMissingMeshIsAnAcceptedCategory(t *testing.T) {
	cmd := newItemsEnrichCmd(&rootFlags{})
	if cmd.Flags().Lookup("missing-mesh") == nil {
		t.Fatal("items enrich has no --missing-mesh flag")
	}
	if !strings.Contains(cmd.Long, "--missing-mesh") {
		t.Error("items enrich help does not describe --missing-mesh")
	}
}
