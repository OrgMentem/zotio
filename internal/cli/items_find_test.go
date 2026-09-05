// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"zotio/internal/store"
)

func TestExtraContainsExactTokenBoundary(t *testing.T) {
	cases := []struct {
		name   string
		extra  string
		prefix string
		token  string
		want   bool
	}{
		{"citation exact newline", "Citation Key: smith2023\n", "Citation Key: ", "smith2023", true},
		{"citation exact end-of-string", "Citation Key: smith2023", "Citation Key: ", "smith2023", true},
		{"citation prefix should not match", "Citation Key: smith2023a", "Citation Key: ", "smith2023", false},
		{"citation exact with trailing space", "Citation Key: smith2023 ", "Citation Key: ", "smith2023", true},
		{"citation with second line after token", "Citation Key: smith2023a\nCitation Key: smith2023\n", "Citation Key: ", "smith2023", true},
		{"pmid exact", "PMID: 123\n", "PMID: ", "123", true},
		{"pmid prefix rejected", "PMID: 12345", "PMID: ", "123", false},
		{"pmid prefix followed by exact later", "PMID: 12345\nPMID: 123\n", "PMID: ", "123", true},
		{"arxiv exact", "arXiv: 2006.11239\n", "arXiv: ", "2006.11239", true},
		{"arxiv prefix rejected", "arXiv: 2006.11239", "arXiv: ", "2006.11", false},
		{"arxiv exact end-of-string", "arXiv: 2006.11", "arXiv: ", "2006.11", true},
		{"empty token no match", "Citation Key: smith2023", "Citation Key: ", "", false},
		{"token with tab boundary", "PMID: 123\tother", "PMID: ", "123", true},
		{"token with cr boundary", "PMID: 123\rother", "PMID: ", "123", true},
		{"embedded label rejected", "NotPMID: 123", "PMID: ", "123", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extraContainsExactToken(tc.extra, tc.prefix, tc.token)
			if got != tc.want {
				t.Fatalf("extraContainsExactToken(%q, %q, %q) = %v, want %v", tc.extra, tc.prefix, tc.token, got, tc.want)
			}
		})
	}
}

func TestFindRowMatchesExact_CitekeyPrefixOverMatch(t *testing.T) {
	rows := []map[string]any{
		{"id": "A", "data": `{"data":{"key":"A","itemType":"journalArticle","extra":"Citation Key: smith2023"}}`},
		{"id": "B", "data": `{"data":{"key":"B","itemType":"journalArticle","extra":"Citation Key: smith2023a"}}`},
	}
	// Searching smith2023 must match only A, not B.
	if !findRowMatchesExact(rows[0], findItemsQuery{Citekey: "smith2023"}) {
		t.Fatal("row A (exact citekey) should match")
	}
	if findRowMatchesExact(rows[1], findItemsQuery{Citekey: "smith2023"}) {
		t.Fatal("row B (smith2023a) must NOT match smith2023 — prefix over-match")
	}
}

func TestFindRowMatchesExact_PMIDPrefixOverMatch(t *testing.T) {
	rows := []map[string]any{
		{"id": "C", "data": `{"data":{"key":"C","itemType":"journalArticle","extra":"PMID: 123"}}`},
		{"id": "D", "data": `{"data":{"key":"D","itemType":"journalArticle","extra":"PMID: 12345"}}`},
	}
	if !findRowMatchesExact(rows[0], findItemsQuery{PMID: "123"}) {
		t.Fatal("PMID 123 exact row should match")
	}
	if findRowMatchesExact(rows[1], findItemsQuery{PMID: "123"}) {
		t.Fatal("PMID 12345 row must NOT match query 123 — prefix over-match")
	}
}

