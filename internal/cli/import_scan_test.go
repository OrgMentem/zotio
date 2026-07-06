// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean q1ia): cover DOI extraction (filename slash-decoding + embedded
// content), library-aware classification, and the DOI index build.

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"zotio/internal/store"
)

func writeScanFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestExtractPDFDOI(t *testing.T) {
	dir := t.TempDir()

	// Filename with a slash-encoded DOI (%2F) and no DOI in content.
	if doi, src := extractPDFDOI(writeScanFile(t, dir, "10.1234%2Fjournal.2020.5.pdf", "no doi in body")); doi != "10.1234/journal.2020.5" || src != "filename" {
		t.Errorf("filename DOI = (%q,%q), want (10.1234/journal.2020.5, filename)", doi, src)
	}
	// DOI only in embedded content (filename has none).
	if doi, src := extractPDFDOI(writeScanFile(t, dir, "paper.pdf", "%PDF-1.5\n<rdf>doi:10.5555/abc.def</rdf>")); doi != "10.5555/abc.def" || src != "content" {
		t.Errorf("content DOI = (%q,%q), want (10.5555/abc.def, content)", doi, src)
	}
	// No DOI anywhere.
	if doi, src := extractPDFDOI(writeScanFile(t, dir, "scan001.pdf", "binary-ish bytes, no identifier")); doi != "" || src != "none" {
		t.Errorf("no DOI = (%q,%q), want (\"\", none)", doi, src)
	}
}

func TestClassifyPDF(t *testing.T) {
	dir := t.TempDir()
	idx := libraryDOIIndex{byDOI: map[string]libItem{
		"10.1000/dup": {key: "K1", title: "Dup", hasPDF: true},
		"10.2000/att": {key: "K2", title: "Att", hasPDF: false},
	}}
	ctx := context.Background()

	if r := classifyPDF(ctx, writeScanFile(t, dir, "a.pdf", "doi 10.1000/dup here"), idx, nil); r.Status != "duplicate" || r.ItemKey != "K1" {
		t.Errorf("duplicate: got status=%q key=%q", r.Status, r.ItemKey)
	}
	if r := classifyPDF(ctx, writeScanFile(t, dir, "b.pdf", "doi 10.2000/att here"), idx, nil); r.Status != "attach_candidate" || r.ItemKey != "K2" {
		t.Errorf("attach_candidate: got status=%q key=%q", r.Status, r.ItemKey)
	}
	if r := classifyPDF(ctx, writeScanFile(t, dir, "c.pdf", "doi 10.9999/new here"), idx, nil); r.Status != "new" || r.DOI != "10.9999/new" {
		t.Errorf("new: got status=%q doi=%q", r.Status, r.DOI)
	}
	if r := classifyPDF(ctx, writeScanFile(t, dir, "d.pdf", "nothing useful"), idx, nil); r.Status != "unidentified" {
		t.Errorf("unidentified: got status=%q", r.Status)
	}
}

func TestBuildLibraryDOIIndex(t *testing.T) {
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	items := []json.RawMessage{
		json.RawMessage(`{"key":"I1","data":{"key":"I1","itemType":"journalArticle","title":"A","DOI":"10.1/A"}}`),
		json.RawMessage(`{"key":"I2","data":{"key":"I2","itemType":"journalArticle","title":"B","DOI":"10.2/b"}}`),
		json.RawMessage(`{"key":"I3","data":{"key":"I3","itemType":"book","title":"C"}}`),
		json.RawMessage(`{"key":"ATT","data":{"key":"ATT","itemType":"attachment","parentItem":"I1","contentType":"application/pdf"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}

	idx, err := buildLibraryDOIIndex(db)
	if err != nil {
		t.Fatalf("buildLibraryDOIIndex: %v", err)
	}
	if len(idx.byDOI) != 2 {
		t.Errorf("indexed %d DOIs, want 2 (%v)", len(idx.byDOI), idx.byDOI)
	}
	// Case-insensitive key; I1 has a PDF attachment.
	if li, ok := idx.byDOI["10.1/a"]; !ok || li.key != "I1" || !li.hasPDF {
		t.Errorf("I1 entry = %+v (ok=%v), want key I1 hasPDF true", li, ok)
	}
	if li := idx.byDOI["10.2/b"]; li.hasPDF {
		t.Errorf("I2 should have no PDF, got hasPDF true")
	}
}
