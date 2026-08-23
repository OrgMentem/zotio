// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import "testing"

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
	if !findRowMatchesExact(rows[0], "", "", "", "", "smith2023") {
		t.Fatal("row A (exact citekey) should match")
	}
	if findRowMatchesExact(rows[1], "", "", "", "", "smith2023") {
		t.Fatal("row B (smith2023a) must NOT match smith2023 — prefix over-match")
	}
}

func TestFindRowMatchesExact_PMIDPrefixOverMatch(t *testing.T) {
	rows := []map[string]any{
		{"id": "C", "data": `{"data":{"key":"C","itemType":"journalArticle","extra":"PMID: 123"}}`},
		{"id": "D", "data": `{"data":{"key":"D","itemType":"journalArticle","extra":"PMID: 12345"}}`},
	}
	if !findRowMatchesExact(rows[0], "", "", "", "123", "") {
		t.Fatal("PMID 123 exact row should match")
	}
	if findRowMatchesExact(rows[1], "", "", "", "123", "") {
		t.Fatal("PMID 12345 row must NOT match query 123 — prefix over-match")
	}
}

func TestFindRowMatchesExact_ArXivPrefixOverMatch(t *testing.T) {
	rows := []map[string]any{
		{"id": "E", "data": `{"data":{"key":"E","itemType":"journalArticle","extra":"arXiv: 2006.11"}}`},
		{"id": "F", "data": `{"data":{"key":"F","itemType":"journalArticle","extra":"arXiv: 2006.11239"}}`},
	}
	if !findRowMatchesExact(rows[0], "", "2006.11", "", "", "") {
		t.Fatal("arXiv 2006.11 exact row should match")
	}
	if findRowMatchesExact(rows[1], "", "2006.11", "", "", "") {
		t.Fatal("arXiv 2006.11239 must NOT match query 2006.11 — prefix over-match")
	}
	// Reverse: searching the longer id should match only F.
	if findRowMatchesExact(rows[0], "", "2006.11239", "", "", "") {
		t.Fatal("arXiv 2006.11 must NOT match query 2006.11239")
	}
	if !findRowMatchesExact(rows[1], "", "2006.11239", "", "", "") {
		t.Fatal("arXiv 2006.11239 exact row should match")
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
			got := findRowMatchesExact(tc.row, "", tc.arxiv, "", tc.pmid, tc.citekey)
			if got != tc.want {
				t.Fatalf("findRowMatchesExact(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestFilterFindRowsExact_Citekey(t *testing.T) {
	rows := []map[string]any{
		{"id": "A", "data": `{"data":{"key":"A","itemType":"journalArticle","extra":"Citation Key: smith2023"}}`},
		{"id": "B", "data": `{"data":{"key":"B","itemType":"journalArticle","extra":"Citation Key: smith2023a"}}`},
	}
	got := filterFindRowsExact(rows, "", "", "", "", "smith2023")
	if len(got) != 1 || got[0]["id"] != "A" {
		t.Fatalf("filter smith2023 = %v, want [A]", got)
	}
}

func TestFilterFindRowsExact_LegitimateMatchStillSucceeds(t *testing.T) {
	// Regression: exact search with a single item must still return it.
	rows := []map[string]any{
		{"id": "A", "data": `{"data":{"key":"A","itemType":"journalArticle","extra":"Citation Key: smith2023"}}`},
	}
	got := filterFindRowsExact(rows, "", "", "", "", "smith2023")
	if len(got) != 1 {
		t.Fatalf("exact citekey filter with single exact item = %d rows, want 1", len(got))
	}
	rows2 := []map[string]any{
		{"id": "C", "data": `{"data":{"key":"C","itemType":"journalArticle","extra":"PMID: 123"}}`},
	}
	got2 := filterFindRowsExact(rows2, "", "", "", "123", "")
	if len(got2) != 1 {
		t.Fatalf("exact PMID filter = %d rows, want 1", len(got2))
	}
	rows3 := []map[string]any{
		{"id": "E", "data": `{"data":{"key":"E","itemType":"journalArticle","extra":"arXiv: 2006.11"}}`},
	}
	got3 := filterFindRowsExact(rows3, "", "2006.11", "", "", "")
	if len(got3) != 1 {
		t.Fatalf("exact arXiv filter = %d rows, want 1", len(got3))
	}
}

func TestFilterFindRowsExact_CitationKeyFieldExact(t *testing.T) {
	rows := []map[string]any{
		{"id": "A", "data": `{"data":{"key":"A","itemType":"journalArticle","citationKey":"smith2023"}}`},
		{"id": "B", "data": `{"data":{"key":"B","itemType":"journalArticle","citationKey":"smith2023a"}}`},
	}
	got := filterFindRowsExact(rows, "", "", "", "", "smith2023")
	if len(got) != 1 || got[0]["id"] != "A" {
		t.Fatalf("citationKey field filter = %v, want [A]", got)
	}
}

func TestFilterFindRowsExact_ArchiveIDExact(t *testing.T) {
	rows := []map[string]any{
		{"id": "X", "data": `{"data":{"key":"X","itemType":"journalArticle","archiveID":"arXiv:2006.11"}}`},
		{"id": "Y", "data": `{"data":{"key":"Y","itemType":"journalArticle","archiveID":"arXiv:2006.11239"}}`},
	}
	got := filterFindRowsExact(rows, "", "2006.11", "", "", "")
	if len(got) != 1 || got[0]["id"] != "X" {
		t.Fatalf("archiveID filter 2006.11 = %v, want [X]", got)
	}
}

func TestFilterFindRowsExact_RequiresBoundaryNotPrefix(t *testing.T) {
	t.Run("citekey", func(t *testing.T) {
		rows := []map[string]any{
			{"id": "A", "data": `{"data":{"key":"A","itemType":"journalArticle","extra":"Citation Key: smith2023"}}`},
			{"id": "B", "data": `{"data":{"key":"B","itemType":"journalArticle","extra":"Citation Key: smith2023a"}}`},
		}
		// Candidate set as returned by the LIKE pre-filter would contain both;
		// post-filter must reduce to exactly the queried one.
		filtered := filterFindRowsExact(rows, "", "", "", "", "smith2023")
		if len(filtered) != 1 || filtered[0]["id"] != "A" {
			t.Fatalf("citekey smith2023 filtered = %v, want single A", filtered)
		}
		filtered2 := filterFindRowsExact(rows, "", "", "", "", "smith2023a")
		if len(filtered2) != 1 || filtered2[0]["id"] != "B" {
			t.Fatalf("citekey smith2023a filtered = %v, want single B", filtered2)
		}
	})
	t.Run("pmid", func(t *testing.T) {
		rows := []map[string]any{
			{"id": "C", "data": `{"data":{"key":"C","itemType":"journalArticle","extra":"PMID: 123"}}`},
			{"id": "D", "data": `{"data":{"key":"D","itemType":"journalArticle","extra":"PMID: 12345"}}`},
		}
		filtered := filterFindRowsExact(rows, "", "", "", "123", "")
		if len(filtered) != 1 || filtered[0]["id"] != "C" {
			t.Fatalf("pmid 123 filtered = %v, want single C", filtered)
		}
		filtered2 := filterFindRowsExact(rows, "", "", "", "12345", "")
		if len(filtered2) != 1 || filtered2[0]["id"] != "D" {
			t.Fatalf("pmid 12345 filtered = %v, want single D", filtered2)
		}
	})
}
