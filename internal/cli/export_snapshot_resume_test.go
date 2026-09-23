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
			committed, err := os.ReadFile(output)
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
			f, err := os.OpenFile(output, os.O_WRONLY|os.O_APPEND, 0)
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
