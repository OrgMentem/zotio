// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH: connector client request-contract tests.

package connector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConnectorSaveItemsRequest(t *testing.T) {
	t.Parallel()

	seen := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = true
		if r.URL.Path != "/connector/saveItems" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		if got := r.Header.Get("X-Zotero-Connector-API-Version"); got != "3" {
			t.Fatalf("api version = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("content-type = %q", got)
		}
		var body struct {
			SessionID string           `json:"sessionID"`
			URI       string           `json:"uri"`
			Items     []map[string]any `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.SessionID != "session" || body.URI != "https://doi.org/10.test/foo" {
			t.Fatalf("unexpected body metadata: %+v", body)
		}
		if len(body.Items) != 1 || body.Items[0]["id"] != "connector-key" {
			t.Fatalf("items missing connector id: %+v", body.Items)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	c := New(server.URL+"/connector", time.Second)
	if err := c.SaveItems(context.Background(), "session", "https://doi.org/10.test/foo", []map[string]any{{"id": "connector-key", "itemType": "journalArticle"}}); err != nil {
		t.Fatalf("SaveItems returned error: %v", err)
	}
	if !seen {
		t.Fatal("server did not receive request")
	}
}

func TestConnectorSaveAttachmentRequest(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connector/saveAttachment" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("X-Zotero-Connector-API-Version"); got != "3" {
			t.Fatalf("api version = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/pdf" {
			t.Fatalf("content-type = %q", got)
		}
		var meta struct {
			SessionID    string `json:"sessionID"`
			ParentItemID string `json:"parentItemID"`
			Title        string `json:"title"`
			URL          string `json:"url"`
		}
		if strings.Contains(r.Header.Get("X-Metadata"), "é") {
			t.Fatalf("metadata header contains non-ASCII: %q", r.Header.Get("X-Metadata"))
		}
		if err := json.Unmarshal([]byte(r.Header.Get("X-Metadata")), &meta); err != nil {
			t.Fatalf("metadata JSON: %v", err)
		}
		if meta.SessionID != "session" || meta.ParentItemID != "connector-key" || meta.Title != "Café PDF" || meta.URL != "https://example.test/source.pdf" {
			t.Fatalf("metadata = %+v", meta)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if string(data) != "%PDF-1.4" {
			t.Fatalf("raw body = %q", string(data))
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	c := New(server.URL+"/connector", time.Second)
	if err := c.SaveAttachment(context.Background(), "session", "connector-key", "Café PDF", "https://example.test/source.pdf", "application/pdf", []byte("%PDF-1.4")); err != nil {
		t.Fatalf("SaveAttachment returned error: %v", err)
	}
}

func TestConnectorSaveStandaloneAttachmentRequest(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connector/saveStandaloneAttachment" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("X-Zotero-Connector-API-Version"); got != "3" {
			t.Fatalf("api version = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/pdf" {
			t.Fatalf("content-type = %q", got)
		}
		var meta struct {
			SessionID string `json:"sessionID"`
			Title     string `json:"title"`
			URL       string `json:"url"`
		}
		if strings.Contains(r.Header.Get("X-Metadata"), "é") {
			t.Fatalf("metadata header contains non-ASCII: %q", r.Header.Get("X-Metadata"))
		}
		if err := json.Unmarshal([]byte(r.Header.Get("X-Metadata")), &meta); err != nil {
			t.Fatalf("metadata JSON: %v", err)
		}
		if meta.SessionID != "session" || meta.Title != "Café.pdf" || meta.URL != "https://example.test/paper.pdf" {
			t.Fatalf("metadata = %+v", meta)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if string(data) != "%PDF-1.4" {
			t.Fatalf("raw body = %q", string(data))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"canRecognize":true}`))
	}))
	defer server.Close()

	c := New(server.URL+"/connector", time.Second)
	canRecognize, err := c.SaveStandaloneAttachment(context.Background(), "session", "Café.pdf", "https://example.test/paper.pdf", "application/pdf", []byte("%PDF-1.4"))
	if err != nil {
		t.Fatalf("SaveStandaloneAttachment returned error: %v", err)
	}
	if !canRecognize {
		t.Fatal("SaveStandaloneAttachment did not report canRecognize")
	}
}

func TestConnectorGetRecognizedItem(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connector/getRecognizedItem" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var body struct {
			SessionID string `json:"sessionID"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.SessionID != "session" {
			t.Fatalf("sessionID = %q", body.SessionID)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"title":"Attention Is All You Need","itemType":"preprint"}`))
	}))
	defer server.Close()

	c := New(server.URL+"/connector", time.Second)
	item, recognized, err := c.GetRecognizedItem(context.Background(), "session")
	if err != nil {
		t.Fatalf("GetRecognizedItem returned error: %v", err)
	}
	if !recognized || item.Title != "Attention Is All You Need" || item.ItemType != "preprint" {
		t.Fatalf("recognized item = %+v recognized=%v", item, recognized)
	}
}

func TestConnectorGetRecognizedItemNoContent(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := New(server.URL+"/connector", time.Second)
	item, recognized, err := c.GetRecognizedItem(context.Background(), "session")
	if err != nil {
		t.Fatalf("GetRecognizedItem returned error: %v", err)
	}
	if recognized || item != (RecognizedItem{}) {
		t.Fatalf("recognized item = %+v recognized=%v", item, recognized)
	}
}

