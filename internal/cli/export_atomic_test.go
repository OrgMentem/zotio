// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exportPageBody renders n items as a JSON array, so a page carries real bytes
// that a streaming export will have already written before a later page fails.
func exportPageBody(prefix string, n int) string {
	parts := make([]string, 0, n)
	for i := range n {
		parts = append(parts, fmt.Sprintf(`{"key":"%s%03d","version":1,"data":{"title":"t%d"}}`, prefix, i, i))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// failAfterFirstPageServer answers the first page and then fails, which is the
// shape that used to destroy a previously good export: the target was truncated
// before any fetch, so the failure left an empty or partial file behind.
func failAfterFirstPageServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "start=0") || !strings.Contains(r.URL.RawQuery, "start=") {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Total-Results", "250")
			_, _ = w.Write([]byte(exportPageBody("K", 100)))
			return
		}
		http.Error(w, "upstream rejected the request", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func useTestServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
}

// Measured before this change: a failed `export` truncated an 11,299,615-byte
// artifact to 0. The failure must now leave the previous export untouched.
func TestExportFailedRunPreservesPreviousArtifact(t *testing.T) {
	useTestServer(t, failAfterFirstPageServer(t))

	target := filepath.Join(t.TempDir(), "items.jsonl")
	const previous = "the previously good export\n"
	if err := os.WriteFile(target, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newExportCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"items", "--output", target})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("export succeeded despite an upstream failure on page two")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if string(got) != previous {
		t.Fatalf("previous export was damaged by a failed run: %q", got)
	}
	if n := countLeftoverTemps(t, filepath.Dir(target)); n != 0 {
		t.Fatalf("%d temporary files left behind", n)
	}
}

// A failed export must not create the target at all when there was none, rather
// than leaving an empty file that looks like an empty library.
func TestExportFailedRunCreatesNoArtifact(t *testing.T) {
	useTestServer(t, failAfterFirstPageServer(t))

	dir := t.TempDir()
	target := filepath.Join(dir, "items.jsonl")
	cmd := newExportCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"items", "--output", target})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("export succeeded despite an upstream failure")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("failed export left a file behind (err=%v)", err)
	}
	if n := countLeftoverTemps(t, dir); n != 0 {
		t.Fatalf("%d temporary files left behind", n)
	}
}

// Stdout is a stream: a consumer has already been handed every byte generated
// before the failure, so those bytes must still be flushed. This is the
// behaviour the atomic file path deliberately does NOT copy.
func TestExportStdoutStillEmitsBytesGeneratedBeforeFailure(t *testing.T) {
	useTestServer(t, failAfterFirstPageServer(t))

	cmd := newExportCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"items"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("export succeeded despite an upstream failure")
	}
	if !strings.Contains(out.String(), `"K000"`) {
		t.Fatalf("stdout lost the records generated before the failure: %q", out.String())
	}
}

// Measured before this change: `collections export` against a missing key
// exited non-zero after leaving 1,086 complete, valid @article entries on disk
// -- syntactically perfect and silently incomplete.
func TestCollectionsExportFailedWalkPreservesPreviousArtifact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Items resolve, then the subcollection walk fails: output has already
		// been written by the time the export gives up.
		if strings.Contains(r.URL.Path, "/collections/") && strings.HasSuffix(r.URL.Path, "/collections") {
			http.Error(w, "upstream rejected the request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Total-Results", "1")
		if strings.Contains(r.URL.RawQuery, "format=keys") {
			_, _ = w.Write([]byte("KEY1"))
			return
		}
		_, _ = w.Write([]byte("@article{KEY1}"))
	}))
	t.Cleanup(srv.Close)
	useTestServer(t, srv)

	target := filepath.Join(t.TempDir(), "refs.bib")
	const previous = "@article{PREVIOUSLY_GOOD}\n"
	if err := os.WriteFile(target, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newCollectionsExportCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"ROOT", "--output", target})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("collections export succeeded despite a failed subcollection walk")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if string(got) != previous {
		t.Fatalf("previous bibliography was replaced by a partial one: %q", got)
	}
}

// os.WriteFile left an existing file's mode alone, so atomic publication must
// too: silently tightening a mode the user chose is a behaviour change.
func TestAnnotationsExportPreservesExistingFileMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	useTestServer(t, srv)

	target := filepath.Join(t.TempDir(), "annotations.json")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newAnnotationsExportCmd(&rootFlags{asJSON: true, noCache: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"--collection", "COLL1", "--format", "json", "--refresh", "--limit", "1", "--output", target})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("annotations export: %v", err)
	}
	assertFileMode(t, target, 0o644)
}

// Publishing by rename would replace a symlink with a regular file, so an
// existing symlink target is refused outright and left alone.
func TestExportRefusesSymlinkOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"K1","version":1,"data":{"title":"t"}}]`))
	}))
	t.Cleanup(srv.Close)
	useTestServer(t, srv)

	dir := t.TempDir()
	referent := filepath.Join(dir, "real.jsonl")
	if err := os.WriteFile(referent, []byte("referent contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(referent, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	cmd := newExportCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"items", "--output", link})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("export error = %v, want symlink refusal", err)
	}
	if fi, statErr := os.Lstat(link); statErr != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was replaced (err=%v)", statErr)
	}
	if got, _ := os.ReadFile(referent); string(got) != "referent contents" {
		t.Fatalf("referent was modified: %q", got)
	}
}
