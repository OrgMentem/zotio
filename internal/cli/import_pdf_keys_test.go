// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// import pdf must return the keys it created, and never a stranger's.

package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// importPDFTestFloor is the wall-clock floor an op captures before the write.
func importPDFTestFloor() time.Time { return time.Now().UTC().Add(-importPDFClockSkew) }

// justAdded renders a dateAdded that postdates the floor.
func justAdded() string { return time.Now().UTC().Format(time.RFC3339) }

// longAgo renders a dateAdded from well before this import.
func longAgo() string { return time.Now().UTC().AddDate(0, -6, 0).Format(time.RFC3339) }

// The connector reports only title and itemType, so the keys an agent needs to
// file the import are resolved from the library. A unique match must yield the
// parent key, its PDF child, and the DOI in one payload.
func TestResolveImportPDFKeysRecognized(t *testing.T) {
	var childrenFor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/0/items/top":
			if got := r.URL.Query().Get("sort"); got != "dateAdded" {
				t.Errorf("sort = %q, want dateAdded", got)
			}
			if got := r.URL.Query().Get("direction"); got != "desc" {
				t.Errorf("direction = %q, want desc", got)
			}
			_, _ = fmt.Fprintf(w, `[
				{"key":"NEWPARENT","data":{"itemType":"journalArticle","title":"Attention Is All You Need","DOI":"10.48550/arXiv.1706.03762","dateAdded":%q}},
				{"key":"OTHER","data":{"itemType":"journalArticle","title":"Something Else","dateAdded":%q}}
			]`, justAdded(), justAdded())
		case "/users/0/items/NEWPARENT/children":
			childrenFor = "NEWPARENT"
			_, _ = w.Write([]byte(`[
				{"key":"NOTE1","data":{"itemType":"note"}},
				{"key":"PDFCHILD","data":{"itemType":"attachment","contentType":"application/pdf"}}
			]`))
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	flags := &rootFlags{configPath: testConfigFile(t, srv.URL+"/users/0")}
	result := importPDFResult{
		Status:   "recognized",
		Title:    "Attention Is All You Need",
		ItemType: "journalArticle",
	}
	resolveImportPDFKeys(flags, &result, "paper.pdf", importPDFTestFloor())

	if result.KeysNote != "" {
		t.Fatalf("keys_note = %q, want keys resolved cleanly", result.KeysNote)
	}
	if result.ItemKey != "NEWPARENT" {
		t.Errorf("item_key = %q, want NEWPARENT", result.ItemKey)
	}
	if result.AttachmentKey != "PDFCHILD" {
		t.Errorf("attachment_key = %q, want PDFCHILD", result.AttachmentKey)
	}
	if result.DOI != "10.48550/arXiv.1706.03762" {
		t.Errorf("doi = %q, want the parent's DOI", result.DOI)
	}
	if childrenFor != "NEWPARENT" {
		t.Errorf("children fetched for %q, want NEWPARENT", childrenFor)
	}
}

// An unrecognized PDF stays a top-level standalone attachment: the attachment
// key is the only key there is, and it must still come back.
func TestResolveImportPDFKeysUnrecognizedStandalone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/0/items/top" {
			t.Errorf("unexpected request for %q; a standalone needs no children lookup", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprintf(w, `[{"key":"STANDALONE","data":{"itemType":"attachment","title":"scan.pdf","dateAdded":%q}}]`, justAdded())
	}))
	defer srv.Close()

	flags := &rootFlags{configPath: testConfigFile(t, srv.URL+"/users/0")}
	result := importPDFResult{Status: "unrecognized"}
	resolveImportPDFKeys(flags, &result, "scan.pdf", importPDFTestFloor())

	if result.KeysNote != "" {
		t.Fatalf("keys_note = %q, want the standalone resolved", result.KeysNote)
	}
	if result.AttachmentKey != "STANDALONE" {
		t.Errorf("attachment_key = %q, want STANDALONE", result.AttachmentKey)
	}
	if result.ItemKey != "" {
		t.Errorf("item_key = %q, want empty: no parent item was created", result.ItemKey)
	}
}

