// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Read Zotero Word (.docx) and LibreOffice (.odt) citation fields.

package cli

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	// zoteroFieldsFormat is the manuscript format name bibcheck reports for
	// .docx and .odt files.
	zoteroFieldsFormat = "zotero-fields"
	// zoteroFieldKeyPrefix marks an occurrence that cites an item by Zotero
	// item key rather than by Better BibTeX citekey.
	zoteroFieldKeyPrefix = "zotero:"

	// A real citation field is a few kilobytes. These caps bound what a
	// hostile archive can make zotio read: the XML parts are decompressed
	// through a hard limit, and a single field instruction is capped.
	maxZoteroPartBytes  = 128 << 20
	maxZoteroFieldBytes = 8 << 20

	wordprocessingMLNS = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
	odfTextNS          = "urn:oasis:names:tc:opendocument:xmlns:text:1.0"
	odfMetaNS          = "urn:oasis:names:tc:opendocument:xmlns:meta:1.0"

	docxCustomPropsNS = "http://schemas.openxmlformats.org/officeDocument/2006/custom-properties"

	zoteroUnreadableKey = zoteroFieldKeyPrefix + "unreadable-field"
	zoteroBookmarkProp  = "ZOTERO_BREF_"
)

var zoteroFieldItemKeyRE = regexp.MustCompile(`^[A-Z0-9]{8}$`)

// zoteroRawField is one Zotero citation field code and the 1-based paragraph
// it sits in.
type zoteroRawField struct {
	code string
	para int
}

// zoteroPartElem is one item in a part's document order: a field, or a
// reference to a footnote or endnote whose fields belong at that position.
type zoteroPartElem struct {
	field    *zoteroRawField
	noteKind string
	noteID   string
	para     int
}

// parseZoteroFieldOccurrences reads the Zotero citation fields of a .docx or
// .odt file. Each citationItem becomes one occurrence located by the
// paragraph that holds the field.
func parseZoteroFieldOccurrences(path, ext string) ([]bibcheckOccurrence, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, usageErr(fmt.Errorf("reading manuscript %q: not a valid %s archive: %w", path, ext, err))
	}
	defer zr.Close()

	var fields []zoteroRawField
	switch ext {
	case ".docx":
		fields, err = zoteroDocxFields(&zr.Reader)
	default:
		fields, err = zoteroOdtFields(&zr.Reader)
	}
	if err != nil {
		return nil, fmt.Errorf("reading manuscript %q: %w", path, err)
	}

	group := activeGroupIDLocked()
	out := make([]bibcheckOccurrence, 0, len(fields))
	for _, f := range fields {
		out = append(out, zoteroFieldOccurrences(path, f, group)...)
	}
	return out, nil
}

// readZoteroZipPart returns the named archive member, or nil when absent. The
// declared size is checked first, then the decompressed stream is read
// through a hard limit, because a zip header can lie about its size.
func readZoteroZipPart(zr *zip.Reader, name string) ([]byte, error) {
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		if f.UncompressedSize64 > maxZoteroPartBytes {
			return nil, fmt.Errorf("%s is larger than %d MiB uncompressed", name, maxZoteroPartBytes>>20)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", name, err)
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, maxZoteroPartBytes+1))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		if len(data) > maxZoteroPartBytes {
			return nil, fmt.Errorf("%s is larger than %d MiB uncompressed", name, maxZoteroPartBytes>>20)
		}
		return data, nil
	}
	return nil, nil
}

func xmlAttr(attrs []xml.Attr, local string) string {
	for _, a := range attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

func isZoteroItemCode(code string) bool {
	return strings.Contains(code, "ZOTERO_ITEM") && strings.Contains(code, "CSL_CITATION")
}

// ---- .docx ----

func zoteroDocxFields(zr *zip.Reader) ([]zoteroRawField, error) {
	doc, err := readZoteroZipPart(zr, "word/document.xml")
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, usageErr(errors.New("not a Word document: word/document.xml is missing"))
	}
	custom, err := readZoteroZipPart(zr, "docProps/custom.xml")
	if err != nil {
		return nil, err
	}
	props, err := docxCustomProps(custom)
	if err != nil {
		return nil, fmt.Errorf("parsing docProps/custom.xml: %w", err)
	}
	body, _, err := scanDocxPart(doc, false, props)
	if err != nil {
		return nil, fmt.Errorf("parsing word/document.xml: %w", err)
	}
	notes := map[string]map[string][]zoteroPartElem{}
	for _, part := range []struct{ kind, name string }{{"footnote", "word/footnotes.xml"}, {"endnote", "word/endnotes.xml"}} {
		data, err := readZoteroZipPart(zr, part.name)
		if err != nil {
			return nil, err
		}
		if data == nil {
			continue
		}
		_, byID, err := scanDocxPart(data, true, props)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", part.name, err)
		}
		notes[part.kind] = byID
	}

	// Footnote and endnote fields are cited where the note is referenced.
	var out []zoteroRawField
	for _, e := range body {
		if e.field != nil {
			out = append(out, *e.field)
			continue
		}
		for _, n := range notes[e.noteKind][e.noteID] {
			if n.field != nil {
				out = append(out, zoteroRawField{code: n.field.code, para: e.para})
			}
		}
	}
	return out, nil
}

