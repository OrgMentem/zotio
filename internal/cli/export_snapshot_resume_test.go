// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestExportSnapshotJSONLResumeDiscardsUncommittedPage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		extra  []byte
		legacy bool
	}{
		{name: "partial-page-write", extra: []byte(`{"key":"TORN`), legacy: false},
		{name: "complete-page-before-checkpoint", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var starts []int
			failed := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				start, _ := strconv.Atoi(r.URL.Query().Get("start"))
				limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
				mu.Lock()
				starts = append(starts, start)
				if start == 2 && !failed {
					failed = true
					mu.Unlock()
					http.Error(w, "interrupted", http.StatusBadRequest)
					return
				}
				mu.Unlock()
				var page []map[string]any
				for i := start; i < start+limit && i < 5; i++ {
					page = append(page, snapshotFormatItem(i))
				}
				_ = json.NewEncoder(w).Encode(page)
			}))
			t.Cleanup(srv.Close)
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
			output := filepath.Join(t.TempDir(), "library.jsonl")
			if err := runSnapshotFormat(output, "jsonl", false); err == nil {
				t.Fatal("interrupted snapshot unexpectedly succeeded")
			}
			checkpointPath := output + ".checkpoint.json"
			cp, ok := readExportCheckpoint(checkpointPath)
			if !ok || cp.Fetched != 2 || cp.DataBytes == nil {
				t.Fatalf("checkpoint = %+v, readable=%v; want two items and a byte offset", cp, ok)
			}
			// An interrupted run keeps its committed data in <output>.partial
			// and never creates or touches the output itself.
			if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
				t.Fatalf("interrupted run created the output file: %v", statErr)
			}
			committed, err := os.ReadFile(output + ".partial")
			if err != nil {
				t.Fatal(err)
			}
			if *cp.DataBytes != int64(len(committed)) {
				t.Fatalf("checkpoint bytes = %d, committed data = %d", *cp.DataBytes, len(committed))
			}
			extra := tc.extra
			if tc.legacy {
				for i := 2; i < 4; i++ {
					item, _ := json.Marshal(snapshotFormatItem(i))
					extra = append(extra, item...)
					extra = append(extra, '\n')
				}
				cp.DataBytes = nil // checkpoint written by an older zotio
				if err := writeExportCheckpoint(checkpointPath, cp); err != nil {
					t.Fatal(err)
				}
			}
			f, err := os.OpenFile(output+".partial", os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write(extra); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if err := runSnapshotFormat(output, "jsonl", true); err != nil {
				t.Fatalf("resume: %v", err)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
			if len(lines) != 5 {
				t.Fatalf("JSONL lines = %d, want 5: %s", len(lines), data)
			}
			for i, line := range lines {
				var item map[string]any
				if err := json.Unmarshal(line, &item); err != nil || item["key"] != fmt.Sprintf("K%03d", i) {
					t.Errorf("item %d = %s, decode error %v", i, line, err)
				}
			}
			assertSnapshotFormatManifest(t, output, "jsonl", 5)
			mu.Lock()
			gotStarts := fmt.Sprint(starts)
			mu.Unlock()
			if gotStarts != "[0 2 2 4]" {
				t.Errorf("page starts = %s, want [0 2 2 4]", gotStarts)
			}
		})
	}
}

// A kill during the checkpoint write leaves a torn checkpoint beside already
// committed pages. Resuming must refuse with an explicit corruption error and
// leave both the data file and the checkpoint untouched: the old code treated
// the torn checkpoint as absent, truncated the export, and lost the
// recoverable first page when the next fetch failed.
func TestExportSnapshotResumeRefusesCorruptCheckpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	output := filepath.Join(t.TempDir(), "library.jsonl")
	committed := []byte("{\"key\":\"K000\",\"version\":1}\n{\"key\":\"K001\",\"version\":2}\n")
	if err := os.WriteFile(output, committed, 0o600); err != nil {
		t.Fatalf("seed output: %v", err)
	}
	checkpointPath := output + ".checkpoint.json"
	torn := []byte(`{"path":"/items","next_start":`)
	if err := os.WriteFile(checkpointPath, torn, 0o600); err != nil {
		t.Fatalf("seed torn checkpoint: %v", err)
	}
	err := runSnapshotFormat(output, "jsonl", true)
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("resume error = %v, want checkpoint-corruption refusal", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read preserved output: %v", err)
	}
	if !bytes.Equal(got, committed) {
		t.Fatalf("output changed on checkpoint refusal: got %q want %q", got, committed)
	}
	kept, err := os.ReadFile(checkpointPath)
	if err != nil {
		t.Fatalf("checkpoint removed on refusal: %v", err)
	}
	if !bytes.Equal(kept, torn) {
		t.Fatalf("checkpoint changed on refusal: got %q want %q", kept, torn)
	}
}

