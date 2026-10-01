// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import "testing"

// TestICRISParserLineStructure covers the RIS line shapes exporters emit beyond
// one-tag-per-line LF text: a UTF-8 byte order mark, wrapped (untagged)
// continuation lines, and stray text outside a record. Each case checks the
// parsed items and the offline record count the connector preview reports.
func TestICRISParserLineStructure(t *testing.T) {
	for _, tc := range []struct {
		name string
		ris  string
		want []map[string]string
	}{
		{
			name: "BOM before first TY keeps the record type",
			ris:  "\ufeffTY  - JOUR\nTI  - With BOM\nER  -\nTY  - BOOK\nTI  - Second\nER  -\n",
			want: []map[string]string{
				{"itemType": "journalArticle", "title": "With BOM"},
				{"itemType": "book", "title": "Second"},
			},
		},
		{
			name: "BOM with CRLF line endings",
			ris:  "\ufeffTY  - CHAP\r\nTI  - Windows Export\r\nER  - \r\n",
			want: []map[string]string{
				{"itemType": "bookSection", "title": "Windows Export"},
			},
		},
		{
			name: "wrapped abstract lines join the abstract",
			ris:  "TY  - JOUR\nTI  - Study\nAB  - The first part.\ntype of study was randomised;\nerror bars show SD.\nER  -\n",
			want: []map[string]string{
				{"itemType": "journalArticle", "title": "Study", "abstractNote": "The first part. type of study was randomised; error bars show SD."},
			},
		},
		{
			name: "indented title continuation joins the title",
			ris:  "TY  - JOUR\nTI  - A long title\n   that wraps\nJO  - Journal of\n  Wrapped Names\nER  -\n",
			want: []map[string]string{
				{"itemType": "journalArticle", "title": "A long title that wraps", "publicationTitle": "Journal of Wrapped Names"},
			},
		},
		{
			name: "continuation after a non-text tag does not leak into the title",
			ris:  "TY  - JOUR\nTI  - Kept\nAU  - Doe, Jane\nAffiliation line\nKW  - keyword\ntitle-like continuation\nER  -\n",
			want: []map[string]string{
				{"itemType": "journalArticle", "title": "Kept"},
			},
		},
		{
			name: "text outside a record creates no item",
			ris:  "Exported by a reference manager\nER  -\ntrailing prose\nTY  - RPRT\nTI  - Only\nER  -\nafterword\n",
			want: []map[string]string{
				{"itemType": "report", "title": "Only"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, err := parseRISItems(tc.ris, "")
			if err != nil {
				t.Fatalf("parseRISItems = %v, want nil", err)
			}
			if len(items) != len(tc.want) {
				t.Fatalf("items = %#v, want %d items", items, len(tc.want))
			}
			for i, want := range tc.want {
				for field, value := range want {
					if got := items[i][field]; got != value {
						t.Fatalf("item %d %s = %#v, want %q (item %#v)", i, field, got, value, items[i])
					}
				}
				for _, field := range []string{"title", "abstractNote", "publicationTitle"} {
					if _, ok := want[field]; !ok {
						if got, present := items[i][field]; present {
							t.Fatalf("item %d %s = %#v, want unset", i, field, got)
						}
					}
				}
			}
			if got := countImportFileRecords(tc.ris, "ris"); got != len(tc.want) {
				t.Fatalf("countImportFileRecords = %d, want %d", got, len(tc.want))
			}
		})
	}
}