type docxField struct {
	instr  strings.Builder
	result bool
}

// scanDocxPart walks one WordprocessingML part. A field is a begin / instrText
// ... / separate / result / end run sequence, and Word splits its instruction
// over any number of instrText runs, so text is accumulated per open field and
// judged when the field ends. w:fldSimple carries its instruction in an
// attribute. A w:bookmarkStart named ZOTERO_BREF_* is a citation stored in
// Bookmarks mode: its code lives in the document custom properties (props),
// as for .odt. With notes set, fields are filed under the enclosing
// w:footnote / w:endnote id instead of the body.
func scanDocxPart(data []byte, notes bool, props map[string]string) ([]zoteroPartElem, map[string][]zoteroPartElem, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	var (
		body    []zoteroPartElem
		byID    = map[string][]zoteroPartElem{}
		stack   []*docxField
		para    int
		curNote string
		inInstr bool
	)
	add := func(e zoteroPartElem) {
		if notes {
			if curNote != "" {
				byID[curNote] = append(byID[curNote], e)
			}
			return
		}
		body = append(body, e)
	}
	emit := func(code string) {
		if isZoteroItemCode(code) {
			add(zoteroPartElem{field: &zoteroRawField{code: code, para: para}})
		}
	}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space != wordprocessingMLNS {
				continue
			}
			switch t.Name.Local {
			case "p":
				para++
			case "footnote", "endnote":
				if notes {
					curNote = xmlAttr(t.Attr, "id")
				}
			case "footnoteReference", "endnoteReference":
				if !notes {
					kind := "footnote"
					if t.Name.Local == "endnoteReference" {
						kind = "endnote"
					}
					body = append(body, zoteroPartElem{noteKind: kind, noteID: xmlAttr(t.Attr, "id"), para: para})
				}
			case "fldSimple":
				emit(xmlAttr(t.Attr, "instr"))
			case "bookmarkStart":
				// A bookmark that names a Zotero citation but has no readable
				// code is reported as unreadable, never dropped: it is still a
				// citation the gate has not seen.
				name := xmlAttr(t.Attr, "name")
				if !strings.HasPrefix(name, zoteroBookmarkProp) {
					continue
				}
				code := odfBookmarkCode(props, name)
				if code == "" || isZoteroItemCode(code) {
					add(zoteroPartElem{field: &zoteroRawField{code: code, para: para}})
				}
			case "fldChar":
				switch xmlAttr(t.Attr, "fldCharType") {
				case "begin":
					stack = append(stack, &docxField{})
				case "separate":
					if n := len(stack); n > 0 {
						stack[n-1].result = true
					}
				case "end":
					if n := len(stack); n > 0 {
						emit(stack[n-1].instr.String())
						stack = stack[:n-1]
					}
				}
			case "instrText":
				inInstr = true
			}
		case xml.CharData:
			if inInstr && len(stack) > 0 {
				top := stack[len(stack)-1]
				if !top.result {
					if top.instr.Len()+len(t) > maxZoteroFieldBytes {
						return nil, nil, fmt.Errorf("a field instruction is larger than %d MiB", maxZoteroFieldBytes>>20)
					}
					top.instr.Write(t)
				}
			}
		case xml.EndElement:
			if t.Name.Space != wordprocessingMLNS {
				continue
			}
			switch t.Name.Local {
			case "instrText":
				inInstr = false
			case "footnote", "endnote":
				if notes {
					curNote = ""
				}
			}
		}
	}
	// Fields still open at EOF are truncated; report each Zotero one as unreadable.
	for _, f := range stack {
		if strings.Contains(f.instr.String(), "ZOTERO_ITEM") {
			add(zoteroPartElem{field: &zoteroRawField{code: "", para: para}})
		}
	}
	return body, byID, nil
}

