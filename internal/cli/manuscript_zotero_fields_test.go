// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	zfNSW      = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`
	zfNSOffice = `xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0"`
	zfNSText   = `xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"`
	zfNSMeta   = `xmlns:meta="urn:oasis:names:tc:opendocument:xmlns:meta:1.0"`
)

// zfCode builds a Zotero field code citing the given item URIs, one
// citationItem each.
func zfCode(uris ...string) string {
	items := make([]string, 0, len(uris))
	for i, uri := range uris {
		items = append(items, fmt.Sprintf(`{"id":%d,"uris":[%q],"itemData":{"type":"article-journal"}}`, i+1, uri))
	}
	return `ADDIN ZOTERO_ITEM CSL_CITATION {"citationID":"c1","properties":{"noteIndex":0},"citationItems":[` + strings.Join(items, ",") + `],"schema":"https://github.com/citation-style-language/schema/raw/master/csl-citation.json"}`
}

func zfUser(key string) string  { return "http://zotero.org/users/123/items/" + key }
func zfGroup(key string) string { return "http://zotero.org/groups/77/items/" + key }

// zfChunks splits s into n roughly equal pieces.
func zfChunks(s string, n int) []string {
	size := (len(s) + n - 1) / n
	var out []string
	for len(s) > size {
		out = append(out, s[:size])
		s = s[size:]
	}
	return append(out, s)
}

// zfDocxField renders a complex field whose instruction is split over
// several instrText runs.
func zfDocxField(code string, runs int) string {
	var b strings.Builder
	b.WriteString(`<w:r><w:fldChar w:fldCharType="begin"/></w:r>`)
	for _, c := range zfChunks(code, runs) {
		b.WriteString(`<w:r><w:instrText xml:space="preserve">` + c + `</w:instrText></w:r>`)
	}
	b.WriteString(`<w:r><w:fldChar w:fldCharType="separate"/></w:r><w:r><w:t>(cited)</w:t></w:r><w:r><w:fldChar w:fldCharType="end"/></w:r>`)
	return b.String()
}

func zfDocxSimple(code string) string {
	return `<w:fldSimple w:instr=" ` + strings.NewReplacer(`"`, "&quot;").Replace(code) + ` "><w:r><w:t>(cited)</w:t></w:r></w:fldSimple>`
}