// Reporting a guessed key would make the caller file the wrong item, so an
// ambiguous title must report no key at all and say why.
func TestResolveImportPDFKeysAmbiguousReportsNoKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/0/items/top" {
			t.Errorf("unexpected request for %q; an ambiguous match must not be followed", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprintf(w, `[
			{"key":"DUPA","data":{"itemType":"journalArticle","title":"Twice Imported","dateAdded":%q}},
			{"key":"DUPB","data":{"itemType":"journalArticle","title":"Twice Imported","dateAdded":%q}}
		]`, justAdded(), justAdded())
	}))
	defer srv.Close()

	flags := &rootFlags{configPath: testConfigFile(t, srv.URL+"/users/0")}
	result := importPDFResult{Status: "recognized", Title: "Twice Imported", ItemType: "journalArticle"}
	resolveImportPDFKeys(flags, &result, "dup.pdf", importPDFTestFloor())

	if result.ItemKey != "" || result.AttachmentKey != "" {
		t.Fatalf("resolved keys %q/%q from an ambiguous match", result.ItemKey, result.AttachmentKey)
	}
	if result.KeysNote == "" {
		t.Fatal("ambiguous match left keys_note empty; the caller cannot tell why keys are missing")
	}
}

// The decisive case: Zotero's connector saves into whatever library the desktop
// pane targets, which need not be the library zotio reads. When the new item is
// therefore absent, an OLDER item sharing the recognized title must never be
// reported as the import's key.
func TestResolveImportPDFKeysIgnoresOlderTitleMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/0/items/top" {
			t.Errorf("followed a stale match to %q", r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		// Same title and type, but added six months ago: a different item.
		_, _ = fmt.Fprintf(w, `[{"key":"STRANGER","data":{"itemType":"journalArticle","title":"Trust in leadership","DOI":"10.1/old","dateAdded":%q}}]`, longAgo())
	}))
	defer srv.Close()

	flags := &rootFlags{configPath: testConfigFile(t, srv.URL+"/users/0")}
	result := importPDFResult{Status: "recognized", Title: "Trust in leadership", ItemType: "journalArticle"}
	resolveImportPDFKeys(flags, &result, "trust.pdf", importPDFTestFloor())

	if result.ItemKey != "" {
		t.Fatalf("item_key = %q: reported an item this import did not create", result.ItemKey)
	}
	if result.DOI != "" {
		t.Errorf("doi = %q: leaked a stranger's DOI", result.DOI)
	}
	if result.KeysNote == "" {
		t.Fatal("keys_note empty; the caller cannot tell the keys were withheld")
	}
}

// An item with no usable dateAdded cannot be proven to belong to this import.
func TestResolveImportPDFKeysRejectsMissingDateAdded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"key":"NODATE","data":{"itemType":"journalArticle","title":"No Date"}}]`))
	}))
	defer srv.Close()

	flags := &rootFlags{configPath: testConfigFile(t, srv.URL+"/users/0")}
	result := importPDFResult{Status: "recognized", Title: "No Date", ItemType: "journalArticle"}
	resolveImportPDFKeys(flags, &result, "nodate.pdf", importPDFTestFloor())

	if result.ItemKey != "" {
		t.Fatalf("item_key = %q from an item with no dateAdded", result.ItemKey)
	}
}

// A library read failure must not turn an applied import into a failure: the
// PDF is already in Zotero.
func TestResolveImportPDFKeysReadFailureIsNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no such library"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	flags := &rootFlags{configPath: testConfigFile(t, srv.URL+"/users/0")}
	result := importPDFResult{Status: "recognized", Title: "Any Paper", ItemType: "journalArticle"}
	resolveImportPDFKeys(flags, &result, "any.pdf", importPDFTestFloor())

	if result.KeysNote == "" {
		t.Fatal("read failure left keys_note empty")
	}
	if result.Status != "recognized" {
		t.Fatalf("status = %q, want the import status left untouched", result.Status)
	}
}