// docxCustomProps reads the name -> string value pairs of
// docProps/custom.xml. A missing part yields no properties.
func docxCustomProps(custom []byte) (map[string]string, error) {
	props := map[string]string{}
	if custom == nil {
		return props, nil
	}
	dec := xml.NewDecoder(bytes.NewReader(custom))
	var name string
	var val strings.Builder
	in := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return props, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space == docxCustomPropsNS && t.Name.Local == "property" {
				in = true
				name = xmlAttr(t.Attr, "name")
				val.Reset()
			}
		case xml.CharData:
			if in && val.Len()+len(t) <= maxZoteroFieldBytes {
				val.Write(t)
			}
		case xml.EndElement:
			if in && t.Name.Space == docxCustomPropsNS && t.Name.Local == "property" {
				if name != "" {
					props[name] = val.String()
				}
				in = false
			}
		}
	}
}

// ---- .odt ----

func zoteroOdtFields(zr *zip.Reader) ([]zoteroRawField, error) {
	content, err := readZoteroZipPart(zr, "content.xml")
	if err != nil {
		return nil, err
	}
	if content == nil {
		return nil, usageErr(errors.New("not an OpenDocument text file: content.xml is missing"))
	}
	meta, err := readZoteroZipPart(zr, "meta.xml")
	if err != nil {
		return nil, err
	}
	props, err := odfUserDefinedProps(meta)
	if err != nil {
		return nil, fmt.Errorf("parsing meta.xml: %w", err)
	}

	dec := xml.NewDecoder(bytes.NewReader(content))
	var out []zoteroRawField
	para := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parsing content.xml: %w", err)
		}
		t, ok := tok.(xml.StartElement)
		if !ok || t.Name.Space != odfTextNS {
			continue
		}
		switch t.Name.Local {
		case "p", "h":
			para++
		case "reference-mark-start", "reference-mark":
			// The Zotero plugin stores the field code in the mark's name:
			// "ZOTERO_ITEM CSL_CITATION {json} RND<random>".
			if name := xmlAttr(t.Attr, "name"); isZoteroItemCode(name) {
				if len(name) > maxZoteroFieldBytes {
					return nil, fmt.Errorf("a reference mark name is larger than %d MiB", maxZoteroFieldBytes>>20)
				}
				out = append(out, zoteroRawField{code: name, para: para})
			}
		case "bookmark-start", "bookmark":
			// Bookmark fields cannot hold a long code in a name, so the
			// plugin keeps it in a document property of the same name,
			// split into 255-character pieces ZOTERO_BREF_x_1, _2, ...
			name := xmlAttr(t.Attr, "name")
			if !strings.HasPrefix(name, zoteroBookmarkProp) {
				continue
			}
			code := odfBookmarkCode(props, name)
			switch {
			case isZoteroItemCode(code):
				out = append(out, zoteroRawField{code: code, para: para})
			case code == "":
				out = append(out, zoteroRawField{code: "", para: para})
			}
		}
	}
	return out, nil
}

// odfUserDefinedProps reads the meta:user-defined properties of meta.xml.
func odfUserDefinedProps(meta []byte) (map[string]string, error) {
	props := map[string]string{}
	if meta == nil {
		return props, nil
	}
	dec := xml.NewDecoder(bytes.NewReader(meta))
	var name string
	var val strings.Builder
	in := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return props, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space == odfMetaNS && t.Name.Local == "user-defined" {
				in = true
				name = xmlAttr(t.Attr, "name")
				val.Reset()
			}
		case xml.CharData:
			if in && val.Len()+len(t) <= maxZoteroFieldBytes {
				val.Write(t)
			}
		case xml.EndElement:
			if in && t.Name.Space == odfMetaNS && t.Name.Local == "user-defined" {
				if name != "" {
					props[name] = val.String()
				}
				in = false
			}
		}
	}
}