func zfWriteZip(t *testing.T, path string, parts map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	zw := zip.NewWriter(f)
	for name, body := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

func zfDocx(t *testing.T, dir, name string, paragraphs ...string) string {
	t.Helper()
	var body strings.Builder
	for _, p := range paragraphs {
		body.WriteString("<w:p>" + p + "</w:p>")
	}
	path := filepath.Join(dir, name)
	zfWriteZip(t, path, map[string]string{
		"word/document.xml": `<?xml version="1.0" encoding="UTF-8"?><w:document ` + zfNSW + `><w:body>` + body.String() + `</w:body></w:document>`,
	})
	return path
}

func zfOdtMark(code string) string {
	name := strings.NewReplacer("&", "&amp;", `"`, "&quot;", "<", "&lt;").Replace(strings.TrimPrefix(code, "ADDIN ") + " RNDabcdefghij")
	return `<text:reference-mark-start text:name="` + name + `"/>(cited)<text:reference-mark-end text:name="` + name + `"/>`
}

func zfOdt(t *testing.T, dir, name, meta string, paragraphs ...string) string {
	t.Helper()
	var body strings.Builder
	for _, p := range paragraphs {
		body.WriteString("<text:p>" + p + "</text:p>")
	}
	parts := map[string]string{
		"content.xml": `<?xml version="1.0" encoding="UTF-8"?><office:document-content ` + zfNSOffice + ` ` + zfNSText + `><office:body><office:text>` + body.String() + `</office:text></office:body></office:document-content>`,
	}
	if meta != "" {
		parts["meta.xml"] = `<?xml version="1.0" encoding="UTF-8"?><office:document-meta ` + zfNSOffice + ` ` + zfNSMeta + `><office:meta>` + meta + `</office:meta></office:document-meta>`
	}
	path := filepath.Join(dir, name)
	zfWriteZip(t, path, parts)
	return path
}

func zfSeed(t *testing.T) {
	t.Helper()
	cliTestSeedItemsStore(t, []json.RawMessage{
		json.RawMessage(`{"key":"AAAA1111","version":1,"data":{"key":"AAAA1111","itemType":"journalArticle","title":"Alpha","creators":[{"lastName":"Doe"}],"date":"2024","publicationTitle":"J"}}`),
		json.RawMessage(`{"key":"BBBB2222","version":1,"data":{"key":"BBBB2222","itemType":"journalArticle","title":"Beta","creators":[{"lastName":"Roe"}],"date":"2023","publicationTitle":"J"}}`),
	})
}

func TestZoteroFieldsDocxFieldCodeSplitAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	// Four paragraphs: field in 1 (split over 5 runs), fldSimple in 3, a
	// multi-item field in 4. Footnote fields are cited at their reference.
	path := zfDocx(t, dir, "paper.docx",
		zfDocxField(zfCode(zfUser("AAAA1111")), 5),
		`<w:r><w:t>plain</w:t></w:r>`,
		zfDocxSimple(zfCode(zfUser("BBBB2222"))),
		zfDocxField(zfCode(zfUser("ZZZZ9999"), zfGroup("CCCC3333")), 3),
	)
	got, format, err := parseManuscriptCitationOccurrences(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if format != "zotero-fields" {
		t.Fatalf("format = %q, want zotero-fields", format)
	}
	type row struct {
		Key  string
		Line int
		Item string
	}
	var rows []row
	for _, o := range got {
		rows = append(rows, row{o.CiteKey, o.Line, o.ItemKey})
	}
	want := []row{
		{"zotero:AAAA1111", 1, "AAAA1111"},
		{"zotero:BBBB2222", 3, "BBBB2222"},
		{"zotero:ZZZZ9999", 4, "ZZZZ9999"},
		{"zotero:groups/77/CCCC3333", 4, ""},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("occurrences = %+v, want %+v", rows, want)
	}
}

func TestZoteroFieldsDocxFootnoteCitedAtReference(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.docx")
	zfWriteZip(t, path, map[string]string{
		"word/document.xml": `<w:document ` + zfNSW + `><w:body>` +
			`<w:p><w:r><w:t>a</w:t></w:r></w:p>` +
			`<w:p><w:r><w:footnoteReference w:id="2"/></w:r></w:p>` +
			`<w:p>` + zfDocxField(zfCode(zfUser("AAAA1111")), 2) + `</w:p>` +
			`</w:body></w:document>`,
		"word/footnotes.xml": `<w:footnotes ` + zfNSW + `><w:footnote w:id="2"><w:p>` + zfDocxField(zfCode(zfUser("BBBB2222")), 2) + `</w:p></w:footnote></w:footnotes>`,
	})
	got, _, err := parseManuscriptCitationOccurrences(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if keys := citeKeysFromOccurrences(got); !reflect.DeepEqual(keys, []string{"zotero:BBBB2222", "zotero:AAAA1111"}) {
		t.Fatalf("keys = %v, want footnote citation first (cited at its reference)", keys)
	}
	if got[0].Line != 2 || got[1].Line != 3 {
		t.Fatalf("lines = %d,%d, want 2,3", got[0].Line, got[1].Line)
	}
}

func TestZoteroFieldsOdtReferenceMarksAndBookmarks(t *testing.T) {
	dir := t.TempDir()
	bookmarkCode := strings.TrimPrefix(zfCode(zfUser("BBBB2222")), "ADDIN ")
	var meta strings.Builder
	for i, c := range zfChunks(bookmarkCode, 4) {
		fmt.Fprintf(&meta, `<meta:user-defined meta:name="ZOTERO_BREF_xyz_%d">%s</meta:user-defined>`, i+1, strings.NewReplacer("&", "&amp;", "<", "&lt;").Replace(c))
	}
	// A bibliography bookmark must not be mistaken for a citation.
	meta.WriteString(`<meta:user-defined meta:name="ZOTERO_BREF_bib_1">ZOTERO_BIBL {"uncited":[]} CSL_BIBLIOGRAPHY</meta:user-defined>`)
	path := zfOdt(t, dir, "paper.odt", meta.String(),
		`intro `+zfOdtMark(zfCode(zfUser("AAAA1111"))),
		`mid <text:bookmark-start text:name="ZOTERO_BREF_xyz"/>(cited)<text:bookmark-end text:name="ZOTERO_BREF_xyz"/>`,
		`<text:bookmark-start text:name="ZOTERO_BREF_bib"/>refs<text:bookmark-end text:name="ZOTERO_BREF_bib"/>`,
	)
	got, format, err := parseManuscriptCitationOccurrences(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if format != "zotero-fields" {
		t.Fatalf("format = %q", format)
	}
	if keys := citeKeysFromOccurrences(got); !reflect.DeepEqual(keys, []string{"zotero:AAAA1111", "zotero:BBBB2222"}) {
		t.Fatalf("keys = %v", keys)
	}
	if got[0].Line != 1 || got[1].Line != 2 {
		t.Fatalf("lines = %d,%d, want 1,2", got[0].Line, got[1].Line)
	}
}

func TestZoteroFieldOccurrencesLibraryResolution(t *testing.T) {
	f := zoteroRawField{code: zfCode(zfUser("AAAA1111"), zfGroup("BBBB2222"), "http://zotero.org/users/local/abcd1234/items/CCCC3333"), para: 7}
	keys := func(group string) []string {
		var out []string
		for _, o := range zoteroFieldOccurrences("p.docx", f, group) {
			out = append(out, o.CiteKey+"|"+o.ItemKey)
		}
		return out
	}
	if got, want := keys(""), []string{"zotero:AAAA1111|AAAA1111", "zotero:groups/77/BBBB2222|", "zotero:CCCC3333|CCCC3333"}; !reflect.DeepEqual(got, want) {
		t.Errorf("personal = %v, want %v", got, want)
	}
	if got, want := keys("77"), []string{"zotero:users/123/AAAA1111|", "zotero:BBBB2222|BBBB2222", "zotero:users/local/abcd1234/CCCC3333|"}; !reflect.DeepEqual(got, want) {
		t.Errorf("group 77 = %v, want %v", got, want)
	}
	if got := keys("78"); got[1] != "zotero:groups/77/BBBB2222|" {
		t.Errorf("group 78 = %v, want group 77 citation foreign", got)
	}
}

func TestZoteroFieldOccurrencesLegacyAndUnreadable(t *testing.T) {
	legacy := zoteroRawField{code: `ADDIN ZOTERO_ITEM CSL_CITATION {"citationItems":[{"id":9,"uri":"http://zotero.org/users/5/items/AAAA1111"},{"id":10,"uri":["http://zotero.org/users/5/items/BBBB2222"]},{"id":11,"uri":[]}]} RND123`, para: 1}
	var got []string
	for _, o := range zoteroFieldOccurrences("p", legacy, "") {
		got = append(got, o.CiteKey)
	}
	if want := []string{"zotero:AAAA1111", "zotero:BBBB2222", "zotero:item-without-uri:11"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy = %v, want %v", got, want)
	}
	broken := zoteroFieldOccurrences("p", zoteroRawField{code: `ADDIN ZOTERO_ITEM CSL_CITATION {"citationItems":[`, para: 3}, "")
	if len(broken) != 1 || broken[0].CiteKey != zoteroUnreadableKey || broken[0].Line != 3 {
		t.Fatalf("broken field = %+v, want one unreadable occurrence at paragraph 3", broken)
	}
}

func TestZoteroFieldsEmptyPayloadIsUnreadable(t *testing.T) {
	for _, payload := range []string{`{}`, `null`, `[]`, `{"citationItems":[]}`} {
		got := zoteroFieldOccurrences("p", zoteroRawField{code: `ADDIN ZOTERO_ITEM CSL_CITATION ` + payload + ` RND1`, para: 2}, "")
		if len(got) != 1 || got[0].CiteKey != zoteroUnreadableKey || got[0].Line != 2 {
			t.Fatalf("payload %s = %+v, want one unreadable occurrence", payload, got)
		}
	}
}

func TestZoteroFieldsBibcheckGatesOnUnreadableDocxFields(t *testing.T) {
	home := bibcheckIsolatedHome(t)
	zfSeed(t)
	a := zfCode(zfUser("AAAA1111"))
	truncated := `<w:r><w:fldChar w:fldCharType="begin"/></w:r><w:r><w:instrText>` + strings.NewReplacer("&", "&amp;", "<", "&lt;").Replace(zfCode(zfUser("BBBB2222"))) + `</w:instrText></w:r>`
	for name, bad := range map[string]string{"truncated": truncated, "empty": zfDocxSimple(`ADDIN ZOTERO_ITEM CSL_CITATION {} RND1`)} {
		t.Run(name, func(t *testing.T) {
			path := zfDocx(t, home, name+".docx", zfDocxField(a, 2), bad)
			report, _, err := runBibcheckJSON(t, path, "--fail-on-unknown")
			if ExitCode(err) != 11 {
				t.Fatalf("ExitCode = %d (err=%v), want 11", ExitCode(err), err)
			}
			if report.Summary.Unknown != 1 {
				t.Fatalf("summary = %+v, want 1 unknown", report.Summary)
			}
		})
	}
}

func TestZoteroFieldsNotAnArchiveIsUsageError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paper.docx")
	writeTestFile(t, path, "not a zip")
	_, _, err := parseManuscriptCitationOccurrences(path)
	if ExitCode(err) != 2 {
		t.Fatalf("ExitCode = %d (err=%v), want usage error 2", ExitCode(err), err)
	}
}

func TestZoteroFieldsUnsupportedExtensionListsNewFormats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paper.rtf")
	writeTestFile(t, path, "x")
	_, _, err := parseManuscriptCitationOccurrences(path)
	if err == nil || !strings.Contains(err.Error(), ".docx, .odt") {
		t.Fatalf("error = %v, want supported formats to list .docx and .odt", err)
	}
}

func TestZoteroFieldsBibcheckGatesOnMissingItem(t *testing.T) {
	for _, ext := range []string{".docx", ".odt"} {
		t.Run(ext, func(t *testing.T) {
			home := bibcheckIsolatedHome(t)
			zfSeed(t)
			a, b, missing := zfCode(zfUser("AAAA1111")), zfCode(zfUser("BBBB2222")), zfCode(zfUser("ZZZZ9999"))
			var path string
			if ext == ".docx" {
				path = zfDocx(t, home, "paper.docx", zfDocxField(a, 6), zfDocxSimple(b), zfDocxField(missing, 4))
			} else {
				path = zfOdt(t, home, "paper.odt", "", zfOdtMark(a), zfOdtMark(b), zfOdtMark(missing))
			}

			report, _, err := runBibcheckJSON(t, path, "--fail-on-unknown")
			if ExitCode(err) != 11 {
				t.Fatalf("ExitCode = %d (err=%v), want 11", ExitCode(err), err)
			}
			if report.Format != "zotero-fields" {
				t.Fatalf("format = %q, want zotero-fields", report.Format)
			}
			if report.Summary.Total != 3 || report.Summary.OK != 2 || report.Summary.Unknown != 1 {
				t.Fatalf("summary = %+v, want 3 keys: 2 ok, 1 unknown", report.Summary)
			}
			undefined := bibcheckFindingByKind(t, report, "undefined_citekey")
			if !strings.Contains(undefined.Title, "ZZZZ9999") || undefined.Severity != sevHigh {
				t.Fatalf("finding = %+v, want high finding naming ZZZZ9999", undefined)
			}
			if got := evidenceString(t, undefined, "file_line"); got != path+":3" {
				t.Fatalf("file_line = %q, want %q", got, path+":3")
			}
			if undefined.RecommendedAction == nil || undefined.RecommendedAction.Text == "" {
				t.Fatalf("finding has no recommended action: %+v", undefined)
			}
			for _, key := range report.Keys {
				if len(key.Suggestions) != 0 {
					t.Fatalf("key %q has citekey suggestions, want none for item-key citations", key.CiteKey)
				}
			}
		})
	}
}

func TestZoteroFieldsBibliographyManuscriptEmitsExactlyCitedItems(t *testing.T) {
	for _, ext := range []string{".docx", ".odt"} {
		t.Run(ext, func(t *testing.T) {
			isolateCSLTestEnv(t)
			mbSeedCiteKeys(t, map[string]string{"AAAA1111": "alpha", "BBBB2222": "beta", "CCCC3333": "gamma"})
			api := mbStartBibServer(t)
			dir := t.TempDir()
			a, b, missing := zfCode(zfUser("AAAA1111")), zfCode(zfUser("BBBB2222")), zfCode(zfUser("ZZZZ9999"))

			build := func(name string, codes ...string) string {
				if ext == ".docx" {
					paras := make([]string, 0, len(codes))
					for i, c := range codes {
						if i%2 == 0 {
							paras = append(paras, zfDocxField(c, 5))
						} else {
							paras = append(paras, zfDocxSimple(c))
						}
					}
					return zfDocx(t, dir, name+ext, paras...)
				}
				paras := make([]string, 0, len(codes))
				for _, c := range codes {
					paras = append(paras, zfOdtMark(c))
				}
				return zfOdt(t, dir, name+ext, "", paras...)
			}

			// An unresolvable item refuses the bibliography, as for other formats.
			bad := build("bad", a, missing, b)
			out, err := runCSLItemsCommand(t, &rootFlags{noCache: true}, []string{"bibliography", "--manuscript", bad})
			if ExitCode(err) != 11 || !strings.Contains(err.Error(), "ZZZZ9999") || !strings.Contains(err.Error(), "bad"+ext+":2") {
				t.Fatalf("unresolved: ExitCode=%d err=%v, want 11 naming ZZZZ9999 at paragraph 2", ExitCode(err), err)
			}
			if out.Len() != 0 || len(api.snapshot()) != 0 {
				t.Fatalf("unresolved: output %q, requests %v, want none", out.String(), api.snapshot())
			}

			// B is cited first, then A twice: exactly two items, first-citation order.
			good := build("good", b, a, a)
			out, err = runCSLItemsCommand(t, &rootFlags{noCache: true}, []string{"bibliography", "--manuscript", good})
			if err != nil {
				t.Fatalf("items bibliography --manuscript: %v", err)
			}
			if got, want := api.snapshot(), [][]string{{"BBBB2222", "AAAA1111"}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("bib requests = %v, want %v (uncited CCCC3333 absent)", got, want)
			}
			if out.String() != "chunk1:BBBB2222,AAAA1111" {
				t.Fatalf("output = %q", out.String())
			}
		})
	}
}
