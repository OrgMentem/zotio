// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Directory resolve emits a reviewable create manifest with CrossRef item data.
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

// TestImportResolveUnidentifiedEntryExplainsItself covers the papio field
// report of 2026-08-22 round 2, finding 2: a staged PDF with no extractable
// identifier produced an entry with no identifier, no item and an EMPTY note,
// which reads as a missing registry record rather than a failed extraction.
// The note branch that existed was unreachable - it sat under
// action == "create", which is only assigned once a DOI has been found.
func TestImportResolveUnidentifiedEntryExplainsItself(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "scan001.pdf"), []byte("no identifier here"), 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
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
	if len(got.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(got.Entries))
	}
	entry := got.Entries[0]
	if entry.Classification != "unidentified" || entry.Action != "recognize" || entry.Status != "unresolved" {
		t.Fatalf("classification/action/status = %q/%q/%q", entry.Classification, entry.Action, entry.Status)
	}
	if entry.Note == "" {
		t.Fatal("note is empty: an unresolved entry must say why it is unresolved")
	}
	// The note must name the extraction failure, not imply a missing record.
	if !strings.Contains(entry.Note, "filename") || !strings.Contains(entry.Note, "recognizer") {
		t.Errorf("note = %q, want it to name the failed extraction and the next step", entry.Note)
	}
}

// Manifest resolve refreshes unresolved DOI create entries in-place on stdout.
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

func TestImportManifestFanoutPreservesSequentialOrderAndReportsErrors(t *testing.T) {
	const entryCount = 24
	const failingIndex = 7

	dir := t.TempDir()
	for i := range entryCount {
		name := fmt.Sprintf("10.5555%%2Forder-%02d.pdf", i)
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatalf("write fixture %d: %v", i, err)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doi := strings.TrimPrefix(r.URL.Path, "/works/")
		var index int
		if _, err := fmt.Sscanf(doi, "10.5555/order-%02d", &index); err != nil {
			http.Error(w, "unexpected DOI "+doi, http.StatusBadRequest)
			return
		}
		// Reverse delays force completion order away from source order.
		time.Sleep(time.Duration(entryCount-index) * time.Millisecond)
		if index == failingIndex {
			http.Error(w, "mock registry failure", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]any{
				"type":  "journal-article",
				"title": []string{fmt.Sprintf("Title %02d", index)},
				"DOI":   doi,
			},
		})
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichCrossRefBase, srv.URL)
	t.Setenv("HOME", t.TempDir())

	paths, err := listPDFs(dir, 0)
	if err != nil {
		t.Fatalf("list fixtures: %v", err)
	}
	idx := libraryDOIIndex{byDOI: map[string]libItem{}}
	httpClient := &http.Client{Timeout: 5 * time.Second}
	want := importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           dir,
		Entries:       make([]importManifestEntry, 0, len(paths)),
	}
	for _, path := range paths {
		result, err := resolveImportManifestEntry(context.Background(), path, idx, httpClient)
		if err != nil {
			t.Fatalf("sequential reference %q: %v", path, err)
		}
		want.Entries = append(want.Entries, result.Entry)
	}

	flags := &rootFlags{timeout: 5 * time.Second}
	cmd := newImportResolveCmd(flags)
	cmd.SetContext(context.Background())
	got, gotErr := buildImportManifestFromDir(cmd, flags, dir, 0)
	if gotErr == nil {
		t.Fatal("fanout error was dropped")
	}
	failingPath := filepath.Join(dir, fmt.Sprintf("10.5555%%2Forder-%02d.pdf", failingIndex))
	if !strings.Contains(gotErr.Error(), failingPath) || !strings.Contains(gotErr.Error(), "HTTP 500") {
		t.Fatalf("fanout error = %q, want source path and provider failure", gotErr)
	}

	var wantJSON, gotJSON bytes.Buffer
	if err := writeImportManifest(&wantJSON, want); err != nil {
		t.Fatalf("encode sequential reference: %v", err)
	}
	if err := writeImportManifest(&gotJSON, got); err != nil {
		t.Fatalf("encode fanout result: %v", err)
	}
	if !bytes.Equal(gotJSON.Bytes(), wantJSON.Bytes()) {
		t.Fatalf("fanout manifest order differs from sequential reference\nwant:\n%s\ngot:\n%s", wantJSON.Bytes(), gotJSON.Bytes())
	}
	if got.Entries[failingIndex].Status != "unresolved" || !strings.Contains(got.Entries[failingIndex].Note, "HTTP 500") {
		t.Fatalf("failed entry = %#v, want unresolved status with the provider error", got.Entries[failingIndex])
	}
}

func TestProviderErrorBodyIsBoundedAndCannotReachTerminalUnsanitized(t *testing.T) {
	body := "\x1b]0;owned\x07" +
		strings.Repeat("x", providerErrorBodyLimit+1024) +
		"TAIL_SECRET"
	transport := externalHTTPRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return externalHTTPTestResponse(req, http.StatusBadGateway, "", body), nil
	})

	var decoded map[string]any
	err := getCappedProviderJSON(
		context.Background(),
		&http.Client{Transport: transport},
		providerCrossRef,
		"https://8.8.8.8/provider",
		nil,
		&decoded,
	)
	if err == nil {
		t.Fatal("provider failure returned no error")
	}
	if len(err.Error()) > providerErrorBodyLimit+64 {
		t.Fatalf("provider error length = %d, want at most %d: %q", len(err.Error()), providerErrorBodyLimit+64, err)
	}
	if strings.Contains(err.Error(), "TAIL_SECRET") || !strings.Contains(err.Error(), "(truncated)") {
		t.Fatalf("provider error was not truncated before rendering: %q", err)
	}

	var stderr bytes.Buffer
	fmt.Fprintf(&stderr, "Error: %s\n", SanitizeForTerminal(err.Error()))
	if strings.ContainsAny(stderr.String(), "\x1b\x07") {
		t.Fatalf("terminal error contains an ANSI or OSC control byte: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "HTTP 502") {
		t.Fatalf("terminal error lost the useful HTTP status: %q", stderr.String())
	}
}

func TestImportResolveCancellationWritesNoPartialManifest(t *testing.T) {
	dir := t.TempDir()
	for i := range 40 {
		name := fmt.Sprintf("10.5555%%2Fcancel-%02d.pdf", i)
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatalf("write fixture %d: %v", i, err)
		}
	}

	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	withBase(t, &enrichCrossRefBase, srv.URL)
	t.Setenv("HOME", t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	flags := &rootFlags{timeout: 30 * time.Second}
	cmd := newImportResolveCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{dir})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})

	done := make(chan error, 1)
	go func() {
		done <- cmd.Execute()
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("import resolve did not start a provider request")
	}

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("import resolve error = %v, want context cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("import resolve did not stop after cancellation")
	}
	if out.Len() != 0 {
		t.Fatalf("cancelled import resolve wrote a partial manifest:\n%s", out.String())
	}
}