// odfBookmarkCode joins the property pieces for a bookmark. LibreOffice
// appends a number to the name of a copied bookmark, so up to three trailing
// characters are dropped when the exact name has no code.
func odfBookmarkCode(props map[string]string, name string) string {
	for drop := 0; drop <= 3 && drop < len(name); drop++ {
		base := name[:len(name)-drop]
		var b strings.Builder
		for i := 1; ; i++ {
			piece, ok := props[base+"_"+strconv.Itoa(i)]
			if !ok {
				break
			}
			b.WriteString(piece)
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	return ""
}

// ---- field code -> occurrences ----

type zoteroCitationPayload struct {
	CitationItems []struct {
		ID   json.RawMessage `json:"id"`
		URIs json.RawMessage `json:"uris"`
		URI  json.RawMessage `json:"uri"`
	} `json:"citationItems"`
}

// zoteroFieldOccurrences turns one field code into occurrences, one per cited
// item. A field whose code cannot be read yields one unknown occurrence
// rather than vanishing, so a damaged field cannot hide a citation from the
// gate.
func zoteroFieldOccurrences(path string, f zoteroRawField, group string) []bibcheckOccurrence {
	occ := func(key, itemKey string) bibcheckOccurrence {
		return bibcheckOccurrence{CiteKey: key, File: path, Line: f.para, Format: zoteroFieldsFormat, ItemKey: itemKey}
	}
	idx := strings.Index(f.code, "CSL_CITATION")
	if idx < 0 {
		return []bibcheckOccurrence{occ(zoteroUnreadableKey, "")}
	}
	var payload zoteroCitationPayload
	if err := json.NewDecoder(strings.NewReader(strings.TrimSpace(f.code[idx+len("CSL_CITATION"):]))).Decode(&payload); err != nil {
		return []bibcheckOccurrence{occ(zoteroUnreadableKey, "")}
	}
	var out []bibcheckOccurrence
	for _, item := range payload.CitationItems {
		uris := append(zoteroURIStrings(item.URIs), zoteroURIStrings(item.URI)...)
		lib, key, ok := chooseZoteroItemURI(uris, group)
		switch {
		case !ok:
			id := strings.Trim(string(item.ID), `" `)
			if id == "" {
				id = "no-uri"
			}
			out = append(out, occ(zoteroFieldKeyPrefix+"item-without-uri:"+id, ""))
		case zoteroLibraryMatches(lib, group):
			out = append(out, occ(zoteroFieldKeyPrefix+key, key))
		default:
			out = append(out, occ(zoteroFieldKeyPrefix+lib+"/"+key, ""))
		}
	}
	if len(out) == 0 {
		return []bibcheckOccurrence{occ(zoteroUnreadableKey, "")}
	}
	return out
}

// zoteroURIStrings accepts the "uris" array and the legacy "uri" array or
// string.
func zoteroURIStrings(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	return nil
}

// chooseZoteroItemURI picks the item URI for a citation item: the one in the
// targeted library when the item lists several, else the first parseable one.
func chooseZoteroItemURI(uris []string, group string) (lib, key string, ok bool) {
	for _, raw := range uris {
		l, k, parsed := parseZoteroItemURI(raw)
		if !parsed {
			continue
		}
		if zoteroLibraryMatches(l, group) {
			return l, k, true
		}
		if !ok {
			lib, key, ok = l, k, true
		}
	}
	return lib, key, ok
}

// parseZoteroItemURI reads http://zotero.org/users/<id>/items/<KEY>,
// .../users/local/<id>/items/<KEY>, and .../groups/<id>/items/<KEY>. lib is
// the path before "/items", such as "groups/123".
func parseZoteroItemURI(raw string) (lib, key string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", false
	}
	if host := strings.ToLower(u.Hostname()); host != "zotero.org" && host != "www.zotero.org" {
		return "", "", false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	n := len(segs)
	if n < 4 || segs[n-2] != "items" || !zoteroFieldItemKeyRE.MatchString(segs[n-1]) {
		return "", "", false
	}
	lib = strings.Join(segs[:n-2], "/")
	switch {
	case n == 4 && (segs[0] == "users" || segs[0] == "groups") && segs[1] != "":
	case n == 5 && segs[0] == "users" && segs[1] == "local" && segs[2] != "":
	default:
		return "", "", false
	}
	return lib, segs[n-1], true
}

// zoteroLibraryMatches reports whether a URI library is the one the command
// targets: group <gid> for --group, otherwise any personal library. The store
// does not record the owner's numeric user ID, so a personal URI is matched by
// kind (users/<id> or users/local/<id>), not by id.
func zoteroLibraryMatches(lib, group string) bool {
	if group != "" {
		return lib == "groups/"+group
	}
	return strings.HasPrefix(lib, "users/")
}

// bibcheckUndefinedTitle titles an unresolved citation: a Zotero field cites
// an item key, a TeX or Pandoc file cites a citekey.
func bibcheckUndefinedTitle(citeKey string) string {
	if strings.HasPrefix(citeKey, zoteroFieldKeyPrefix) {
		return fmt.Sprintf("Unknown Zotero item %s", strings.TrimPrefix(citeKey, zoteroFieldKeyPrefix))
	}
	return fmt.Sprintf("Undefined citekey %s", citeKey)
}

func bibcheckUndefinedAction(citeKey string, def *RecommendedAction) *RecommendedAction {
	if strings.HasPrefix(citeKey, zoteroFieldKeyPrefix) {
		return &RecommendedAction{Text: "Restore the item in Zotero (it is missing, deleted, trashed, or in another library), or refresh or remove the citation in the document"}
	}
	return def
}
