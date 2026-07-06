// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PATCH(glean roadmap-phase4 7e799ea9): directory resolve emits a reviewable create manifest with CrossRef item data.
func TestImportResolveDirectoryBuildsManifest(t *testing.T) {
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "10.1234%2Fdemo.pdf")
	if err := os.WriteFile(pdfPath, nil, 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}

	srv := importResolveCrossRefWorkServer(t, "Resolved DOI Title", "10.1234/demo")
	withBase(t, &enrichCrossRefBase, srv.URL)
	t.Setenv("HOME", t.TempDir())

	flags := &rootFlags{timeout: 5 * time.Second}
	cmd := newImportResolveCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{dir})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import resolve dir: %v", err)
	}

	var got importManifest
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode manifest %q: %v", out.String(), err)
	}
	if got.SchemaVersion != importManifestSchemaVersion {
		t.Fatalf("schema_version = %d, want %d", got.SchemaVersion, importManifestSchemaVersion)
	}
	if got.Dir != dir {
		t.Fatalf("dir = %q, want %q", got.Dir, dir)
	}
	if len(got.Entries) != 1 {
		t.Fatalf("entries = %d, want 1: %#v", len(got.Entries), got.Entries)
	}
	entry := got.Entries[0]
	abs, err := filepath.Abs(pdfPath)
	if err != nil {
		t.Fatalf("abs pdf: %v", err)
	}
	if entry.Path != abs {
		t.Fatalf("path = %q, want %q", entry.Path, abs)
	}
	if entry.Classification != "new" || entry.Action != "create" || entry.Status != "resolved" {
		t.Fatalf("entry classification/action/status = %q/%q/%q", entry.Classification, entry.Action, entry.Status)
	}
	if entry.IdentifierType != "doi" || entry.Identifier != "10.1234/demo" {
		t.Fatalf("entry identifier = %q:%q", entry.IdentifierType, entry.Identifier)
	}
	if entry.Item == nil || entry.Item["title"] != "Resolved DOI Title" {
		t.Fatalf("entry item = %#v, want title", entry.Item)
	}
}

// PATCH(glean roadmap-phase4 7e799ea9): manifest resolve refreshes unresolved DOI create entries in-place on stdout.
func TestImportResolveManifestRefreshesUnresolvedCreate(t *testing.T) {
	srv := importResolveCrossRefWorkServer(t, "Refreshed DOI Title", "10.1234/demo")
	withBase(t, &enrichCrossRefBase, srv.URL)

	manifestPath := filepath.Join(t.TempDir(), "manifest.json")
	var manifest bytes.Buffer
	if err := writeImportManifest(&manifest, importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Entries: []importManifestEntry{{
			Path:           "/tmp/paper.pdf",
			Classification: "new",
			Action:         "create",
			IdentifierType: "doi",
			Identifier:     "10.1234/demo",
			Status:         "unresolved",
			Note:           "previous failure",
		}},
	}); err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath, manifest.Bytes(), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	flags := &rootFlags{timeout: 5 * time.Second}
	cmd := newImportResolveCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{manifestPath})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import resolve manifest: %v", err)
	}

	var got importManifest
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode manifest %q: %v", out.String(), err)
	}
	if len(got.Entries) != 1 {
		t.Fatalf("entries = %d, want 1: %#v", len(got.Entries), got.Entries)
	}
	entry := got.Entries[0]
	if entry.Status != "resolved" || entry.Note != "" {
		t.Fatalf("entry status/note = %q/%q", entry.Status, entry.Note)
	}
	if entry.Item == nil || entry.Item["title"] != "Refreshed DOI Title" {
		t.Fatalf("entry item = %#v, want refreshed title", entry.Item)
	}
}

func importResolveCrossRefWorkServer(t *testing.T, title, doi string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]any{
				"type":  "journal-article",
				"title": []string{title},
				"DOI":   doi,
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}