func snapshotTestServer(t *testing.T, requests *int) {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*requests++
		mu.Unlock()
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var page []map[string]any
		for i := start; i < start+limit && i < 5; i++ {
			page = append(page, snapshotFormatItem(i))
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
}

// The data file and the manifest are one generation. A run that dies between
// the two renames leaves new data beside the old manifest; --resume must finish
// publishing the staged manifest without refetching, and a run without
// --resume must refuse instead of pairing yet another generation with it.
func TestExportSnapshotInterruptedPublicationIsRecoverable(t *testing.T) {
	for _, format := range []string{"jsonl", "csljson"} {
		t.Run(format, func(t *testing.T) {
			requests := 0
			snapshotTestServer(t, &requests)
			output := filepath.Join(t.TempDir(), "library."+format)
			manifest := output + ".manifest.json"
			if err := os.WriteFile(output, []byte("OLD DATA"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifest, []byte("OLD MANIFEST"), 0o600); err != nil {
				t.Fatal(err)
			}

			snapshotBetweenRenames = func() error { return fmt.Errorf("simulated crash between renames") }
			err := runSnapshotFormat(output, format, false)
			snapshotBetweenRenames = nil
			if err == nil || !strings.Contains(err.Error(), "simulated crash") {
				t.Fatalf("interrupted publication error = %v", err)
			}
			if got, _ := os.ReadFile(manifest); string(got) != "OLD MANIFEST" {
				t.Fatalf("manifest = %q, want the old manifest still in place", got)
			}
			if _, err := os.Stat(manifest + ".partial"); err != nil {
				t.Fatalf("staged manifest missing after interruption: %v", err)
			}
			fetchedBefore := requests

			// A fresh run must not silently pair new data with the old manifest.
			err = runSnapshotFormat(output, format, false)
			if err == nil || ExitCode(err) != 9 || !strings.Contains(err.Error(), "--resume") {
				t.Fatalf("rerun without --resume: err = %v (exit %d), want a precondition refusal naming --resume", err, ExitCode(err))
			}
			if requests != fetchedBefore {
				t.Fatalf("refused rerun fetched %d times", requests-fetchedBefore)
			}

			if err := runSnapshotFormat(output, format, true); err != nil {
				t.Fatalf("resume of interrupted publication: %v", err)
			}
			if requests != fetchedBefore {
				t.Fatalf("resume refetched: %d requests, want %d", requests, fetchedBefore)
			}
			if format == "jsonl" {
				items, err := readJSONLItems(output)
				if err != nil || len(items) != 5 {
					t.Fatalf("recovered JSONL data = %d items (err %v), want 5", len(items), err)
				}
			} else {
				assertSnapshotFormatContent(t, output, format, 5)
			}
			assertSnapshotFormatManifest(t, output, format, 5)
			for _, leftover := range []string{manifest + ".partial", output + ".partial", output + ".checkpoint.json"} {
				if _, err := os.Stat(leftover); !os.IsNotExist(err) {
					t.Errorf("%s remains after recovery: %v", leftover, err)
				}
			}
		})
	}
}

// The staging file is checked like the output target: a symlink there would
// send the snapshot's bytes, or a truncation on resume, to the link's target.
func TestExportSnapshotRefusesSymlinkedStagingFile(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "fresh"
		if resume {
			name = "resume"
		}
		t.Run(name, func(t *testing.T) {
			requests := 0
			snapshotTestServer(t, &requests)
			dir := t.TempDir()
			output := filepath.Join(dir, "library.jsonl")
			victim := filepath.Join(dir, "victim.txt")
			if err := os.WriteFile(victim, []byte("DO NOT TOUCH"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victim, output+".partial"); err != nil {
				t.Skipf("creating symlink: %v", err)
			}
			err := runSnapshotFormat(output, "jsonl", resume)
			if err == nil || !strings.Contains(err.Error(), "staging file") {
				t.Fatalf("symlinked staging file: err = %v, want a refusal naming the staging file", err)
			}
			if got, _ := os.ReadFile(victim); string(got) != "DO NOT TOUCH" {
				t.Fatalf("symlink target = %q, want it untouched", got)
			}
			if requests != 0 {
				t.Fatalf("refusal came after %d requests, want none", requests)
			}
		})
	}
}