func TestFindRowMatchesExact_ArXivPrefixOverMatch(t *testing.T) {
	rows := []map[string]any{
		{"id": "E", "data": `{"data":{"key":"E","itemType":"journalArticle","extra":"arXiv: 2006.11"}}`},
		{"id": "F", "data": `{"data":{"key":"F","itemType":"journalArticle","extra":"arXiv: 2006.11239"}}`},
	}
	if !findRowMatchesExact(rows[0], findItemsQuery{ArXiv: "2006.11"}) {
		t.Fatal("arXiv 2006.11 exact row should match")
	}
	if findRowMatchesExact(rows[1], findItemsQuery{ArXiv: "2006.11"}) {
		t.Fatal("arXiv 2006.11239 must NOT match query 2006.11 — prefix over-match")
	}
	// Reverse: searching the longer id should match only F.
	if findRowMatchesExact(rows[0], findItemsQuery{ArXiv: "2006.11239"}) {
		t.Fatal("arXiv 2006.11 must NOT match query 2006.11239")
	}
	if !findRowMatchesExact(rows[1], findItemsQuery{ArXiv: "2006.11239"}) {
		t.Fatal("arXiv 2006.11239 exact row should match")
	}

	for _, extra := range []string{"arXiv: 2006.11abc", "https://arxiv.org/abs/2006.11abc"} {
		row := map[string]any{"data": `{"data":{"extra":` + fmt.Sprintf("%q", extra) + `}}`}
		if findRowMatchesExact(row, findItemsQuery{ArXiv: "2006.11"}) {
			t.Fatalf("stored arXiv prefix %q matched 2006.11", extra)
		}
	}
}
func TestFindRowMatchesExact_ExtraIdentifierSpellings(t *testing.T) {
	cases := []struct {
		name    string
		row     map[string]any
		arxiv   string
		pmid    string
		citekey string
		want    bool
	}{
		{
			"arXiv colon-tight",
			map[string]any{"data": `{"data":{"extra":"arXiv:0910.4514 [math-ph]"}}`},
			"0910.4514", "", "", true,
		},
		{
			"arXiv spaced",
			map[string]any{"data": `{"data":{"extra":"arXiv: 0910.4514 [math-ph]"}}`},
			"0910.4514", "", "", true,
		},
		{
			"PMID colon-tight",
			map[string]any{"data": `{"data":{"extra":"PMID:12345678"}}`},
			"", "12345678", "", true,
		},
		{
			"PMID spaced",
			map[string]any{"data": `{"data":{"extra":"PMID: 12345678"}}`},
			"", "12345678", "", true,
		},
		{
			"citation key colon-tight",
			map[string]any{"data": `{"data":{"extra":"Citation Key:smith2023"}}`},
			"", "", "smith2023", true,
		},
		{
			"citation key spaced",
			map[string]any{"data": `{"data":{"extra":"Citation Key: smith2023"}}`},
			"", "", "smith2023", true,
		},
		{
			"arXiv colon-tight prefix rejected",
			map[string]any{"data": `{"data":{"extra":"arXiv:0910.45141"}}`},
			"0910.4514", "", "", false,
		},
		{
			"PMID colon-tight prefix rejected",
			map[string]any{"data": `{"data":{"extra":"PMID:12345"}}`},
			"", "123", "", false,
		},
		{
			"citation key colon-tight prefix rejected",
			map[string]any{"data": `{"data":{"extra":"Citation Key:smith2023a"}}`},
			"", "", "smith2023", false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findRowMatchesExact(tc.row, findItemsQuery{ArXiv: tc.arxiv, PMID: tc.pmid, Citekey: tc.citekey})
			if got != tc.want {
				t.Fatalf("findRowMatchesExact(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// The dedicated citationKey field is a separate matcher branch from the
// "Citation Key:" token in extra, which TestFindRowMatchesExact_CitekeyPrefixOverMatch
// already covers. Boundary, not prefix, in both directions.
func TestFindRowMatchesExact_CitationKeyFieldExact(t *testing.T) {
	exact := map[string]any{"data": `{"data":{"key":"A","itemType":"journalArticle","citationKey":"smith2023"}}`}
	longer := map[string]any{"data": `{"data":{"key":"B","itemType":"journalArticle","citationKey":"smith2023a"}}`}

	if !findRowMatchesExact(exact, findItemsQuery{Citekey: "smith2023"}) {
		t.Fatal("citationKey smith2023 should match query smith2023")
	}
	if findRowMatchesExact(longer, findItemsQuery{Citekey: "smith2023"}) {
		t.Fatal("citationKey smith2023a must NOT match query smith2023 — prefix over-match")
	}
	if !findRowMatchesExact(longer, findItemsQuery{Citekey: "smith2023a"}) {
		t.Fatal("citationKey smith2023a should match query smith2023a")
	}
}

// archiveID is likewise its own branch, and it carries the "arXiv:" prefix inline
// rather than as a separate extra token.
func TestFindRowMatchesExact_ArchiveIDExact(t *testing.T) {
	exact := map[string]any{"data": `{"data":{"key":"X","itemType":"journalArticle","archiveID":"arXiv:2006.1100"}}`}
	longer := map[string]any{"data": `{"data":{"key":"Y","itemType":"journalArticle","archiveID":"arXiv:2006.11001"}}`}

	if !findRowMatchesExact(exact, findItemsQuery{ArXiv: "2006.1100"}) {
		t.Fatal("archiveID arXiv:2006.1100 should match query 2006.1100")
	}
	if findRowMatchesExact(longer, findItemsQuery{ArXiv: "2006.1100"}) {
		t.Fatal("archiveID arXiv:2006.11001 must NOT match query 2006.1100 — prefix over-match")
	}
	if !findRowMatchesExact(longer, findItemsQuery{ArXiv: "2006.11001"}) {
		t.Fatal("archiveID arXiv:2006.11001 should match query 2006.11001")
	}
}

func TestFindRowMatchesExactURLTitleAndOpenAlex(t *testing.T) {
	row := map[string]any{"data": `{"data":{
		"url":"HTTPS://Example.ORG/paper/#section",
		"title":"  The Exact Title  ",
		"extra":"openalex id: w2741809807"
	}}`}
	tests := []struct {
		name  string
		query findItemsQuery
		want  bool
	}{
		{name: "normalized URL", query: findItemsQuery{URL: "https://example.org/paper"}, want: true},
		{name: "exact title", query: findItemsQuery{Title: "the exact title"}, want: true},
		{name: "OpenAlex ID", query: findItemsQuery{OpenAlex: "W2741809807"}, want: true},
		{name: "title remains exact", query: findItemsQuery{Title: "Exact Title"}, want: false},
		{name: "OpenAlex prefix rejected", query: findItemsQuery{OpenAlex: "W274180980"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, err := tt.query.normalized()
			if err != nil {
				t.Fatalf("normalize query: %v", err)
			}
			if got := findRowMatchesExact(row, query); got != tt.want {
				t.Fatalf("findRowMatchesExact() = %v, want %v", got, tt.want)
			}
		})
	}
	spoof := map[string]any{"data": `{"data":{"extra":"NotOpenAlex: W2741809807"}}`}
	if findRowMatchesExact(spoof, findItemsQuery{OpenAlex: "W2741809807"}) {
		t.Fatal("OpenAlex label embedded in another label matched")
	}
}
func TestNormalizeFindURLOnlyRemovesOneLiteralTrailingSlash(t *testing.T) {
	canonical := normalizeFindURL("https://example.org/paper")
	if got := normalizeFindURL("HTTPS://Example.ORG/paper/#fragment"); got != canonical {
		t.Fatalf("literal trailing slash = %q, want %q", got, canonical)
	}
	for _, distinct := range []string{
		"https://example.org/paper//",
		"https://example.org/paper%2F",
	} {
		if got := normalizeFindURL(distinct); got == canonical {
			t.Fatalf("distinct URL %q normalized to %q", distinct, got)
		}
	}
}

func TestFindRowMatchesExactIgnoresMalformedUnrelatedFields(t *testing.T) {
	row := map[string]any{"data": `{"data":{"DOI":"10.9999/other","title":[]}}`}
	if findRowMatchesExact(row, findItemsQuery{DOI: "10.1234/query"}) {
		t.Fatal("malformed unrelated title made a different DOI match")
	}
	if !findRowMatchesExact(row, findItemsQuery{DOI: "10.9999/other"}) {
		t.Fatal("malformed unrelated title hid the exact DOI match")
	}
}

func TestFindItemsQueryNormalizesExternalIdentifiers(t *testing.T) {
	query, err := (findItemsQuery{
		DOI:      "https://doi.org/10.1234/ABC",
		ArXiv:    "https://ARXIV.ORG/PDF/2401.00001v3.pdf",
		URL:      "HTTPS://Example.ORG/path/#fragment",
		OpenAlex: "https://openalex.org/w2741809807",
	}).normalized()
	if err != nil {
		t.Fatalf("normalize query: %v", err)
	}
	if query.DOI != "10.1234/ABC" || query.ArXiv != "2401.00001" ||
		query.URL != "https://example.org/path" || query.OpenAlex != "W2741809807" {
		t.Fatalf("normalized query = %+v", query)
	}

	_, err = (findItemsQuery{OpenAlex: "A123"}).normalized()
	if err == nil || !strings.Contains(err.Error(), "--openalex must be a work ID") {
		t.Fatalf("invalid OpenAlex error = %v", err)
	}
	for _, invalid := range []string{
		"https://arxiv.org/abs/2401.00001junk",
		"arXiv:2401.00001junk",
		"https://notarxiv.org/abs/2401.00001",
	} {
		_, err := (findItemsQuery{ArXiv: invalid}).normalized()
		if err == nil || !strings.Contains(err.Error(), "--arxiv must be an ID") {
			t.Fatalf("invalid arXiv input %q error = %v", invalid, err)
		}
	}
}

func TestExtractArxivIDFromStringRequiresHostAndIDBoundaries(t *testing.T) {
	for input, want := range map[string]string{
		"See https://arxiv.org/abs/2401.00001v2 for details": "2401.00001",
		"https://arxiv.org/abs/2401.00001/":                  "2401.00001",
		"arXiv: 0910.4514 [math-ph]":                         "0910.4514",
		"https://notarxiv.org/abs/2401.00001":                "",
		"https://not_arxiv.org/abs/2401.00001":               "",
		"https://evil.example/arxiv.org/abs/2401.00001":      "",
		"https://arxiv.org/abs/2401.00001junk":               "",
		"https://arxiv.org/abs/2401.00001/evil":              "",
		"arXiv: 2401.00001junk":                              "",
		"arXiv: 2401.00001/evil":                             "",
		"not_arXiv: 2401.00001":                              "",
	} {
		if got := extractArxivIDFromString(input); got != want {
			t.Fatalf("extractArxivIDFromString(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestQueryFindItemsExactHonorsCancellation(t *testing.T) {
	db, err := store.OpenWithContext(t.Context(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	noMatches, err := queryFindItemsExact(t.Context(), localQueryStore{Store: db}, findItemsQuery{URL: "https://example.org"})
	if err != nil {
		t.Fatalf("queryFindItemsExact empty: %v", err)
	}
	if noMatches == nil || len(noMatches) != 0 {
		t.Fatalf("empty exact lookup = %#v, want non-nil empty slice", noMatches)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := queryFindItemsExact(ctx, localQueryStore{Store: db}, findItemsQuery{URL: "https://example.org"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("queryFindItemsExact canceled error = %v, want context.Canceled", err)
	}
}

func TestItemsFindCommandLooksUpURLTitleAndOpenAlex(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	dbPath := filepath.Join(dataHome, "zotio", "data.db")
	db, err := store.OpenWithContext(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"MATCH","data":{"key":"MATCH","itemType":"journalArticle","title":"\tThe Exact Title\u00a0","url":"HTTPS://Example.ORG/paper/#section","DOI":"https://doi.org/10.1234/ABC","extra":"OpenAlex ID: W2741809807"}}`),
		json.RawMessage(`{"key":"OTHER","data":{"key":"OTHER","itemType":"journalArticle","title":"The Exact Title Extended","url":"https://example.org/other","DOI":"10.1234/ABCD","extra":"OpenAlex ID: W27418098070"}}`),
	}); err != nil {
		t.Fatalf("seed lookup items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	tests := []struct {
		name string
		args []string
	}{
		{name: "URL", args: []string{"--url", "https://example.org/paper"}},
		{name: "title", args: []string{"--title", " the exact title "}},
		{name: "OpenAlex", args: []string{"--openalex", "https://openalex.org/W2741809807"}},
		{name: "normalized DOI", args: []string{"--doi", "10.1234/ABC"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newItemsFindCmd(&rootFlags{asJSON: true})
			cmd.SetArgs(tt.args)
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&bytes.Buffer{})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			var got struct {
				Results []struct {
					Key string `json:"key"`
				} `json:"results"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("decode output %q: %v", stdout.String(), err)
			}
			if len(got.Results) != 1 || got.Results[0].Key != "MATCH" {
				t.Fatalf("results = %+v, want MATCH", got.Results)
			}
		})
	}
}

// seedNearTitleStore builds a store whose titles exercise the three outcomes
// the near-match block has to separate: an exact hit, a mistyped hit, and an
// item that merely shares vocabulary. It returns the store path so a test can
// damage the store itself.
func seedNearTitleStore(t *testing.T) string {
	t.Helper()
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	dbPath := filepath.Join(dataHome, "zotio", "data.db")
	db, err := store.OpenWithContext(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"ATTN","data":{"key":"ATTN","itemType":"journalArticle","title":"Attention Is All You Need","date":"2017-06-12"}}`),
		json.RawMessage(`{"key":"SUBT","data":{"key":"SUBT","itemType":"journalArticle","title":"Attention Is All You Need: A Transformer Study","date":"n.d."}}`),
		json.RawMessage(`{"key":"FRST","data":{"key":"FRST","itemType":"journalArticle","title":"Random Forests","date":"2001"}}`),
	}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	return dbPath
}

func runItemsFind(t *testing.T, flags *rootFlags, args ...string) (string, string) {
	t.Helper()
	cmd := newItemsFindCmd(flags)
	cmd.SetArgs(args)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(%v) error = %v", args, err)
	}
	return stdout.String(), stderr.String()
}

type findEnvelope struct {
	Results []struct {
		Key string `json:"key"`
	} `json:"results"`
	Near     []nearTitleMatch   `json:"near_title_matches"`
	NearKeys []nearCiteKeyMatch `json:"near_citekey_matches"`
	Meta     struct {
		TitleLookup   string `json:"title_lookup"`
		CitekeyLookup string `json:"citekey_lookup"`
	} `json:"meta"`
}

func decodeFindEnvelope(t *testing.T, out string) findEnvelope {
	t.Helper()
	var got findEnvelope
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return got
}

func TestItemsFindReportsNearTitlesWhenExactMisses(t *testing.T) {
	seedNearTitleStore(t)
	stdout, _ := runItemsFind(t, &rootFlags{asJSON: true}, "--title", "Attention Is All You Nead")
	got := decodeFindEnvelope(t, stdout)

	if len(got.Results) != 0 {
		t.Fatalf("results = %+v, want empty: a mistyped title is not an exact match", got.Results)
	}
	if len(got.Near) == 0 {
		t.Fatalf("near_title_matches empty; the typo case is the reason this exists")
	}
	if got.Near[0].Key != "ATTN" {
		t.Fatalf("best near match = %s, want ATTN", got.Near[0].Key)
	}
	if got.Near[0].Year != "2017" {
		t.Fatalf("near match year = %q, want 2017", got.Near[0].Year)
	}
	if got.Near[0].Score <= 0 || got.Near[0].Score > 1 {
		t.Fatalf("score = %v, want a reported value in (0,1]", got.Near[0].Score)
	}
	for _, match := range got.Near {
		if match.Key == "FRST" {
			t.Fatalf("unrelated item reported as a near title: %+v", got.Near)
		}
	}
}

// End to end over a seeded store: no key may appear twice in
// near_title_matches. The duplicate is injected the way a real mirror carries
// it — a second FTS document for one item, under a rowid no write path
// addresses — and it is injected AFTER the store has migrated, so this asserts
// the read path alone, not the repair migration.
func TestItemsFindReportsEachNearTitleKeyOnce(t *testing.T) {
	dbPath := seedNearTitleStore(t)
	db, err := store.OpenWithContext(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	// FNV-1a over the resource-qualified key: the scheme a superseded binary
	// used, which is why these rows exist in the field. Any rowid the current
	// writer does not compute would do.
	legacy := fnv.New64a()
	_, _ = legacy.Write([]byte("items\x00ATTN"))
	if _, err := db.DB().ExecContext(t.Context(),
		`INSERT INTO resources_fts (rowid, id, resource_type, content) VALUES (?, 'ATTN', 'items', ?)`,
		int64(legacy.Sum64()&0x7FFFFFFFFFFFFFFF), "Attention Is All You Need journalArticle ATTN",
	); err != nil {
		t.Fatalf("inject the second index document: %v", err)
	}
	var documents int
	if err := db.DB().QueryRowContext(t.Context(),
		`SELECT count(*) FROM resources_fts WHERE id = 'ATTN' AND resource_type = 'items'`,
	).Scan(&documents); err != nil {
		t.Fatalf("count ATTN documents: %v", err)
	}
	if documents != 2 {
		t.Fatalf("ATTN index documents = %d, want 2: the duplication this defends against is not present", documents)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	stdout, _ := runItemsFind(t, &rootFlags{asJSON: true}, "--title", "Attention Is All You Nead")
	got := decodeFindEnvelope(t, stdout)
	if len(got.Near) == 0 {
		t.Fatalf("near_title_matches empty; the typo case is the reason this exists")
	}
	seen := map[string]int{}
	for _, match := range got.Near {
		seen[match.Key]++
	}
	for key, count := range seen {
		if count > 1 {
			t.Fatalf("key %s appears %d times in near_title_matches: %+v", key, count, got.Near)
		}
	}
	if seen["ATTN"] != 1 {
		t.Fatalf("ATTN appears %d times, want once: %+v", seen["ATTN"], got.Near)
	}
}

// The identity contract: an exact hit must not be diluted by advisory rows, and
// callers such as `import resolve` must keep seeing exact equality in .results.
func TestItemsFindOmitsNearTitlesWhenExactMatches(t *testing.T) {
	seedNearTitleStore(t)
	stdout, _ := runItemsFind(t, &rootFlags{asJSON: true}, "--title", "attention is all you need")
	got := decodeFindEnvelope(t, stdout)

	if len(got.Results) != 1 || got.Results[0].Key != "ATTN" {
		t.Fatalf("results = %+v, want exactly ATTN", got.Results)
	}
	if got.Near != nil {
		t.Fatalf("near_title_matches = %+v, want absent when the title matched", got.Near)
	}
}

// A lookup by identifier must never grow a title block; the near-match path is
// keyed on --title alone.
//
// Asserting only that near_title_matches is absent passes for the wrong
// reason: with an empty title, titleCandidateMatchQuery returns "" and
// store.TitleCandidates short-circuits before touching the index, so the rows
// are empty whether or not the gate exists. meta.title_lookup is what actually
// defends it — reaching the near lookup at all would set no_near_matches.
func TestItemsFindOmitsNearTitlesWithoutTitleQuery(t *testing.T) {
	seedNearTitleStore(t)
	stdout, _ := runItemsFind(t, &rootFlags{asJSON: true}, "--doi", "10.9999/absent")
	got := decodeFindEnvelope(t, stdout)

	if len(got.Results) != 0 {
		t.Fatalf("results = %+v, want empty", got.Results)
	}
	if got.Near != nil {
		t.Fatalf("near_title_matches = %+v, want absent for an identifier lookup", got.Near)
	}
	if got.Meta.TitleLookup != "" {
		t.Fatalf("meta.title_lookup = %q, want empty: the near lookup must not run without --title", got.Meta.TitleLookup)
	}
}

// --plain and --csv route through printOutputWithFlags, so the prose block must
// not appear. A test writer cannot reach the terminal branch (a bytes.Buffer is
// never a char device), so the mode gate itself is asserted separately below.
func TestItemsFindNearTitlesStayOutOfMachineFormats(t *testing.T) {
	for _, tt := range []struct {
		name  string
		flags *rootFlags
	}{
		{name: "plain", flags: &rootFlags{plain: true}},
		{name: "csv", flags: &rootFlags{csv: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedNearTitleStore(t)
			stdout, _ := runItemsFind(t, tt.flags, "--title", "Attention Is All You Nead")
			if strings.Contains(stdout, "near titles") || strings.Contains(stdout, "ATTN") {
				t.Fatalf("%s stdout carries the prose block, which corrupts what the caller parses:\n%s", tt.name, stdout)
			}
		})
	}
}

func TestWantsNearMatchProseRefusesMachineAndQuietModes(t *testing.T) {
	cases := map[string]struct {
		flags *rootFlags
		want  bool
	}{
		"default": {flags: &rootFlags{}, want: true},
		"plain":   {flags: &rootFlags{plain: true}, want: false},
		"csv":     {flags: &rootFlags{csv: true}, want: false},
		"quiet":   {flags: &rootFlags{quiet: true}, want: false},
	}
	for name, tc := range cases {
		if got := wantsNearMatchProse(tc.flags); got != tc.want {
			t.Fatalf("wantsNearMatchProse(%s) = %v, want %v", name, got, tc.want)
		}
	}
}

// --quiet still receives the rows in the JSON envelope WHEN STDOUT IS NOT A
// TERMINAL, which is the case here and the case for every scripted caller:
// the flag trims prose, it does not withhold an answer a caller asked for. On
// a real terminal --quiet builds no envelope, and the exit code is the answer.
func TestItemsFindQuietKeepsNearTitlesInTheEnvelope(t *testing.T) {
	seedNearTitleStore(t)
	stdout, _ := runItemsFind(t, &rootFlags{quiet: true}, "--title", "Attention Is All You Nead")
	got := decodeFindEnvelope(t, stdout)
	if len(got.Near) == 0 || got.Near[0].Key != "ATTN" {
		t.Fatalf("near_title_matches = %+v, want ATTN present under --quiet", got.Near)
	}
}

// The block a human reads. Leading with the score put the least actionable
// column first, and a bare float said nothing about which of two identical
// rows to pick; the reader's next act is passing a key to another command, so
// the key leads and the command is named outright.
func TestPrintNearTitleMatchesLeadsWithTheActionableColumns(t *testing.T) {
	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	matches := []nearTitleMatch{
		{Key: "ATTN", Title: "Attention Is All You Need", Score: 0.95, Year: "2017", ItemType: "journalArticle"},
		{Key: "PREP", Title: "Attention Is All You Need", Score: 0.95, Year: "2017", ItemType: "preprint", Trashed: true},
		{Key: "SUBT", Title: "Attention Is All You Need: A Transformer Study", Score: 0.67},
	}
	if err := printFindLookupMiss(cmd, findItemsQuery{Title: "Attention Is All You Nead"},
		nearTitleBlock{matches: matches, total: 8, lookup: titleLookupNear}, nearCiteKeyBlock{}); err != nil {
		t.Fatalf("printFindLookupMiss: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{"matched: none", "near titles", "KEY", "YEAR", "TYPE", "TITLE", "SCORE", "ATTN", "2017", "0.95", "0.67"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "confirm before using") {
		t.Fatalf("block does not warn that these are different titles:\n%s", out)
	}
	if !strings.Contains(out, nearMatchMissingField) {
		t.Fatalf("a dateless, typeless item must still render both columns:\n%s", out)
	}
	// PREP and ATTN differ in nothing a reader can see except type and trash
	// state, which is exactly when those two markers decide the answer.
	if !strings.Contains(out, "preprint") || !strings.Contains(out, "journalArticle") {
		t.Fatalf("item type absent, so two same-title same-year rows are indistinguishable:\n%s", out)
	}
	if !strings.Contains(out, "(trashed)") {
		t.Fatalf("a trashed near match is unmarked while the exact path prints a DELETED column:\n%s", out)
	}
	if !strings.Contains(out, "(3 of 8 shown)") {
		t.Fatalf("truncation unreported, so the reader cannot know row 4 exists:\n%s", out)
	}
	keyColumn := strings.Index(out, "ATTN")
	scoreColumn := strings.Index(out, "0.95")
	if keyColumn < 0 || scoreColumn < 0 || keyColumn > scoreColumn {
		t.Fatalf("score leads the row; the key the reader acts on must come first:\n%s", out)
	}
	if !strings.Contains(stderr.String(), "No item has the title") {
		t.Fatalf("stderr does not state the exact answer:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "zotio items get ATTN") {
		t.Fatalf("stderr does not name the next command concretely:\n%s", stderr.String())
	}
}

// The genuinely-absent answer. Before this the zero-near path fell through to
// the generic writer and printed a bare `[]`, which reads as a broken command.
func TestPrintTitleLookupMissAnswersAnAbsentTitle(t *testing.T) {
	for _, tt := range []struct {
		name    string
		lookup  string
		wantErr string
	}{
		{name: "nothing close", lookup: titleLookupNoNear, wantErr: "no title in your library is close to it"},
		{name: "lookup broke", lookup: titleLookupFailed, wantErr: "near-title lookup failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			if err := printFindLookupMiss(cmd, findItemsQuery{Title: "Quantum Gravity in Eleven Dimensions"},
				nearTitleBlock{lookup: tt.lookup}, nearCiteKeyBlock{}); err != nil {
				t.Fatalf("printFindLookupMiss: %v", err)
			}
			if !strings.Contains(stdout.String(), "matched: none") {
				t.Fatalf("an absent title still prints no answer on stdout:\n%q", stdout.String())
			}
			if strings.Contains(stdout.String(), "[]") {
				t.Fatalf("stdout still carries the bare empty array:\n%q", stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want it to name %q", stderr.String(), tt.wantErr)
			}
		})
	}
	// The two states must not share a sentence: reporting "nothing is close"
	// when the search never ran is a confident negative the command did not
	// earn.
	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&stderr)
	if err := printFindLookupMiss(cmd, findItemsQuery{Title: "Anything"},
		nearTitleBlock{lookup: titleLookupFailed}, nearCiteKeyBlock{}); err != nil {
		t.Fatalf("printFindLookupMiss: %v", err)
	}
	if strings.Contains(stderr.String(), "close to it") {
		t.Fatalf("a failed lookup claims nothing is close:\n%s", stderr.String())
	}
}

func TestTitleCandidatesSurviveAMistypedWord(t *testing.T) {
	db, err := store.OpenWithContext(t.Context(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"ATTN","data":{"key":"ATTN","itemType":"journalArticle","title":"Attention Is All You Need"}}`),
		json.RawMessage(`{"key":"CHLD","data":{"key":"CHLD","itemType":"attachment","parentItem":"ATTN","title":"Attention Is All You Need PDF"}}`),
	}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	// One word is wrong. Under FTS5's implicit AND this returns nothing, which
	// is exactly the lookup the OR expression exists to keep alive.
	rows, err := db.TitleCandidates(t.Context(), "Attention Is All You Nead", titleCandidateLimit)
	if err != nil {
		t.Fatalf("TitleCandidates: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("candidates = %d, want 1 top-level item (child attachments excluded)", len(rows))
	}
	if !strings.Contains(string(rows[0]), `"ATTN"`) {
		t.Fatalf("candidate = %s, want ATTN", rows[0])
	}
}

// A title copied out of a reference list carries a trailing full stop, and a
// title copied out of a PDF carries whatever whitespace the layout produced.
// Both used to miss: the exact test folded only case and surrounding
// whitespace, so the near block then reported the SAME paper under a heading
// that says "different titles". These belong in .results.
func TestItemsFindMatchesATitleTypedFromAReferenceList(t *testing.T) {
	for _, tt := range []struct {
		name  string
		title string
	}{
		{name: "trailing full stop", title: "Attention is all you need."},
		{name: "internal whitespace run", title: "Attention  Is All   You Need"},
		{name: "both", title: "  Attention  is all you need.  "},
		{name: "newline for a space", title: "Attention Is All You\nNeed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedNearTitleStore(t)
			stdout, _ := runItemsFind(t, &rootFlags{asJSON: true}, "--title", tt.title)
			got := decodeFindEnvelope(t, stdout)
			if len(got.Results) != 1 || got.Results[0].Key != "ATTN" {
				t.Fatalf("results = %+v, want exactly ATTN for %q", got.Results, tt.title)
			}
			if got.Meta.TitleLookup != titleLookupExactHit {
				t.Fatalf("meta.title_lookup = %q, want %q", got.Meta.TitleLookup, titleLookupExactHit)
			}
			if got.Near != nil {
				t.Fatalf("near_title_matches = %+v, want absent: this is an exact hit", got.Near)
			}
		})
	}
}

// Four outcomes used to be one absent key, and an agent could not tell "the
// paper is not here" from "the near lookup broke".
func TestItemsFindReportsTitleLookupState(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "exact hit", args: []string{"--title", "attention is all you need"}, want: titleLookupExactHit},
		{name: "near matches", args: []string{"--title", "Attention Is All You Nead"}, want: titleLookupNear},
		{name: "nothing close", args: []string{"--title", "Quantum Gravity in Eleven Dimensions"}, want: titleLookupNoNear},
		{name: "not a title lookup", args: []string{"--doi", "10.9999/absent"}, want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedNearTitleStore(t)
			stdout, _ := runItemsFind(t, &rootFlags{asJSON: true}, tt.args...)
			if got := decodeFindEnvelope(t, stdout).Meta.TitleLookup; got != tt.want {
				t.Fatalf("meta.title_lookup = %q, want %q", got, tt.want)
			}
		})
	}
}

// The state that used to be indistinguishable from "nothing is close": the
// near lookup itself failing. Deleting the fts5 structure record corrupts the
// index while leaving every column the readiness probe requires, which is what
// a damaged store looks like from here — the exact lookup still answers, and
// only the MATCH query fails, with "fts5: corruption found reading blob 10".
func TestItemsFindReportsAFailedNearLookup(t *testing.T) {
	dbPath := seedNearTitleStore(t)
	db, err := store.OpenWithContext(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if _, err := db.DB().Exec(`DELETE FROM resources_fts_data WHERE id = 10`); err != nil {
		t.Fatalf("corrupt fts index: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	stdout, stderr := runItemsFind(t, &rootFlags{asJSON: true}, "--title", "Attention Is All You Nead")
	got := decodeFindEnvelope(t, stdout)
	if got.Meta.TitleLookup != titleLookupFailed {
		t.Fatalf("meta.title_lookup = %q, want %q: a broken lookup must not read as a confident negative",
			got.Meta.TitleLookup, titleLookupFailed)
	}
	if got.Near != nil {
		t.Fatalf("near_title_matches = %+v, want absent when the lookup failed", got.Near)
	}
	if !strings.Contains(stderr, "warning: near-title lookup unavailable") {
		t.Fatalf("stderr = %q, want the failure named", stderr)
	}
}

// --plain and --csv build no envelope (wantsJSONEnvelope refuses them), so
// before this the near rows reached neither stream: empty stdout, empty
// stderr, exit 0. The code comment claimed the envelope carried them.
func TestItemsFindNotesNearTitlesItCannotShow(t *testing.T) {
	for _, tt := range []struct {
		name  string
		flags *rootFlags
	}{
		{name: "plain", flags: &rootFlags{plain: true}},
		{name: "csv", flags: &rootFlags{csv: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedNearTitleStore(t)
			stdout, stderr := runItemsFind(t, tt.flags, "--title", "Attention Is All You Nead")
			if strings.Contains(stdout, "near titles") || strings.Contains(stdout, "ATTN") {
				t.Fatalf("%s stdout carries the prose block, which corrupts what the caller parses:\n%s", tt.name, stdout)
			}
			if !strings.Contains(stderr, "near titles found") {
				t.Fatalf("%s drops the rows silently; stderr = %q", tt.name, stderr)
			}
			if !strings.Contains(stderr, "--json") {
				t.Fatalf("%s note does not say how to see them; stderr = %q", tt.name, stderr)
			}
		})
	}
}

// --quiet is exempt: there the exit code is the whole answer, and the envelope
// carries the rows whenever stdout is not a terminal.
func TestItemsFindQuietPrintsNoNearTitleNote(t *testing.T) {
	seedNearTitleStore(t)
	_, stderr := runItemsFind(t, &rootFlags{quiet: true}, "--title", "Attention Is All You Nead")
	if strings.Contains(stderr, "near titles found") {
		t.Fatalf("--quiet asked for silence but got a note: %q", stderr)
	}
}

// year was omitempty, so a human saw the `----` placeholder while an agent got
// no key at all for the same row. Both halves now report the same gap.
func TestItemsFindNearTitleRowsAlwaysCarryYear(t *testing.T) {
	seedNearTitleStore(t)
	stdout, _ := runItemsFind(t, &rootFlags{asJSON: true}, "--title", "Attention Is All You Nead")
	if !strings.Contains(stdout, `"year": ""`) {
		t.Fatalf("SUBT has date \"n.d.\", so its year key must be present and empty:\n%s", stdout)
	}
}

// The near list is rank-capped at five. Without the count an agent reads "not
// in the five" as "not in the library" — the same human/agent asymmetry the
// year field had, and the prose block already prints "(5 of 8 shown)".
func TestItemsFindReportsTheNearTitleTotalWhenTruncated(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	db, err := store.OpenWithContext(t.Context(), filepath.Join(dataHome, "zotio", "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	rows := make([]json.RawMessage, 0, nearTitleMatchLimit+2)
	for i := range nearTitleMatchLimit + 2 {
		rows = append(rows, json.RawMessage(fmt.Sprintf(
			`{"key":"ATTN%04d","data":{"key":"ATTN%04d","itemType":"journalArticle","title":"Attention Is All You Need on Corpus %d","date":"20%02d"}}`,
			i, i, i, i+10)))
	}
	if _, _, err := db.UpsertBatch("items", rows); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	stdout, _ := runItemsFind(t, &rootFlags{asJSON: true}, "--title", "Attention Is All You Nead")
	var got struct {
		Near []nearTitleMatch `json:"near_title_matches"`
		Meta struct {
			Total int `json:"near_title_total"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if len(got.Near) != nearTitleMatchLimit {
		t.Fatalf("near rows = %d, want the rank cap %d", len(got.Near), nearTitleMatchLimit)
	}
	if got.Meta.Total != len(rows) {
		t.Fatalf("meta.near_title_total = %d, want %d: the reader cannot tell the list was capped", got.Meta.Total, len(rows))
	}
}

// The selectors are OR-ed. A --doi hit beside a --title miss returns rows, so
// calling that an exact title hit would report a title match that never
// happened — and the near lookup does not run once anything matched, so the
// title question is genuinely unanswered and the key must stay absent.
func TestItemsFindDoesNotClaimATitleHitFromAnotherSelector(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	db, err := store.OpenWithContext(t.Context(), filepath.Join(dataHome, "zotio", "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"ATTN","data":{"key":"ATTN","itemType":"journalArticle","title":"Attention Is All You Need","DOI":"10.48550/arXiv.1706.03762"}}`),
	}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	stdout, _ := runItemsFind(t, &rootFlags{asJSON: true},
		"--doi", "10.48550/arXiv.1706.03762", "--title", "Some Entirely Different Paper")
	got := decodeFindEnvelope(t, stdout)
	if len(got.Results) != 1 || got.Results[0].Key != "ATTN" {
		t.Fatalf("results = %+v, want ATTN matched by DOI", got.Results)
	}
	if got.Meta.TitleLookup != "" {
		t.Fatalf("meta.title_lookup = %q, want empty: the title never matched and no near lookup ran", got.Meta.TitleLookup)
	}

	// The same command when the title IS the thing that matched.
	stdout, _ = runItemsFind(t, &rootFlags{asJSON: true},
		"--doi", "10.9999/absent", "--title", "attention is all you need.")
	if got := decodeFindEnvelope(t, stdout).Meta.TitleLookup; got != titleLookupExactHit {
		t.Fatalf("meta.title_lookup = %q, want %q", got, titleLookupExactHit)
	}
}

// Every selector is a flag, so a positional argument is a mistake — most often
// a shell that ate the quotes around a title. It used to be ignored in silence.
func TestItemsFindRejectsPositionalArguments(t *testing.T) {
	seedNearTitleStore(t)
	cmd := newItemsFindCmd(&rootFlags{asJSON: true})
	cmd.SetArgs([]string{"--title", "Random Forests", "extra", "junk", "args"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute with positional arguments = nil, want a refusal naming them")
	}
	if !strings.Contains(err.Error(), "extra") {
		t.Fatalf("error = %v, want it to name the unexpected arguments", err)
	}
}

// A title with no term of two or more characters produces an empty MATCH
// expression, and TitleCandidates returns before it queries the index rather
// than building `MATCH ”`, which fts5 rejects.
func TestTitleCandidatesRejectATitleWithNoUsableTerm(t *testing.T) {
	db, err := store.OpenWithContext(t.Context(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	rows, err := db.TitleCandidates(t.Context(), " -- ,, ", titleCandidateLimit)
	if err != nil {
		t.Fatalf("TitleCandidates punctuation-only: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("candidates = %d, want none for a title with no usable term", len(rows))
	}
}

// seedNearCiteKeyStore builds a store whose citekeys exercise the outcomes the
// near-key block has to separate: the key itself, a key that is one typo away
// from it, and a key that merely has the same shape (an author and a year).
func seedNearCiteKeyStore(t *testing.T) {
	t.Helper()
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	db, err := store.OpenWithContext(t.Context(), filepath.Join(dataHome, "zotio", "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"ATTN","data":{"key":"ATTN","itemType":"journalArticle","title":"Attention Is All You Need","date":"2023","citationKey":"smith2023"}}`),
		json.RawMessage(`{"key":"JONE","data":{"key":"JONE","itemType":"journalArticle","title":"Something Else Entirely","date":"2023","extra":"Citation Key: jones2023"}}`),
	}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

// The citekey half of the same failure the title half fixed: a key is copied
// out of a manuscript by hand, so it carries typos at the same rate, and zero
// results could not tell an absent item from a mistyped key.
func TestItemsFindReportsNearCiteKeysWhenExactMisses(t *testing.T) {
	seedNearCiteKeyStore(t)
	stdout, _ := runItemsFind(t, &rootFlags{asJSON: true}, "--citekey", "smith2032")
	got := decodeFindEnvelope(t, stdout)

	if len(got.Results) != 0 {
		t.Fatalf("results = %+v, want empty: a mistyped citekey is not an exact match", got.Results)
	}
	if len(got.NearKeys) == 0 {
		t.Fatalf("near_citekey_matches empty; the typo case is the reason this exists")
	}
	if got.NearKeys[0].CiteKey != "smith2023" || got.NearKeys[0].ItemKey != "ATTN" {
		t.Fatalf("best near key = %+v, want smith2023 on item ATTN", got.NearKeys[0])
	}
	if got.NearKeys[0].Title != "Attention Is All You Need" {
		t.Fatalf("near key row = %+v, want the title a reader confirms the row with", got.NearKeys[0])
	}
	if got.NearKeys[0].Score <= 0 || got.NearKeys[0].Score >= 1 {
		t.Fatalf("score = %v, want a reported value in (0,1): the heading says these keys differ", got.NearKeys[0].Score)
	}
	for _, match := range got.NearKeys {
		if match.CiteKey == "jones2023" {
			t.Fatalf("an unrelated key from the same year was reported: %+v", got.NearKeys)
		}
	}
	if got.Meta.CitekeyLookup != citekeyLookupNear {
		t.Fatalf("meta.citekey_lookup = %q, want %q", got.Meta.CitekeyLookup, citekeyLookupNear)
	}
}

// The identity contract: an exact hit must not be diluted by advisory rows,
// so the near lookup does not even run.
func TestItemsFindOmitsNearCiteKeysWhenExactMatches(t *testing.T) {
	seedNearCiteKeyStore(t)
	stdout, _ := runItemsFind(t, &rootFlags{asJSON: true}, "--citekey", "smith2023")
	got := decodeFindEnvelope(t, stdout)
	if len(got.Results) != 1 || got.Results[0].Key != "ATTN" {
		t.Fatalf("results = %+v, want exactly ATTN", got.Results)
	}
	if got.NearKeys != nil {
		t.Fatalf("near_citekey_matches = %+v, want absent when the key matched", got.NearKeys)
	}
	if got.Meta.CitekeyLookup != citekeyLookupExactHit {
		t.Fatalf("meta.citekey_lookup = %q, want %q", got.Meta.CitekeyLookup, citekeyLookupExactHit)
	}
}

// A citekey is matched as stored, because that is the string LaTeX and the
// .bib file have to agree on: a key that differs only in case is a genuine
// miss and stays out of .results. The near block is what makes that miss
// answerable, and it is the one case where a suggestion may score 1.00 — the
// library really does hold that key, spelled in another case.
func TestItemsFindReportsACaseOnlyCiteKeyMissAtFullScore(t *testing.T) {
	seedNearCiteKeyStore(t)
	stdout, _ := runItemsFind(t, &rootFlags{asJSON: true}, "--citekey", "Smith2023")
	got := decodeFindEnvelope(t, stdout)
	if len(got.Results) != 0 {
		t.Fatalf("results = %+v, want empty: the stored key is smith2023", got.Results)
	}
	if len(got.NearKeys) != 1 || got.NearKeys[0].CiteKey != "smith2023" {
		t.Fatalf("near_citekey_matches = %+v, want smith2023", got.NearKeys)
	}
	if got.NearKeys[0].Score != 1 {
		t.Fatalf("score = %v, want exactly 1: the same key in another case is the one suggestion that needs no judgement",
			got.NearKeys[0].Score)
	}
}

// Four outcomes must not collapse into one absent key, and the key must stay
// absent when no --citekey was given at all.
func TestItemsFindReportsCiteKeyLookupState(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "exact hit", args: []string{"--citekey", "smith2023"}, want: citekeyLookupExactHit},
		{name: "near matches", args: []string{"--citekey", "smith2032"}, want: citekeyLookupNear},
		{name: "nothing close", args: []string{"--citekey", "einstein1905"}, want: citekeyLookupNoNear},
		{name: "not a citekey lookup", args: []string{"--doi", "10.9999/absent"}, want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedNearCiteKeyStore(t)
			stdout, _ := runItemsFind(t, &rootFlags{asJSON: true}, tt.args...)
			if got := decodeFindEnvelope(t, stdout).Meta.CitekeyLookup; got != tt.want {
				t.Fatalf("meta.citekey_lookup = %q, want %q", got, tt.want)
			}
		})
	}
}

// The selectors are OR-ed. A --doi hit beside a --citekey miss returns rows,
// so calling that an exact citekey hit would report a match that never
// happened — and the near lookup does not run once anything matched, so the
// citekey question is genuinely unanswered and the key must stay absent.
func TestItemsFindDoesNotClaimACiteKeyHitFromAnotherSelector(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	db, err := store.OpenWithContext(t.Context(), filepath.Join(dataHome, "zotio", "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"ATTN","data":{"key":"ATTN","itemType":"journalArticle","title":"Attention Is All You Need","DOI":"10.48550/arXiv.1706.03762","citationKey":"smith2023"}}`),
	}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	stdout, _ := runItemsFind(t, &rootFlags{asJSON: true},
		"--doi", "10.48550/arXiv.1706.03762", "--citekey", "wildly1899different")
	got := decodeFindEnvelope(t, stdout)
	if len(got.Results) != 1 || got.Results[0].Key != "ATTN" {
		t.Fatalf("results = %+v, want ATTN matched by DOI", got.Results)
	}
	if got.Meta.CitekeyLookup != "" {
		t.Fatalf("meta.citekey_lookup = %q, want empty: the citekey never matched and no near lookup ran", got.Meta.CitekeyLookup)
	}
	if got.NearKeys != nil {
		t.Fatalf("near_citekey_matches = %+v, want absent: the lookup returned rows", got.NearKeys)
	}
}

// --plain and --csv build no envelope (wantsJSONEnvelope refuses them), so
// without the note the rows reach neither stream: empty stdout, empty stderr,
// exit 0. --quiet is exempt, because there the exit code is the whole answer.
func TestItemsFindNotesNearCiteKeysItCannotShow(t *testing.T) {
	for _, tt := range []struct {
		name  string
		flags *rootFlags
	}{
		{name: "plain", flags: &rootFlags{plain: true}},
		{name: "csv", flags: &rootFlags{csv: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedNearCiteKeyStore(t)
			stdout, stderr := runItemsFind(t, tt.flags, "--citekey", "smith2032")
			if strings.Contains(stdout, "near citekeys") || strings.Contains(stdout, "smith2023") {
				t.Fatalf("%s stdout carries the prose block, which corrupts what the caller parses:\n%s", tt.name, stdout)
			}
			if !strings.Contains(stderr, "near citekey found") {
				t.Fatalf("%s drops the rows silently; stderr = %q", tt.name, stderr)
			}
			if !strings.Contains(stderr, "--json") {
				t.Fatalf("%s note does not say how to see them; stderr = %q", tt.name, stderr)
			}
		})
	}

	t.Run("quiet", func(t *testing.T) {
		seedNearCiteKeyStore(t)
		stdout, stderr := runItemsFind(t, &rootFlags{quiet: true}, "--citekey", "smith2032")
		if strings.Contains(stderr, "near citekey found") {
			t.Fatalf("--quiet asked for silence but got a note: %q", stderr)
		}
		// The rows still reach a scripted caller: --quiet trims prose, it
		// does not withhold an answer that was asked for.
		got := decodeFindEnvelope(t, stdout)
		if len(got.NearKeys) == 0 || got.NearKeys[0].CiteKey != "smith2023" {
			t.Fatalf("near_citekey_matches = %+v, want smith2023 present under --quiet", got.NearKeys)
		}
	})
}

// The block a human reads. It leads with the key they will paste into the
// manuscript, keeps the score last because it is advisory, and says outright
// that these keys are different.
func TestPrintNearCiteKeyMatchesLeadsWithTheActionableColumns(t *testing.T) {
	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	matches := []nearCiteKeyMatch{
		{CiteKey: "smith2023", ItemKey: "ATTN", Title: "Attention Is All You Need", Score: 0.56},
		{CiteKey: "smith2023a", ItemKey: "SMITA", Score: 0.5},
	}
	if err := printFindLookupMiss(cmd, findItemsQuery{Citekey: "smith2032"},
		nearTitleBlock{}, nearCiteKeyBlock{matches: matches, total: 4, lookup: citekeyLookupNear}); err != nil {
		t.Fatalf("printFindLookupMiss: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{"matched: none", "near citekeys", "CITEKEY", "KEY", "TITLE", "SCORE", "smith2023", "ATTN", "0.56", "(2 of 4 shown)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "confirm before using") {
		t.Fatalf("block does not warn that these are different keys:\n%s", out)
	}
	if !strings.Contains(out, nearMatchMissingField) {
		t.Fatalf("a titleless item must still render the column:\n%s", out)
	}
	if keyColumn, scoreColumn := strings.Index(out, "smith2023"), strings.Index(out, "0.56"); keyColumn > scoreColumn {
		t.Fatalf("score leads the row; the key the reader acts on must come first:\n%s", out)
	}
	if !strings.Contains(stderr.String(), `No item has the citation key "smith2032"`) {
		t.Fatalf("stderr does not state the exact answer:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "zotio items get ATTN") {
		t.Fatalf("stderr does not name the next command concretely:\n%s", stderr.String())
	}
}

// The genuinely-absent answer, and the state that must stay distinct from it:
// reporting "nothing is close" when the search never ran is a confident
// negative the command did not earn.
func TestPrintCiteKeyLookupMissAnswersAnAbsentKey(t *testing.T) {
	for _, tt := range []struct {
		name    string
		lookup  string
		wantErr string
		notWant string
	}{
		{name: "nothing close", lookup: citekeyLookupNoNear, wantErr: "no citation key in your library is close to it"},
		{name: "lookup broke", lookup: citekeyLookupFailed, wantErr: "near-citekey lookup failed", notWant: "close to it"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			if err := printFindLookupMiss(cmd, findItemsQuery{Citekey: "einstein1905"},
				nearTitleBlock{}, nearCiteKeyBlock{lookup: tt.lookup}); err != nil {
				t.Fatalf("printFindLookupMiss: %v", err)
			}
			if !strings.Contains(stdout.String(), "matched: none") {
				t.Fatalf("an absent key still prints no answer on stdout:\n%q", stdout.String())
			}
			if strings.Contains(stdout.String(), "[]") {
				t.Fatalf("stdout still carries the bare empty array:\n%q", stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr = %q, want it to name %q", stderr.String(), tt.wantErr)
			}
			if tt.notWant != "" && strings.Contains(stderr.String(), tt.notWant) {
				t.Fatalf("stderr = %q, must not claim %q", stderr.String(), tt.notWant)
			}
		})
	}
}

// Both hand-typed selectors can miss in one run. Each is a separate answer:
// a close citekey does not make the title present, so both blocks are
// printed, "matched: none" is the answer to the command and is printed once,
// and both meta states are reported.
func TestItemsFindAnswersATitleAndACiteKeyMissTogether(t *testing.T) {
	cmd := &cobra.Command{}
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	if err := printFindLookupMiss(cmd, findItemsQuery{Title: "Attention Is All You Nead", Citekey: "smith2032"},
		nearTitleBlock{matches: []nearTitleMatch{{Key: "ATTN", Title: "Attention Is All You Need", Score: 0.95, Year: "2023"}}, total: 1, lookup: titleLookupNear},
		nearCiteKeyBlock{matches: []nearCiteKeyMatch{{CiteKey: "smith2023", ItemKey: "ATTN", Score: 0.56}}, total: 1, lookup: citekeyLookupNear}); err != nil {
		t.Fatalf("printFindLookupMiss: %v", err)
	}
	out := stdout.String()
	if strings.Count(out, "matched: none") != 1 {
		t.Fatalf("matched: none printed %d times, want exactly one answer to the command:\n%s",
			strings.Count(out, "matched: none"), out)
	}
	if !strings.Contains(out, "near titles") || !strings.Contains(out, "near citekeys") {
		t.Fatalf("both selectors missed; each needs its own block:\n%s", out)
	}

	seedNearCiteKeyStore(t)
	stdout2, _ := runItemsFind(t, &rootFlags{asJSON: true},
		"--title", "Attention Is All You Nead", "--citekey", "smith2032")
	got := decodeFindEnvelope(t, stdout2)
	if len(got.Results) != 0 {
		t.Fatalf("results = %+v, want empty", got.Results)
	}
	if len(got.Near) == 0 || len(got.NearKeys) == 0 {
		t.Fatalf("envelope carries only one advisory block: near titles %+v, near citekeys %+v", got.Near, got.NearKeys)
	}
	if got.Meta.TitleLookup != titleLookupNear || got.Meta.CitekeyLookup != citekeyLookupNear {
		t.Fatalf("meta = title %q citekey %q, want both %q", got.Meta.TitleLookup, got.Meta.CitekeyLookup, titleLookupNear)
	}
}

// The near-key list is rank-capped. Without the total an agent reads "not in
// the three" as "not in the library", the same asymmetry between the two
// halves of this output that the near-title total closed.
func TestItemsFindReportsTheNearCiteKeyTotalWhenTruncated(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	db, err := store.OpenWithContext(t.Context(), filepath.Join(dataHome, "zotio", "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	rows := make([]json.RawMessage, 0, nearCiteKeyMatchLimit+2)
	for i := range nearCiteKeyMatchLimit + 2 {
		rows = append(rows, json.RawMessage(fmt.Sprintf(
			`{"key":"SMIT%04d","data":{"key":"SMIT%04d","itemType":"journalArticle","title":"Attention %d","citationKey":"smith2023%c"}}`,
			i, i, i, 'a'+i)))
	}
	if _, _, err := db.UpsertBatch("items", rows); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	stdout, _ := runItemsFind(t, &rootFlags{asJSON: true}, "--citekey", "smith2023")
	var got struct {
		NearKeys []nearCiteKeyMatch `json:"near_citekey_matches"`
		Meta     struct {
			Total int `json:"near_citekey_total"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if len(got.NearKeys) != nearCiteKeyMatchLimit {
		t.Fatalf("near key rows = %d, want the rank cap %d", len(got.NearKeys), nearCiteKeyMatchLimit)
	}
	if got.Meta.Total != len(rows) {
		t.Fatalf("meta.near_citekey_total = %d, want %d: the reader cannot tell the list was capped", got.Meta.Total, len(rows))
	}
}

// seedColonTightCiteKeyStore is a library whose pinned keys are written the
// way Zotero writes them — "Citation Key:key", no space — beside one written
// the way zotio's importer writes them. Both spellings occur in real
// libraries, and both halves of this command have to agree about them.
func seedColonTightCiteKeyStore(t *testing.T) {
	t.Helper()
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	db, err := store.OpenWithContext(t.Context(), filepath.Join(dataHome, "zotio", "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"TIGHT","data":{"key":"TIGHT","itemType":"journalArticle","title":"Tight Pinned Key","date":"2023","extra":"Citation Key:tight2023"}}`),
		json.RawMessage(`{"key":"SPACED","data":{"key":"SPACED","itemType":"journalArticle","title":"Spaced Pinned Key","date":"2023","extra":"Citation Key: spaced2023"}}`),
	}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

// The two halves of --citekey have to mean one thing by "citekey".
// findRowMatchesExact has accepted the colon-tight spelling since ac8ea71,
// but the near path read the inventory through resolveCiteKey, which only
// knew the spaced form: `--citekey tight2023` matched the item, while
// `--citekey tight2032` said nothing was close to it. The rescue path was
// blind to exactly the keys Zotero writes.
func TestItemsFindSuggestsCiteKeysInBothExtraSpellings(t *testing.T) {
	t.Run("colon-tight key is offered for a typo", func(t *testing.T) {
		seedColonTightCiteKeyStore(t)
		got := decodeFindEnvelope(t, mustFindStdout(t, "--citekey", "tight2032"))
		if len(got.NearKeys) != 1 || got.NearKeys[0].CiteKey != "tight2023" || got.NearKeys[0].ItemKey != "TIGHT" {
			t.Fatalf("near_citekey_matches = %+v, want tight2023 on item TIGHT", got.NearKeys)
		}
		if got.Meta.CitekeyLookup != citekeyLookupNear {
			t.Fatalf("meta.citekey_lookup = %q, want %q", got.Meta.CitekeyLookup, citekeyLookupNear)
		}
	})

	t.Run("spaced key is still offered", func(t *testing.T) {
		seedColonTightCiteKeyStore(t)
		got := decodeFindEnvelope(t, mustFindStdout(t, "--citekey", "spaced2032"))
		if len(got.NearKeys) != 1 || got.NearKeys[0].CiteKey != "spaced2023" || got.NearKeys[0].ItemKey != "SPACED" {
			t.Fatalf("near_citekey_matches = %+v, want spaced2023 on item SPACED", got.NearKeys)
		}
	})

	// The other side of the agreement: the exact path already matched this
	// key, and it still does, so the two paths now answer one question the
	// same way.
	t.Run("colon-tight key still matches exactly", func(t *testing.T) {
		seedColonTightCiteKeyStore(t)
		got := decodeFindEnvelope(t, mustFindStdout(t, "--citekey", "tight2023"))
		if len(got.Results) != 1 || got.Results[0].Key != "TIGHT" {
			t.Fatalf("results = %+v, want the colon-tight item matched exactly", got.Results)
		}
		if got.Meta.CitekeyLookup != citekeyLookupExactHit {
			t.Fatalf("meta.citekey_lookup = %q, want %q", got.Meta.CitekeyLookup, citekeyLookupExactHit)
		}
	})
}

func mustFindStdout(t *testing.T, args ...string) string {
	t.Helper()
	stdout, _ := runItemsFind(t, &rootFlags{asJSON: true}, args...)
	return stdout
}

// Two items holding one citekey is the library state `items citekey-conflicts`
// exists for, and it used to break this list twice: the duplicate rows tie
// completely, so which item and title were shown followed row order (the
// inventory query has no ORDER BY), and three of them filled the rank cap
// before any other key appeared. The person who mistyped a key needs the
// list of KEYS they might have meant, so a shared key is one row — the row
// naming the lowest item key, on every run — and the cap is spent on other
// keys.
func TestItemsFindListsADuplicateCiteKeyOnceAndKeepsOtherKeys(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	db, err := store.OpenWithContext(t.Context(), filepath.Join(dataHome, "zotio", "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Inserted so that row order and item-key order disagree: DUPC is read
	// first, and only the item tiebreak puts DUPA in the list.
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"DUPC","data":{"key":"DUPC","itemType":"journalArticle","title":"Third Copy","date":"2023","citationKey":"smith2023"}}`),
		json.RawMessage(`{"key":"DUPB","data":{"key":"DUPB","itemType":"journalArticle","title":"Second Copy","date":"2023","citationKey":"smith2023"}}`),
		json.RawMessage(`{"key":"DUPA","data":{"key":"DUPA","itemType":"journalArticle","title":"First Copy","date":"2023","citationKey":"smith2023"}}`),
		json.RawMessage(`{"key":"LATER","data":{"key":"LATER","itemType":"journalArticle","title":"A Later Year","date":"2024","citationKey":"smith2024"}}`),
	}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	first := decodeFindEnvelope(t, mustFindStdout(t, "--citekey", "smith2032")).NearKeys
	wantKeys := []string{"smith2023", "smith2024"}
	gotKeys := make([]string, 0, len(first))
	for _, row := range first {
		gotKeys = append(gotKeys, row.CiteKey)
	}
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("near citekeys = %#v, want %#v: one key held by three items must be one suggestion, and must not spend the cap of %d",
			gotKeys, wantKeys, nearCiteKeyMatchLimit)
	}
	if first[0].ItemKey != "DUPA" || first[0].Title != "First Copy" {
		t.Fatalf("shared-key row = %+v, want the lowest item key so the row does not follow row order", first[0])
	}

	// Repeated runs over one library print one list. The store cannot supply
	// that: citekeyAuditQuery has no ORDER BY and sort.Slice is not stable.
	for run := range 3 {
		again := decodeFindEnvelope(t, mustFindStdout(t, "--citekey", "smith2032")).NearKeys
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("run %d listed %+v, want the same list as the first run %+v", run+2, again, first)
		}
	}
}

// hostileLibraryText is text a Zotero library can really hold in a title, and
// that a pinned citation key can hold too because Extra is a free-text field:
// a tab, which is the delimiter every advisory block's tabwriter reads as a
// column break; a newline followed by text shaped like a heading of zotio's
// own; and an ANSI colour escape. Rendered untreated on a terminal it broke
// one candidate across two lines, started a line with a heading zotio never
// wrote, left the score under the wrong header, and recoloured the terminal
// from a data field.
const hostileLibraryText = "Attention Is All You\tNeed\n== FAKE HEADING ==\x1b[31m RED"

// assertNoTerminalInjection fails when library data reached the terminal as
// anything but inert text: a raw escape or carriage return, or a line that
// the library, not zotio, decided to start.
func assertNoTerminalInjection(t *testing.T, where, body string) {
	t.Helper()
	if i := strings.IndexAny(body, "\x1b\r"); i >= 0 {
		t.Fatalf("%s: raw control byte %q at offset %d reaches the terminal:\n%q", where, body[i], i, body)
	}
	for n, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "== FAKE HEADING ==") {
			t.Fatalf("%s: line %d is text the library injected, so it can forge zotio's own output:\n%q", where, n+1, body)
		}
	}
}

// assertAdvisoryRowShape fails unless the block prints exactly one row per
// candidate, each carrying its score under the SCORE header. Both halves
// matter: a newline inside a cell splits one candidate into two lines, and a
// tab inside a cell opens a column that shifts the score under the header
// beside it. Offsets are counted in runes, which is what tabwriter pads by.
func assertAdvisoryRowShape(t *testing.T, where, body string, scores []string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	header := -1
	for i, line := range lines {
		if strings.Contains(line, "SCORE") {
			header = i
			break
		}
	}
	if header < 0 {
		t.Fatalf("%s: no header row:\n%q", where, body)
	}
	wantCol := utf8.RuneCountInString(lines[header][:strings.Index(lines[header], "SCORE")])
	for i, score := range scores {
		row := header + 1 + i
		if row >= len(lines) {
			t.Fatalf("%s: candidate %d has no row; the block is %d lines:\n%q", where, i+1, len(lines), body)
		}
		at := strings.LastIndex(lines[row], score)
		if at < 0 {
			t.Fatalf("%s: row %d does not carry score %s, so the candidate spans more than one line:\n%q", where, i+1, score, body)
		}
		if got := utf8.RuneCountInString(lines[row][:at]); got != wantCol {
			t.Fatalf("%s: row %d prints score %s at column %d, want %d (under the SCORE header):\n%q", where, i+1, score, got, wantCol, body)
		}
	}
	if rest := lines[header+1+len(scores):]; len(rest) > 0 && strings.Contains(rest[0], "FAKE") {
		t.Fatalf("%s: an extra row follows the %d candidates:\n%q", where, len(scores), body)
	}
}

// A title is publisher- or user-supplied text, so the near-title block must
// render it as inert data. Every other cell here is library data too — the
// item key, the date the year is derived from, the item type — and each is
// driven with a control byte so none of them is left as the next hole.
func TestPrintNearTitleMatchesRendersHostileLibraryTextAsInertData(t *testing.T) {
	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	long := strings.Repeat("x", 200)
	matches := []nearTitleMatch{
		{Key: "A1", Title: "Attention Is All You Need", Score: 0.99, Year: "2017", ItemType: "journalArticle"},
		{Key: "EVIL", Title: hostileLibraryText, Score: 0.67, Year: "2018", ItemType: "journalArticle"},
		{Key: "K3\x1b[32m", Title: "Plain Title", Score: 0.51, Year: "2019\n", ItemType: "book\tfake"},
		{Key: "LONG", Title: long, Score: 0.42, Year: "2020", ItemType: "book", Trashed: true},
	}
	if err := printNearTitleMatches(cmd, "Attention Is All You Nead", matches, 4); err != nil {
		t.Fatalf("printNearTitleMatches: %v", err)
	}
	out, errOut := stdout.String(), stderr.String()
	assertNoTerminalInjection(t, "stdout", out)
	assertNoTerminalInjection(t, "stderr", errOut)
	assertAdvisoryRowShape(t, "near titles", out, []string{"0.99", "0.67", "0.51", "0.42"})
	if strings.Contains(out, long[:60]) {
		t.Fatalf("a 200-character title is printed whole, so one row destroys the column layout:\n%q", out)
	}
	if !strings.Contains(out, "(trashed)") {
		t.Fatalf("truncation dropped the trashed marker the exact path shows as a DELETED column:\n%q", out)
	}
	// The next-command line is pasted, so the key is sanitized and never
	// truncated: half a key is a command that fails.
	if !strings.Contains(errOut, "zotio items get A1\n") {
		t.Fatalf("stderr does not name the next command with the whole key:\n%q", errOut)
	}
}

// A pinned citation key comes from Extra, a free-text field, so it is exactly
// as untrusted as the title beside it.
func TestPrintNearCiteKeyMatchesRendersHostileLibraryTextAsInertData(t *testing.T) {
	cmd := &cobra.Command{}
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	matches := []nearCiteKeyMatch{
		{CiteKey: "smith2023", ItemKey: "ATTN", Title: "Attention Is All You Need", Score: 0.56},
		{CiteKey: "smith2023\t== FAKE HEADING ==\x1b[31m", ItemKey: "EVIL", Title: hostileLibraryText, Score: 0.5},
	}
	if err := printNearCiteKeyMatches(cmd, "smith2032", matches, 2); err != nil {
		t.Fatalf("printNearCiteKeyMatches: %v", err)
	}
	out, errOut := stdout.String(), stderr.String()
	assertNoTerminalInjection(t, "stdout", out)
	assertNoTerminalInjection(t, "stderr", errOut)
	assertAdvisoryRowShape(t, "near citekeys", out, []string{"0.56", "0.50"})
	if !strings.Contains(errOut, "zotio items get ATTN\n") {
		t.Fatalf("stderr does not name the next command with the whole key:\n%q", errOut)
	}
}

// The terminal treatment is display-only. --json must hand the stored title
// back byte for byte: a consumer diffing it against its own record needs the
// bytes, an escape sequence inside a JSON string is inert, and truncating or
// replacing anything there would corrupt data rather than protect a terminal.
func TestItemsFindKeepsHostileTitleByteIdenticalInJSON(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	db, err := store.OpenWithContext(t.Context(), filepath.Join(dataHome, "zotio", "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	item, err := json.Marshal(map[string]any{
		"key": "EVIL",
		"data": map[string]any{
			"key":      "EVIL",
			"itemType": "journalArticle",
			"title":    hostileLibraryText,
			"date":     "2018",
		},
	})
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{item}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	stdout, _ := runItemsFind(t, &rootFlags{asJSON: true}, "--title", "Attention Is All You Nead")
	got := decodeFindEnvelope(t, stdout)
	if len(got.Near) != 1 {
		t.Fatalf("near_title_matches = %+v, want the seeded item", got.Near)
	}
	if got.Near[0].Title != hostileLibraryText {
		t.Fatalf("near_title_matches[0].title = %q, want the stored bytes %q", got.Near[0].Title, hostileLibraryText)
	}
	if strings.ContainsRune(stdout, '\uFFFD') {
		t.Fatalf("JSON output carries a replacement rune, so a machine reader was handed a sanitized title:\n%s", stdout)
	}
}
