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

func TestExportHelpMatchesSnapshotFlags(t *testing.T) {
	var parentHelp bytes.Buffer
	parent := newExportCmd(&rootFlags{})
	parent.SetOut(&parentHelp)
	parent.SetErr(&parentHelp)
	parent.SetArgs([]string{"--help"})
	if err := parent.Execute(); err != nil {
		t.Fatalf("export --help: %v", err)
	}
	for _, flag := range []string{"--format", "--no-cache"} {
		if strings.Contains(parentHelp.String(), flag) {
			t.Fatalf("export --help advertises unreachable %s:\n%s", flag, parentHelp.String())
		}
	}

	var snapshotHelp bytes.Buffer
	snapshot := newExportSnapshotCmd(&rootFlags{})
	snapshot.SetOut(&snapshotHelp)
	snapshot.SetErr(&snapshotHelp)
	snapshot.SetArgs([]string{"--help"})
	if err := snapshot.Execute(); err != nil {
		t.Fatalf("export snapshot --help: %v", err)
	}
	for _, flag := range []string{"--output", "--limit", "--page-size", "--resume"} {
		if !strings.Contains(snapshotHelp.String(), flag) {
			t.Fatalf("export snapshot --help omits documented %s:\n%s", flag, snapshotHelp.String())
		}
	}
	for _, flag := range []string{"--format", "--no-cache"} {
		if strings.Contains(snapshotHelp.String(), flag) {
			t.Fatalf("export snapshot --help advertises rejected %s:\n%s", flag, snapshotHelp.String())
		}
	}
}

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

func TestCanonicalOutputPathMatchesDanglingSymlinkTarget(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "missing-vault")
	link := filepath.Join(parent, "vault-link")
	if err := os.Symlink("missing-vault", link); err != nil {
		t.Skipf("creating symlink: %v", err)
	}
	throughLink, err := canonicalOutputPath(link)
	if err != nil {
		t.Fatalf("canonical dangling symlink: %v", err)
	}
	direct, err := canonicalOutputPath(target)
	if err != nil {
		t.Fatalf("canonical direct target: %v", err)
	}
	if throughLink != direct {
		t.Fatalf("dangling link identity = %q, direct target = %q", throughLink, direct)
	}
}

func TestCanonicalOutputPathRejectsSymlinkCycle(t *testing.T) {
	parent := t.TempDir()
	first := filepath.Join(parent, "first")
	second := filepath.Join(parent, "second")
	if err := os.Symlink("second", first); err != nil {
		t.Skipf("creating first symlink: %v", err)
	}
	if err := os.Symlink("first", second); err != nil {
		t.Fatalf("creating second symlink: %v", err)
	}
	if _, err := canonicalOutputPath(first); err == nil || !strings.Contains(err.Error(), "possible cycle") {
		t.Fatalf("cycle error = %v, want clear cycle error", err)
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

	manifestRaw, err := os.ReadFile(out + ".manifest.json")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var lf exportLockfile
	if err := json.Unmarshal(manifestRaw, &lf); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if lf.Count != corpus || len(lf.Items) != corpus || lf.ContentSHA256 == "" {
		t.Errorf("manifest = {count:%d items:%d sha:%q}, want count/items %d with a hash", lf.Count, len(lf.Items), lf.ContentSHA256, corpus)
	}
	if lf.Items[0].Key != "K000" {
		t.Errorf("manifest items not sorted: first = %q, want K000", lf.Items[0].Key)
	}
	assertFileMode(t, out, 0o600)
	assertFileMode(t, out+".manifest.json", 0o600)
	// The lock file is retained: plain exports and --deliver=file share this
	// key, and unlinking it could split the flock namespace (ADR-0005).
	if _, err := os.Stat(out + ".lock"); err != nil {
		t.Errorf("writer lock should be retained on success, stat err = %v", err)
	}
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
	manifest, err := os.ReadFile(output + ".manifest.json")
	if err != nil {
		t.Fatalf("read snapshot manifest: %v", err)
	}
	var lock exportLockfile
	if err := json.Unmarshal(manifest, &lock); err != nil || lock.Count != 1 {
		t.Fatalf("snapshot manifest = %q, err = %v", manifest, err)
	}
	if _, err := os.Stat(output + ".lock"); err != nil {
		t.Fatalf("writer lock should be retained after a successful export: %v", err)
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
