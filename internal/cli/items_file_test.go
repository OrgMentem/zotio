// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH: Cover items file attachment-path resolution: direct attachment key,
// parent-item child-PDF resolution, and the no-attachment error.

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func runItemsFile(t *testing.T, baseURL string, asJSON bool, args ...string) (string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", baseURL+"/users/0")
	cmd := newItemsFileCmd(&rootFlags{asJSON: asJSON})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestItemsFileDirectAttachmentKey(t *testing.T) {
	const fileURL = "file:///Users/me/Zotero/storage/ABCD/paper%20draft.pdf"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/0/items/ATT1/file/view/url" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, fileURL)
			return
		}
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}))
	defer srv.Close()

	// Plain output decodes the file:// URL to a filesystem path.
	out, err := runItemsFile(t, srv.URL, false, "ATT1")
	if err != nil {
		t.Fatalf("items file: %v", err)
	}
	if got := strings.TrimSpace(out); got != "/Users/me/Zotero/storage/ABCD/paper draft.pdf" {
		t.Fatalf("path = %q, want decoded filesystem path", got)
	}

	// JSON envelope carries the raw url, decoded path, and resolved attachment key.
	out, err = runItemsFile(t, srv.URL, true, "ATT1")
	if err != nil {
		t.Fatalf("items file --json: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if env["attachment_key"] != "ATT1" {
		t.Errorf("attachment_key = %v, want ATT1", env["attachment_key"])
	}
	if env["url"] != fileURL {
		t.Errorf("url = %v, want %q", env["url"], fileURL)
	}
	if env["path"] != "/Users/me/Zotero/storage/ABCD/paper draft.pdf" {
		t.Errorf("path = %v, want decoded", env["path"])
	}
}

func TestItemsFileResolvesParentToChildPDF(t *testing.T) {
	const fileURL = "file:///data/storage/PDF9/article.pdf"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/0/items/PARENT/file/view/url":
			// Regular item has no file: force the child-resolution path.
			http.Error(w, "no file", http.StatusNotFound)
		case "/users/0/items/PARENT/children":
			_, _ = io.WriteString(w, `[{"key":"NOTE1","itemType":"note"},{"key":"PDF9","itemType":"attachment","contentType":"application/pdf"}]`)
		case "/users/0/items/PDF9/file/view/url":
			_, _ = io.WriteString(w, fileURL)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := runItemsFile(t, srv.URL, true, "PARENT")
	if err != nil {
		t.Fatalf("items file PARENT: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if env["attachment_key"] != "PDF9" {
		t.Errorf("attachment_key = %v, want PDF9 (resolved child)", env["attachment_key"])
	}
	if env["path"] != "/data/storage/PDF9/article.pdf" {
		t.Errorf("path = %v, want %q", env["path"], "/data/storage/PDF9/article.pdf")
	}
}

func TestItemsFileNoAttachmentErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/0/items/BARE/file/view/url":
			http.Error(w, "no file", http.StatusNotFound)
		case "/users/0/items/BARE/children":
			_, _ = io.WriteString(w, `[]`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	if _, err := runItemsFile(t, srv.URL, false, "BARE"); err == nil {
		t.Fatal("expected error when item has no attachment file")
	}
}
