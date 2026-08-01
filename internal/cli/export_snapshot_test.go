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
	"sync"
	"testing"
	"time"
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

func TestCanonicalOutputPathResolvesExistingAncestor(t *testing.T) {
	realParent := t.TempDir()
	linkParent := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Skipf("creating symlink: %v", err)
	}
	got, err := canonicalOutputPath(filepath.Join(linkParent, "not-created", "snapshot.jsonl"))
	if err != nil {
		t.Fatalf("canonical output: %v", err)
	}
	resolvedParent, err := filepath.EvalSymlinks(realParent)
	if err != nil {
		t.Fatalf("resolve real parent: %v", err)
	}
	want := filepath.Join(resolvedParent, "not-created", "snapshot.jsonl")
	if got != want {
		t.Fatalf("canonical output = %q, want %q", got, want)
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
	if err := os.WriteFile(out, []byte("old snapshot"), 0o644); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	if err := os.Chmod(out, 0o644); err != nil {
		t.Fatalf("set snapshot mode: %v", err)
	}
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
	assertFileMode(t, out, 0o600)
	assertFileMode(t, out+".lock.json", 0o600)
	if _, err := os.Stat(out + ".checkpoint.json"); !os.IsNotExist(err) {
		t.Errorf("checkpoint sidecar should be removed on success, stat err = %v", err)
	}
}

func TestExportSnapshotSameOutputReturnsBusyBeforeSecondRequest(t *testing.T) {
	firstRequest := make(chan struct{})
	releaseFirst := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() {
			close(firstRequest)
			<-releaseFirst
		})
		_, _ = w.Write([]byte(`[{"key":"K1","version":1}]`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	output := filepath.Join(t.TempDir(), "snapshot.jsonl")
	first := newExportSnapshotCmd(&rootFlags{})
	first.SilenceErrors, first.SilenceUsage = true, true
	first.SetArgs([]string{"--output", output})
	firstErr := make(chan error, 1)
	go func() { firstErr <- first.Execute() }()
	<-firstRequest

	second := newExportSnapshotCmd(&rootFlags{})
	second.SilenceErrors, second.SilenceUsage = true, true
	second.SetArgs([]string{"--output", output})
	if err := second.Execute(); err == nil || ExitCode(err) != 9 {
		t.Fatalf("second snapshot error = %v, exit = %d; want busy precondition exit 9", err, ExitCode(err))
	}

	close(releaseFirst)
	if err := <-firstErr; err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var item map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &item); err != nil || item["key"] != "K1" {
		t.Fatalf("snapshot JSONL = %q, err = %v", data, err)
	}
	lockfile, err := os.ReadFile(output + ".lock.json")
	if err != nil {
		t.Fatalf("read snapshot lockfile: %v", err)
	}
	var lock exportLockfile
	if err := json.Unmarshal(lockfile, &lock); err != nil || lock.Count != 1 {
		t.Fatalf("snapshot lockfile = %q, err = %v", lockfile, err)
	}
}

func TestExportSnapshotDifferentOutputsRunConcurrently(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		<-release
		_, _ = w.Write([]byte(`[{"key":"K1","version":1}]`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	run := func(output string) <-chan error {
		cmd := newExportSnapshotCmd(&rootFlags{})
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		cmd.SetArgs([]string{"--output", output})
		done := make(chan error, 1)
		go func() { done <- cmd.Execute() }()
		return done
	}
	first := run(filepath.Join(t.TempDir(), "first.jsonl"))
	second := run(filepath.Join(t.TempDir(), "second.jsonl"))
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(3 * time.Second):
			t.Fatal("different output paths did not reach their requests concurrently")
		}
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first export: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second export: %v", err)
	}
}
