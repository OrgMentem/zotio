// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// parent-item child-PDF resolution, and the no-attachment error.

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
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

func newTestFileClient(t *testing.T, srv *httptest.Server, baseURL string) *client.Client {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse srv URL: %v", err)
	}
	cfg := &config.Config{BaseURL: baseURL}
	c := client.New(cfg, time.Second, 0)
	// Route all requests to the httptest server regardless of BaseURL host/port.
	baseTransport := http.DefaultTransport
	c.HTTPClient.Transport = fileTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = u.Scheme
		clone.URL.Host = u.Host
		return baseTransport.RoundTrip(clone)
	})
	return c
}

type fileTestRoundTripFunc func(*http.Request) (*http.Response, error)

func (f fileTestRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestItemsFileDirectAttachmentKey(t *testing.T) {
	const fileURL = "file:///Users/me/Zotero/storage/ABCD/paper%20draft.pdf"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/0/items/ATT1/file/view/url" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, fileURL)
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
			http.Error(w, "no file", http.StatusNotFound)
		case "/users/0/items/PARENT/children":
			_, _ = io.WriteString(w, `[{"key":"PDF9","data":{"key":"PDF9","itemType":"attachment","contentType":"application/pdf"}}]`)
		case "/users/0/items/PDF9/file/view/url":
			_, _ = io.WriteString(w, fileURL)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := runItemsFile(t, srv.URL, true, "PARENT")
	if err != nil {
		t.Fatalf("items file --json: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if env["attachment_key"] != "PDF9" {
		t.Errorf("attachment_key = %v, want PDF9", env["attachment_key"])
	}
	if env["url"] != fileURL {
		t.Errorf("url = %v, want %q", env["url"], fileURL)
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

func TestItemsFileTransientFailureSurfacesError(t *testing.T) {
	fastRetryBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/items/PARENT/file/view/url"):
			http.Error(w, "not an attachment", http.StatusNotFound)
		case strings.HasSuffix(p, "/items/PARENT/children"):
			_, _ = io.WriteString(w, `[{"key":"PDF9","data":{"key":"PDF9","itemType":"attachment","contentType":"application/pdf"}}]`)
		case strings.HasSuffix(p, "/items/PDF9/file/view/url"):
			http.Error(w, "transient 500", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected "+p, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	clientLocal := newTestFileClient(t, srv, "http://127.0.0.1:23119/api/users/0")
	attKey, u, err := resolveAttachmentFileURL(clientLocal, "PARENT")
	if err == nil {
		t.Fatalf("resolveAttachmentFileURL transient 500: got attKey=%q url=%q err=nil, want error", attKey, u)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("transient error = %q, want to contain 500", err.Error())
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/items/PARENT2/file/view/url"):
			http.Error(w, "not found", http.StatusNotFound)
		case strings.HasSuffix(p, "/items/PARENT2/children"):
			_, _ = io.WriteString(w, `[{"key":"PDF9","data":{"key":"PDF9","itemType":"attachment","contentType":"application/pdf"}}]`)
		case strings.HasSuffix(p, "/items/PDF9/file/view/url"):
			http.Error(w, "no file", http.StatusNotFound)
		default:
			http.Error(w, "unexpected "+p, http.StatusNotFound)
		}
	}))
	defer srv2.Close()
	clientLocal2 := newTestFileClient(t, srv2, "http://127.0.0.1:23119/api/users/0")
	attKey, u, err = resolveAttachmentFileURL(clientLocal2, "PARENT2")
	if err != nil {
		t.Fatalf("resolveAttachmentFileURL 404 fallback: unexpected err %v", err)
	}
	if u != "" {
		t.Fatalf("404 fallback url = %q, want empty (no file)", u)
	}
	if attKey != "PDF9" {
		t.Fatalf("attKey = %q, want PDF9", attKey)
	}
}

func TestItemsFileNonLocalBaseDoesNotSpuriousError(t *testing.T) {
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
	c := newTestFileClient(t, srv, srv.URL+"/users/0")
	if isLocalZoteroAPI(c.BaseURL) {
		t.Fatalf("test base %q should not be local", c.BaseURL)
	}
	attKey, fileURL, err := resolveAttachmentFileURL(c, "BARE")
	if err != nil {
		t.Fatalf("non-local resolve unexpected err %v", err)
	}
	if fileURL != "" || attKey != "" {
		t.Fatalf("non-local resolve = %q %q, want empty (no spurious hard error)", attKey, fileURL)
	}
	out, err := runItemsFile(t, srv.URL, false, "BARE")
	if err == nil {
		t.Fatalf("runItemsFile non-local bare: got out=%q err=nil, want no-attachment error", out)
	}
	if !strings.Contains(err.Error(), "no attachment file found") {
		t.Fatalf("err = %q, want no-attachment message, not transport error", err.Error())
	}
}
