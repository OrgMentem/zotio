// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListProfileNamesCorruptStoreWarns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Reset dedup map so warning fires in this test process.
	profileWarnedMu.Lock()
	profileWarned = map[string]struct{}{}
	profileWarnedMu.Unlock()

	p, err := profileStorePath()
	if err != nil {
		t.Fatalf("profileStorePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte("{invalid json"), 0o600); err != nil {
		t.Fatalf("write corrupt profiles: %v", err)
	}

	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	names := ListProfileNames()
	w.Close()
	os.Stderr = oldStderr
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	stderr := buf.String()

	if len(names) != 0 {
		t.Fatalf("corrupt store should return nil/empty, got %v", names)
	}
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "profiles") {
		t.Fatalf("expected stderr warning about profiles, got %q", stderr)
	}
	// Second call should be deduped (warn once per process per path)
	r2, w2, _ := os.Pipe()
	os.Stderr = w2
	_ = ListProfileNames()
	w2.Close()
	os.Stderr = oldStderr
	var buf2 bytes.Buffer
	if _, err := buf2.ReadFrom(r2); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	if buf2.Len() != 0 {
		t.Fatalf("second call should not re-warn, got %q", buf2.String())
	}
}

func TestListProfileNamesMissingStoreSilent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profileWarnedMu.Lock()
	profileWarned = map[string]struct{}{}
	profileWarnedMu.Unlock()

	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	names := ListProfileNames()
	w.Close()
	os.Stderr = oldStderr
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("missing store should be silent, got stderr %q", buf.String())
	}
	if len(names) != 0 {
		t.Fatalf("missing store should return nil/empty, got %v", names)
	}
}
