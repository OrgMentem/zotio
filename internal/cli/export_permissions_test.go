// Copyright 2026 OrgMentem. Licensed under MIT.

package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestExportOutputFileIsPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"K1"}]`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	out := filepath.Join(t.TempDir(), "items.jsonl")
	if err := os.WriteFile(out, []byte("old export"), 0o644); err != nil {
		t.Fatalf("seed export: %v", err)
	}
	if err := os.Chmod(out, 0o644); err != nil {
		t.Fatalf("set export mode: %v", err)
	}

	cmd := newExportCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"items", "--output", out})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export items: %v", err)
	}
	assertFileMode(t, out, 0o600)
}

func TestCollectionsExportOutputFileIsPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	out := filepath.Join(t.TempDir(), "references.bib")
	if err := os.WriteFile(out, []byte("old export"), 0o644); err != nil {
		t.Fatalf("seed collection export: %v", err)
	}
	if err := os.Chmod(out, 0o644); err != nil {
		t.Fatalf("set collection export mode: %v", err)
	}

	cmd := newCollectionsExportCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"COLL1", "--output", out})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("collections export: %v", err)
	}
	assertFileMode(t, out, 0o600)
}