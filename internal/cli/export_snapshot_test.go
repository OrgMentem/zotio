// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// export snapshot command coverage.

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
	"testing"
)

func TestSnapshotScopePath(t *testing.T) {
	cases := []struct {
		in        string
		wantPath  string
		wantTag   string
		wantLabel string
		wantErr   bool
	}{
		{in: "", wantPath: "/items", wantLabel: "library"},
		{in: "library", wantPath: "/items", wantLabel: "library"},
		{in: "collection:ABCD", wantPath: "/collections/ABCD/items", wantLabel: "collection:ABCD"},
		{in: "tag:to-read", wantPath: "/items", wantTag: "to-read", wantLabel: "tag:to-read"},
		{in: "collection:", wantErr: true},
		{in: "item:X", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			path, params, label, err := snapshotScopePath(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("snapshotScopePath(%q) = (%q,%v,%q), want error", tc.in, path, params, label)
				}
				return
			}
			if err != nil {
				t.Fatalf("snapshotScopePath(%q): %v", tc.in, err)
			}
			if path != tc.wantPath || label != tc.wantLabel || params["tag"] != tc.wantTag {
				t.Errorf("snapshotScopePath(%q) = (%q, tag=%q, %q), want (%q, tag=%q, %q)", tc.in, path, params["tag"], label, tc.wantPath, tc.wantTag, tc.wantLabel)
			}
		})
	}
}

func TestExportSnapshotPaginatesAndLocks(t *testing.T) {
	const corpus = 150
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/items") {
			http.NotFound(w, r)
			return
		}
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 100
		}
		parts := make([]string, 0, limit)
		for i := start; i < start+limit && i < corpus; i++ {
			parts = append(parts, fmt.Sprintf(`{"key":"K%03d","version":%d}`, i, i))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[" + strings.Join(parts, ",") + "]"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	out := filepath.Join(t.TempDir(), "snap.jsonl")
	flags := &rootFlags{asJSON: true}
	cmd := newExportSnapshotCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"--output", out, "--page-size", "50"})
	var outBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export snapshot: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read data: %v", err)
	}
	lines := 0
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(l) != "" {
			lines++
		}
	}
	if lines != corpus {
		t.Errorf("data lines = %d, want %d (paginated across 3 pages of 50)", lines, corpus)
	}

	lockRaw, err := os.ReadFile(out + ".lock.json")
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	var lf exportLockfile
	if err := json.Unmarshal(lockRaw, &lf); err != nil {
		t.Fatalf("decode lockfile: %v", err)
	}
	if lf.Count != corpus || len(lf.Items) != corpus || lf.ContentSHA256 == "" {
		t.Errorf("lockfile = {count:%d items:%d sha:%q}, want count/items %d with a hash", lf.Count, len(lf.Items), lf.ContentSHA256, corpus)
	}
	if lf.Items[0].Key != "K000" {
		t.Errorf("lockfile items not sorted: first = %q, want K000", lf.Items[0].Key)
	}
	if _, err := os.Stat(out + ".checkpoint.json"); !os.IsNotExist(err) {
		t.Errorf("checkpoint sidecar should be removed on success, stat err = %v", err)
	}
}
