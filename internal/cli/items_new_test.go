// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase4 items-new): coverage for schema-backed item creation.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// PATCH(glean roadmap-phase4 items-new): serve the schema template endpoint used by items new.
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

// PATCH(glean roadmap-phase4 items-new): exercise the registered root command with schema client env seams.
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

// PATCH(glean roadmap-phase4 items-new): unknown fields must fail loudly before create.
func TestItemsNewRejectsUnknownField(t *testing.T) {
	srv := newItemsNewTemplateServer(t)

	_, err := runItemsNewTestCmd(t, srv, "--no-cache", "--dry-run", "items", "new", "--item-type", "journalArticle", "--field", "title=Hello", "--field", "bogus=x")
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("items new unknown field err = %v, want bogus", err)
	}
}

// PATCH(glean roadmap-phase4 items-new): dry-run output must include merged schema-backed fields.
func TestItemsNewDryRunJSONIncludesAppliedFields(t *testing.T) {
	srv := newItemsNewTemplateServer(t)

	out, err := runItemsNewTestCmd(t, srv, "--json", "--no-cache", "--dry-run", "items", "new", "--item-type", "journalArticle", "--field", "title=Hello")
	if err != nil {
		t.Fatalf("items new --dry-run: %v", err)
	}
	var envelope struct {
		DryRun bool           `json:"dry_run"`
		Item   map[string]any `json:"item"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("decode dry-run output %q: %v", out, err)
	}
	if !envelope.DryRun {
		t.Fatalf("dry_run = false, want true")
	}
	if got := envelope.Item["title"]; got != "Hello" {
		t.Fatalf("title = %v, want Hello", got)
	}
	if got := envelope.Item["itemType"]; got != "journalArticle" {
		t.Fatalf("itemType = %v, want journalArticle", got)
	}
}
