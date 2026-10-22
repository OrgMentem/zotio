// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"reflect"
	"testing"
)

func TestParseBibTeXItemsQuoteInsideBraces(t *testing.T) {
	for _, tc := range []struct {
		name       string
		bib        string
		wantTitles []string
	}{
		{"lone quote in braced title", `@article{k, title = {The 12" Single}, year = {1999}}`, []string{`The 12" Single`}},
		{"one quote per entry stays two entries",
			"@article{a, title = {Alpha 5\" disk}, year = {1}}\n@article{b, title = {Beta 3\" disk}, year = {2}}",
			[]string{`Alpha 5" disk`, `Beta 3" disk`}},
		{"quoted string value still delimits", `@article{a, title = "Say {\"hi\"}, now", year = {1}}`, []string{`Say {"hi"}, now`}},
		{"brace inside quoted value", `@article{a, title = "A {B}, C", year = {1}}`, []string{`A {B}, C`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, err := parseBibTeXItems(tc.bib, "")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var got []string
			for _, it := range items {
				got = append(got, it["title"].(string))
			}
			if !reflect.DeepEqual(got, tc.wantTitles) {
				t.Fatalf("titles = %q, want %q", got, tc.wantTitles)
			}
			for _, it := range items {
				if it["date"] == nil {
					t.Fatalf("year field lost after a quote: %v", it)
				}
			}
		})
	}
}

func TestBibTeXMetaBlocksAreNotRecords(t *testing.T) {
	for _, tc := range []struct {
		name  string
		bib   string
		count int
	}{
		{"string and comment", "@string{jn = {Nature}}\n@article{a, title={One}}\n@comment{jabref-meta: databaseType:bibtex;}", 1},
		{"preamble, mixed case", "@PREAMBLE{\"\\newcommand{x}{y}\"}\n@Comment{x}\n@book{b, title={Two}}", 1},
		{"@word inside a field value", "@article{a, title={One}, annote={see @article{x, y} too}}", 1},
		{"only meta blocks", "@string{a = {b}}\n@comment{c}", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, err := parseBibTeXItems(tc.bib, "")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(items) != tc.count {
				t.Fatalf("items = %d, want %d: %v", len(items), tc.count, items)
			}
			if got := countImportFileRecords(tc.bib, "bibtex"); got != tc.count {
				t.Fatalf("countImportFileRecords = %d, want %d", got, tc.count)
			}
		})
	}
}

func TestBibTeXAuthorBraceProtection(t *testing.T) {
	person := func(last, first string) map[string]any {
		return map[string]any{"creatorType": "author", "lastName": last, "firstName": first}
	}
	org := func(name string) map[string]any { return map[string]any{"creatorType": "author", "name": name} }
	for _, tc := range []struct {
		name   string
		author string
		want   []map[string]any
	}{
		{"plain list", `{Smith, Jane and John Doe}`, []map[string]any{person("Smith", "Jane"), person("Doe", "John")}},
		{"corporate author", `{{Food and Agriculture Organization}}`, []map[string]any{org("Food and Agriculture Organization")}},
		{"corporate plus person", `{{Bill and Melinda Gates Foundation} and Doe, John}`, []map[string]any{org("Bill and Melinda Gates Foundation"), person("Doe", "John")}},
		{"quoted corporate", `"{World Health Organization}"`, []map[string]any{org("World Health Organization")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, err := parseBibTeXItems("@article{k, title={T}, author = "+tc.author+"}", "")
			if err != nil || len(items) != 1 {
				t.Fatalf("parse: %v %v", items, err)
			}
			got, _ := items[0]["creators"].([]map[string]any)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("creators = %v, want %v", got, tc.want)
			}
		})
	}
}
