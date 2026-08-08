// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestSchemaNewItemTemplateBuildsFromLocalSchemaEndpoints(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:23119")
	if err != nil {
		t.Skipf("local API test port unavailable: %v", err)
	}
	var newTemplateHits, preflightHits int
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/0" || r.URL.Path == "/users/0/" {
			preflightHits++
			http.NotFound(w, r)
			return
		}
		// command regressed to the old Web-only implementation.
		if r.URL.Path == "/items/new" {
			newTemplateHits++
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/users/0/itemTypeFields" || r.URL.Path == "/users/0/itemTypeCreatorTypes" || r.URL.Path == "/users/0/creatorFields" {
			http.Error(w, "schema endpoints must be global", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/itemTypeFields":
			if got := r.URL.Query().Get("itemType"); got == "journalArticle" {
				_, _ = w.Write([]byte(`[
					{"field":"title"},{"field":"abstractNote"},{"field":"publicationTitle"},{"field":"publisher"},{"field":"place"},{"field":"date"},{"field":"volume"},{"field":"issue"},{"field":"section"},{"field":"partNumber"},{"field":"partTitle"},{"field":"pages"},{"field":"series"},{"field":"seriesTitle"},{"field":"seriesText"},{"field":"journalAbbreviation"},{"field":"DOI"},{"field":"citationKey"},{"field":"url"},{"field":"accessDate"},{"field":"PMID"},{"field":"PMCID"},{"field":"ISSN"},{"field":"archive"},{"field":"archiveLocation"},{"field":"shortTitle"},{"field":"language"},{"field":"libraryCatalog"},{"field":"callNumber"},{"field":"rights"},{"field":"extra"}
				]`))
			} else {
				_, _ = w.Write([]byte(`[{"field":"note"}]`))
			}
		case "/itemTypeCreatorTypes":
			if r.URL.Query().Get("itemType") == "journalArticle" {
				_, _ = w.Write([]byte(`[{"creatorType":"author"},{"creatorType":"editor"}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case "/creatorFields":
			_, _ = w.Write([]byte(`[{"field":"firstName"},{"field":"lastName"},{"field":"name"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	srv.Listener = listener
	srv.Start()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_API_KEY", "")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	run := func(itemType string) map[string]any {
		t.Helper()
		var out bytes.Buffer
		flags := rootFlags{}
		cmd := newRootCmd(&flags)
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetArgs([]string{"--json", "--no-cache", "schema", "new-item-template", "--item-type", itemType})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("schema new-item-template %s: %v", itemType, err)
		}
		var envelope struct {
			Results []map[string]any `json:"results"`
		}
		if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
			t.Fatalf("decode %s envelope %q: %v", itemType, out.String(), err)
		}
		if len(envelope.Results) != 1 {
			t.Fatalf("results for %s = %d, want one template", itemType, len(envelope.Results))
		}
		return envelope.Results[0]
	}

	journal := run("journalArticle")
	wantJournalFields := []string{"itemType", "title", "creators", "abstractNote", "publicationTitle", "publisher", "place", "date", "volume", "issue", "section", "partNumber", "partTitle", "pages", "series", "seriesTitle", "seriesText", "journalAbbreviation", "DOI", "citationKey", "url", "accessDate", "PMID", "PMCID", "ISSN", "archive", "archiveLocation", "shortTitle", "language", "libraryCatalog", "callNumber", "rights", "extra", "tags", "collections", "relations"}
	if len(journal) != len(wantJournalFields) {
		t.Fatalf("journal template keys = %d, want %d (%v)", len(journal), len(wantJournalFields), journal)
	}
	for _, field := range wantJournalFields {
		if _, ok := journal[field]; !ok {
			t.Errorf("journal template missing field %q", field)
		}
	}
	for _, field := range wantJournalFields {
		if field == "itemType" || field == "creators" || field == "tags" || field == "collections" || field == "relations" {
			continue
		}
		if value := journal[field]; value != "" {
			t.Errorf("journal template field %s = %#v, want empty string", field, value)
		}
	}
	if journal["itemType"] != "journalArticle" {
		t.Errorf("journal itemType = %v, want journalArticle", journal["itemType"])
	}
	creators, ok := journal["creators"].([]any)
	if !ok || len(creators) != 1 {
		t.Fatalf("journal creators = %#v, want one blank creator", journal["creators"])
	}
	creator := creators[0].(map[string]any)
	if creator["creatorType"] != "author" {
		t.Errorf("journal creatorType = %v, want author (primary local schema type)", creator["creatorType"])
	}
	for _, field := range []string{"firstName", "lastName"} {
		if value, ok := creator[field]; !ok || value != "" {
			t.Errorf("journal creator %s = %#v, want empty string", field, value)
		}
	}

	note := run("note")
	wantNoteFields := []string{"itemType", "note", "tags", "collections", "relations"}
	if len(note) != len(wantNoteFields) {
		t.Fatalf("note template keys = %d, want %d (%v)", len(note), len(wantNoteFields), note)
	}
	for _, field := range wantNoteFields {
		if _, ok := note[field]; !ok {
			t.Errorf("note template missing field %q", field)
		}
	}
	if note["itemType"] != "note" || note["note"] != "" {
		t.Errorf("note template identity/content = %#v, want note and empty note", note)
	}
	if _, ok := note["creators"]; ok {
		t.Error("note template has creators, want no creator array")
	}
	if newTemplateHits != 0 {
		t.Fatalf("/items/new was requested %d times; local schema construction must not use it", newTemplateHits)
	}
	if preflightHits == 0 {
		t.Fatal("local API capability preflight did not probe the configured local plane")
	}
}
