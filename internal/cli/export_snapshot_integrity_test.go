// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// snapshotServer serves five records in pages. Per request it consults pageFn,
// which may replace the body or report a library version.
type snapshotServer struct {
	mu      sync.Mutex
	version string
	body    func(start, limit int) (any, bool)
	starts  []int
}

func newSnapshotServer(t *testing.T, s *snapshotServer) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		s.mu.Lock()
		s.starts = append(s.starts, start)
		version := s.version
		override, replaced := (any)(nil), false
		if s.body != nil {
			override, replaced = s.body(start, limit)
		}
		s.mu.Unlock()
		if version != "" {
			w.Header().Set("Last-Modified-Version", version)
		}
		if replaced {
			_ = json.NewEncoder(w).Encode(override)
			return
		}
		page := []map[string]any{}
		for i := start; i < start+limit && i < 5; i++ {
			page = append(page, snapshotFormatItem(i))
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
}

func publishedSnapshot(t *testing.T, output string) (data, manifest []byte) {
	t.Helper()
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err = os.ReadFile(output + ".manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	return data, manifest
}

func assertSnapshotUnchanged(t *testing.T, output string, wantData, wantManifest []byte) {
	t.Helper()
	gotData, gotManifest := publishedSnapshot(t, output)
	if string(gotData) != string(wantData) || string(gotManifest) != string(wantManifest) {
		t.Fatalf("a failed export replaced the previous snapshot or manifest")
	}
}

func exitCodeOf(err error) int {
	var coded *cliError
	if errors.As(err, &coded) {
		return coded.code
	}
	return 1
}

// A 200 null page is a malformed answer, not the end of the library. Treating
// it as EOF published an empty or truncated snapshot over the previous backup.
func TestExportSnapshotRefusesNullPageAndKeepsPreviousSnapshot(t *testing.T) {
	for _, format := range []string{"jsonl", "bibtex"} {
		for _, nullAt := range []int{0, 2} {
			t.Run(format+"-null-at-"+strconv.Itoa(nullAt), func(t *testing.T) {
				srv := &snapshotServer{version: "7"}
				newSnapshotServer(t, srv)
				output := filepath.Join(t.TempDir(), "library.out")
				if err := runSnapshotFormat(output, format, false); err != nil {
					t.Fatalf("seed snapshot: %v", err)
				}
				wantData, wantManifest := publishedSnapshot(t, output)

				srv.mu.Lock()
				srv.body = func(start, limit int) (any, bool) { return nil, start == nullAt }
				srv.mu.Unlock()
				err := runSnapshotFormat(output, format, false)
				if err == nil {
					t.Fatal("a null page was accepted as success")
				}
				assertSnapshotUnchanged(t, output, wantData, wantManifest)
				if cp, ok := readExportCheckpoint(output + ".checkpoint.json"); ok && cp.Done {
					t.Fatalf("null page marked the checkpoint done: %+v", cp)
				}
			})
		}
	}
}

func TestExportSnapshotAcceptsEmptyArrayAsEmptyLibrary(t *testing.T) {
	srv := &snapshotServer{version: "7", body: func(int, int) (any, bool) { return []any{}, true }}
	newSnapshotServer(t, srv)
	output := filepath.Join(t.TempDir(), "library.jsonl")
	if err := runSnapshotFormat(output, "jsonl", false); err != nil {
		t.Fatalf("empty library: %v", err)
	}
	assertSnapshotFormatManifest(t, output, "jsonl", 0)
}

// An edit between pages shifts the offsets: the listing repeats one record and
// drops another. The export must stop instead of certifying that traversal.
func TestExportSnapshotRefusesLibraryChangeBetweenPages(t *testing.T) {
	srv := &snapshotServer{version: "7"}
	newSnapshotServer(t, srv)
	output := filepath.Join(t.TempDir(), "library.jsonl")
	if err := runSnapshotFormat(output, "jsonl", false); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	wantData, wantManifest := publishedSnapshot(t, output)

	srv.mu.Lock()
	srv.body = func(start, limit int) (any, bool) {
		if start == 0 {
			return nil, false
		}
		srv.version = "8" // the edit lands after the first page
		return nil, false
	}
	srv.mu.Unlock()
	err := runSnapshotFormat(output, "jsonl", false)
	if err == nil {
		t.Fatal("export succeeded across a library change")
	}
	if exitCodeOf(err) != 9 {
		t.Fatalf("exit code = %d (%v), want 9", exitCodeOf(err), err)
	}
	assertSnapshotUnchanged(t, output, wantData, wantManifest)
}

// A resumed run replays a saved offset, so it must start on the same library
// version the checkpoint recorded.
func TestExportSnapshotResumeRefusesChangedLibraryVersion(t *testing.T) {
	srv := &snapshotServer{version: "7"}
	newSnapshotServer(t, srv)
	output := filepath.Join(t.TempDir(), "library.jsonl")
	if err := runSnapshotFormat(output, "jsonl", false); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	wantData, wantManifest := publishedSnapshot(t, output)

	failSecond := true
	srv.mu.Lock()
	srv.body = func(start, limit int) (any, bool) {
		if start == 2 && failSecond {
			return nil, true // JSON null interrupts the run after one page
		}
		return nil, false
	}
	srv.mu.Unlock()
	if err := runSnapshotFormat(output, "jsonl", false); err == nil {
		t.Fatal("interrupted export succeeded")
	}
	checkpointPath := output + ".checkpoint.json"
	cp, ok := readExportCheckpoint(checkpointPath)
	if !ok || cp.NextStart != 2 || cp.LibraryVersion != "7" {
		t.Fatalf("checkpoint = %+v, readable=%v; want offset 2 at library version 7", cp, ok)
	}

	srv.mu.Lock()
	srv.version = "8"
	failSecond = false
	srv.starts = nil
	srv.mu.Unlock()
	err := runSnapshotFormat(output, "jsonl", true)
	if err == nil || exitCodeOf(err) != 9 {
		t.Fatalf("resume across a library change = %v, want exit 9", err)
	}
	assertSnapshotUnchanged(t, output, wantData, wantManifest)

	srv.mu.Lock()
	srv.version = "7"
	srv.mu.Unlock()
	if err := runSnapshotFormat(output, "jsonl", true); err != nil {
		t.Fatalf("resume on the recorded version: %v", err)
	}
	assertSnapshotFormatManifest(t, output, "jsonl", 5)
}

func TestBuildExportLockfileRejectsDuplicateKeys(t *testing.T) {
	a, _ := json.Marshal(snapshotFormatItem(1))
	b, _ := json.Marshal(snapshotFormatItem(2))
	if _, err := buildExportLockfile("library", "jsonl", []json.RawMessage{a, b, a}); err == nil {
		t.Fatal("duplicate key accepted by the manifest builder")
	}
	if _, err := buildExportLockfile("library", "jsonl", []json.RawMessage{a, b}); err != nil {
		t.Fatalf("distinct keys rejected: %v", err)
	}
}