func TestConnectorAttachmentResolvers(t *testing.T) {
	t.Parallel()

	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var body struct {
			SessionID string `json:"sessionID"`
			ItemID    string `json:"itemID"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.SessionID != "session" || body.ItemID != "connector-key" {
			t.Fatalf("body = %+v", body)
		}
		switch r.URL.Path {
		case "/connector/hasAttachmentResolvers":
			_, _ = w.Write([]byte("true"))
		case "/connector/saveAttachmentFromResolver":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("Full Text"))
		default:
			t.Fatalf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()

	c := New(server.URL+"/connector", time.Second)
	ok, err := c.HasAttachmentResolvers(context.Background(), "session", "connector-key")
	if err != nil {
		t.Fatalf("HasAttachmentResolvers returned error: %v", err)
	}
	if !ok {
		t.Fatal("HasAttachmentResolvers returned false")
	}
	title, err := c.SaveAttachmentFromResolver(context.Background(), "session", "connector-key")
	if err != nil {
		t.Fatalf("SaveAttachmentFromResolver returned error: %v", err)
	}
	if title != "Full Text" {
		t.Fatalf("title = %q", title)
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %v", paths)
	}
}

func TestConnectorSelectedCollectionAndUpdateSession(t *testing.T) {
	t.Parallel()

	var sawUpdate bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connector/getSelectedCollection":
			_, _ = w.Write([]byte(`{"libraryID":1,"libraryName":"My Library","editable":true,"filesEditable":true,"id":null,"name":"My Library","targets":[{"id":"L1","name":"My Library","level":0,"filesEditable":true},{"id":"C78","name":"Tech","level":1,"filesEditable":true}]}`))
		case "/connector/updateSession":
			sawUpdate = true
			var body struct {
				SessionID string   `json:"sessionID"`
				Target    string   `json:"target"`
				Tags      []string `json:"tags"`
				Note      string   `json:"note"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode update body: %v", err)
			}
			if body.SessionID != "session" || body.Target != "C78" || len(body.Tags) != 1 || body.Tags[0] != "tag" || body.Note != "note" {
				t.Fatalf("update body = %+v", body)
			}
		default:
			t.Fatalf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()

	c := New(server.URL+"/connector", time.Second)
	selected, err := c.SelectedCollection(context.Background())
	if err != nil {
		t.Fatalf("SelectedCollection returned error: %v", err)
	}
	if selected.LibraryName != "My Library" || len(selected.Targets) != 2 || selected.Targets[1].ID != "C78" {
		t.Fatalf("selected = %+v", selected)
	}
	if err := c.UpdateSession(context.Background(), "session", "C78", []string{"tag"}, "note"); err != nil {
		t.Fatalf("UpdateSession returned error: %v", err)
	}
	if !sawUpdate {
		t.Fatal("updateSession was not called")
	}
}

func TestConnectorImportRequest(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connector/import" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("session"); got != "session 1" {
			t.Fatalf("session = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "text/plain" {
			t.Fatalf("content-type = %q", got)
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if string(data) != "TY  - JOUR" {
			t.Fatalf("body = %q", string(data))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`[{"key":"ABC123","data":{"title":"Imported"}}]`))
	}))
	defer server.Close()

	c := New(server.URL+"/connector", time.Second)
	items, err := c.Import(context.Background(), "session 1", []byte("TY  - JOUR"), "text/plain")
	if err != nil {
		t.Fatalf("Import returned error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
}

func TestConnectorTranslators(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connector/getTranslators":
			_, _ = w.Write([]byte(`[{"label":"arXiv.org","translatorID":"abc","priority":100}]`))
		case "/connector/detect":
			var body struct {
				URI  string `json:"uri"`
				HTML string `json:"html"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode detect body: %v", err)
			}
			if body.URI != "https://arxiv.org/abs/1706.03762" || !strings.Contains(body.HTML, "Attention") {
				t.Fatalf("detect body = %+v", body)
			}
			_, _ = w.Write([]byte(`[{"label":"arXiv.org","translatorID":"abc","priority":100}]`))
		default:
			t.Fatalf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()

	c := New(server.URL+"/connector", time.Second)
	all, err := c.GetTranslators(context.Background())
	if err != nil {
		t.Fatalf("GetTranslators returned error: %v", err)
	}
	if len(all) != 1 || all[0].Label != "arXiv.org" {
		t.Fatalf("translators = %+v", all)
	}
	matches, err := c.DetectTranslators(context.Background(), "https://arxiv.org/abs/1706.03762", "<title>Attention</title>")
	if err != nil {
		t.Fatalf("DetectTranslators returned error: %v", err)
	}
	if len(matches) != 1 || matches[0].TranslatorID != "abc" {
		t.Fatalf("matches = %+v", matches)
	}
}

func TestConnectorNonCreatedErrors(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	defer server.Close()

	c := New(server.URL+"/connector", time.Second)
	if err := c.SaveItems(context.Background(), "session", "", []map[string]any{{"id": "id"}}); err == nil {
		t.Fatal("SaveItems returned nil error for HTTP 400")
	}
	if err := c.SaveAttachment(context.Background(), "session", "id", "PDF", "", "application/pdf", nil); err == nil {
		t.Fatal("SaveAttachment returned nil error for HTTP 400")
	}
}
