// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// content), library-aware classification, and the DOI index build.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	if doi, src, err := extractPDFDOI(writeScanFile(t, dir, "10.1234%2Fjournal.2020.5.pdf", "no doi in body")); err != nil || doi != "10.1234/journal.2020.5" || src != "filename" {
		t.Errorf("filename DOI = (%q,%q,%v), want (10.1234/journal.2020.5, filename, nil)", doi, src, err)
	}
	// DOI only in embedded content (filename has none).
	if doi, src, err := extractPDFDOI(writeScanFile(t, dir, "paper.pdf", "%PDF-1.5\n<rdf>doi:10.5555/abc.def</rdf>")); err != nil || doi != "10.5555/abc.def" || src != "content" {
		t.Errorf("content DOI = (%q,%q,%v), want (10.5555/abc.def, content, nil)", doi, src, err)
	}
	// No DOI anywhere.
	if doi, src, err := extractPDFDOI(writeScanFile(t, dir, "scan001.pdf", "binary-ish bytes, no identifier")); err != nil || doi != "" || src != "none" {
		t.Errorf("no DOI = (%q,%q,%v), want (\"\", none, nil)", doi, src, err)
	}
}

func TestClassifyPDF(t *testing.T) {
	dir := t.TempDir()
	idx := libraryDOIIndex{byDOI: map[string]libItem{
		"10.1000/dup": {key: "K1", title: "Dup", hasPDF: true},
		"10.2000/att": {key: "K2", title: "Att", hasPDF: false},
	}}
	ctx := context.Background()

	if r, err := classifyPDFWithErr(ctx, writeScanFile(t, dir, "a.pdf", "doi 10.1000/dup here"), idx, nil); err != nil || r.Status != "duplicate" || r.ItemKey != "K1" {
		t.Errorf("duplicate: got result=%+v err=%v", r, err)
	}
	if r, err := classifyPDFWithErr(ctx, writeScanFile(t, dir, "b.pdf", "doi 10.2000/att here"), idx, nil); err != nil || r.Status != "attach_candidate" || r.ItemKey != "K2" {
		t.Errorf("attach_candidate: got result=%+v err=%v", r, err)
	}
	if r, err := classifyPDFWithErr(ctx, writeScanFile(t, dir, "c.pdf", "doi 10.9999/new here"), idx, nil); err != nil || r.Status != "new" || r.DOI != "10.9999/new" {
		t.Errorf("new: got result=%+v err=%v", r, err)
	}
	if r, err := classifyPDFWithErr(ctx, writeScanFile(t, dir, "d.pdf", "nothing useful"), idx, nil); err != nil || r.Status != "unidentified" {
		t.Errorf("unidentified: got result=%+v err=%v", r, err)
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

func TestExtractPDFDOIReadError(t *testing.T) {
	if _, _, err := extractPDFDOI(t.TempDir()); err == nil {
		t.Fatal("extractPDFDOI directory error = nil, want read error")
	}
}

func TestItemsWithPDFSetPropagatesReadError(t *testing.T) {
	db, err := store.OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if _, err := itemsWithPDFSet(db); err == nil {
		t.Fatal("itemsWithPDFSet error = nil, want closed-store read error")
	}
}

func TestImportScanUnreadablePDFProducesWarning(t *testing.T) {
	isolateDemoEnv(t, "0")
	dbPath := defaultDBPath("zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	dir := t.TempDir()
	broken := filepath.Join(dir, "unreadable.pdf")
	if err := os.Symlink(filepath.Join(dir, "missing.pdf"), broken); err != nil {
		t.Fatalf("create broken symlink: %v", err)
	}
	cmd := newImportScanCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{dir})
	err = cmd.Execute()
	if ExitCode(err) != 13 {
		t.Fatalf("ExitCode(%v) = %d, want 13", err, ExitCode(err))
	}
	var report scanReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode report %q: %v", out.String(), err)
	}
	if len(report.Results) != 0 || len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], broken) || !strings.Contains(report.Warnings[0], "opening") {
		t.Fatalf("report = %+v, want no result and path-qualified read warning", report)
	}
	if errOut.Len() != 0 {
		t.Fatalf("JSON stderr = %q, want no warnings outside result", errOut.String())
	}

	humanCmd := newImportScanCmd(&rootFlags{})
	humanCmd.SilenceErrors, humanCmd.SilenceUsage = true, true
	var humanOut, humanErr bytes.Buffer
	humanCmd.SetOut(&humanOut)
	humanCmd.SetErr(&humanErr)
	humanCmd.SetArgs([]string{dir})
	err = humanCmd.Execute()
	if ExitCode(err) != 13 {
		t.Fatalf("human ExitCode(%v) = %d, want 13", err, ExitCode(err))
	}
	if !strings.Contains(humanOut.String(), "Scanned 1 PDF(s)") || !strings.Contains(humanErr.String(), broken) || !strings.Contains(humanErr.String(), "warning: reading PDF") {
		t.Fatalf("human output=%q stderr=%q, want scan report and path-qualified warning", humanOut.String(), humanErr.String())
	}
}

func TestImportScanMissingStoreGuidesSync(t *testing.T) {
	isolateDemoEnv(t, "0")
	cmd := newImportScanCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{t.TempDir()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("scan with missing store: %v", err)
	}
	if got := out.String(); got != "Run 'zotio sync' first.\n" {
		t.Fatalf("stdout = %q, want sync guidance", got)
	}
}

func TestImportScanStoreOpenFailureDoesNotLookMissing(t *testing.T) {
	isolateDemoEnv(t, "0")
	dbPath := defaultDBPath("zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir db dir: %v", err)
	}
	if err := os.WriteFile(dbPath, []byte("not a SQLite database"), 0o600); err != nil {
		t.Fatalf("write corrupt db: %v", err)
	}
	cmd := newImportScanCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{t.TempDir()})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "opening local database") {
		t.Fatalf("scan error = %v, want contextual store-open failure", err)
	}
	if strings.Contains(out.String(), "Run 'zotio sync' first.") {
		t.Fatalf("stdout = %q, must not misclassify corrupt store as missing", out.String())
	}
}
