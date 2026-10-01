package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestFxaDemoDryRunCreatesNoSQLiteSidecars pins that previewing an existing,
// seeded sandbox reads demo.db without creating demo.db-wal or demo.db-shm.
func TestFxaDemoDryRunCreatesNoSQLiteSidecars(t *testing.T) {
	home := isolateDemoEnv(t, "0")
	runDemoCmd(t)
	dir := filepath.Join(home, ".local", "share", "zotio")
	for _, sidecar := range []string{"demo.db-wal", "demo.db-shm"} {
		if err := os.Remove(filepath.Join(dir, sidecar)); err != nil && !os.IsNotExist(err) {
			t.Fatalf("removing %s: %v", sidecar, err)
		}
	}
	before := fxaListDir(t, dir)

	report, err := previewDemo(context.Background(), filepath.Join(dir, "demo.db"), false)
	if err != nil {
		t.Fatalf("previewDemo: %v", err)
	}
	if report.WouldSeed {
		t.Error("preview of a seeded sandbox reports WouldSeed = true")
	}
	if after := fxaListDir(t, dir); after != before {
		t.Fatalf("demo --dry-run changed the sandbox directory:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func fxaListDir(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		lines = append(lines, fmt.Sprintf("%s %d %s %d", e.Name(), info.Size(), info.Mode(), info.ModTime().UnixNano()))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
