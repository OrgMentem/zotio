// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cliutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAcquireWriterLockLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", ".writer.lock")

	first, err := AcquireWriterLock(path, "updating profiles")
	if err != nil {
		t.Fatalf("first AcquireWriterLock() error = %v", err)
	}

	_, err = AcquireWriterLock(path, "saving profiles")
	if err == nil {
		t.Fatal("second AcquireWriterLock() error = nil, want busy error")
	}
	var busy *WriterLockBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("second AcquireWriterLock() error = %T %[1]v, want *WriterLockBusyError", err)
	}
	for _, want := range []string{"another zotio writer is active", "saving profiles", path, "retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("busy error = %q, want substring %q", err, want)
		}
	}

	if err := first.Release(); err != nil {
		t.Fatalf("first Release() error = %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("second Release() error = %v, want original nil result", err)
	}

	third, err := AcquireWriterLock(path, "saving profiles")
	if err != nil {
		t.Fatalf("AcquireWriterLock() after Release error = %v", err)
	}
	defer func() {
		if err := third.Release(); err != nil {
			t.Errorf("final Release() error = %v", err)
		}
	}()
}

func TestAcquireWriterLockSeparatePaths(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireWriterLock(filepath.Join(dir, "first.lock"), "first write")
	if err != nil {
		t.Fatalf("first AcquireWriterLock() error = %v", err)
	}
	defer func() {
		if err := first.Release(); err != nil {
			t.Errorf("first Release() error = %v", err)
		}
	}()

	second, err := AcquireWriterLock(filepath.Join(dir, "second.lock"), "second write")
	if err != nil {
		t.Fatalf("second AcquireWriterLock() error = %v, want independent lock", err)
	}
	defer func() {
		if err := second.Release(); err != nil {
			t.Errorf("second Release() error = %v", err)
		}
	}()
}

func TestAcquireWriterLockUsesPrivateMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX file permissions")
	}

	path := filepath.Join(t.TempDir(), ".writer.lock")
	lock, err := AcquireWriterLock(path, "writing state")
	if err != nil {
		t.Fatalf("AcquireWriterLock() error = %v", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			t.Errorf("Release() error = %v", err)
		}
	}()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("lock file mode = %#o, want %#o", got, os.FileMode(0o600))
	}
}
