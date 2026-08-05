// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// newItemsNewTemplateServer serves the schema template endpoint used by items new.
func newItemsNewTemplateServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/items/new") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"itemType":"journalArticle","title":"","creators":[],"date":"","DOI":"","publicationTitle":""}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runItemsNewTestCmd exercises the registered root command with schema client env seams.
func runItemsNewTestCmd(t *testing.T, srv *httptest.Server, args ...string) (string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	var flags rootFlags
	cmd := newRootCmd(&flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// Unknown fields must fail loudly before create.
func TestItemsNewRejectsUnknownField(t *testing.T) {
	srv := newItemsNewTemplateServer(t)

	_, err := runItemsNewTestCmd(t, srv, "--no-cache", "--dry-run", "items", "new", "--item-type", "journalArticle", "--field", "title=Hello", "--field", "bogus=x")
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("items new unknown field err = %v, want bogus", err)
	}
}

// The preview must carry the merged schema-backed fields, so the user sees the
// item body they are about to create.
func TestItemsNewPreviewJSONIncludesAppliedFields(t *testing.T) {
	srv := newItemsNewTemplateServer(t)

	out, err := runItemsNewTestCmd(t, srv, "--json", "--no-cache", "--dry-run", "items", "new", "--item-type", "journalArticle", "--field", "title=Hello")
	if err != nil {
		t.Fatalf("items new --dry-run: %v", err)
	}
	env := decodeIdentifierPreview(t, []byte(out))
	if env.Mode != "preview" || env.PreviewReason != "dry_run" {
		t.Fatalf("mode=%q reason=%q, want preview/dry_run", env.Mode, env.PreviewReason)
	}
	if got := env.Item["title"]; got != "Hello" {
		t.Fatalf("title = %v, want Hello", got)
	}
	if got := env.Item["itemType"]; got != "journalArticle" {
		t.Fatalf("itemType = %v, want journalArticle", got)
	}
}
