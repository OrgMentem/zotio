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

func TestAcquireWriterLockChmodNonPermissionErrorIsFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".writer.lock")
	orig := osChmod
	defer func() { osChmod = orig }()

	// Use a non-permission error (ENOENT-style) to prove the old guard
	// `os.IsPermission(err)` suppressed it. The injected error is not a
	// permission error, so IsPermission returns false, but the new code must
	// still surface it.
	injected := errors.New("simulated read-only filesystem")
	osChmod = func(_ string, _ os.FileMode) error { return injected }

	_, err := AcquireWriterLock(path, "writing state")
	if err == nil {
		t.Fatal("AcquireWriterLock() error = nil, want injected chmod error")
	}
	if !errors.Is(err, injected) && !strings.Contains(err.Error(), "simulated read-only filesystem") {
		t.Fatalf("AcquireWriterLock() error = %q, want to wrap injected chmod error", err.Error())
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("AcquireWriterLock() error %q does not mention lock path %q", err.Error(), path)
	}
}

func TestAcquireWriterLockChmodPermissionErrorWithPrivateFileIsBenign(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX file permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".writer.lock")
	orig := osChmod
	defer func() { osChmod = orig }()

	// Simulate EACCES under the same conditions: file already exists with 0600
	// so lockFileIsPrivate will be true, and permission errors should be tolerated.
	// Create the file first with private mode.
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("prep lock file: %v", err)
	}
	permErr := os.ErrPermission
	osChmod = func(_ string, _ os.FileMode) error { return permErr }

	lock, err := AcquireWriterLock(path, "writing state")
	if err != nil {
		t.Fatalf("AcquireWriterLock() with permission error on private file error = %v, want nil (tolerated)", err)
	}
	defer func() {
		if err := lock.Release(); err != nil {
			t.Errorf("Release() error = %v", err)
		}
	}()
}

func TestAcquireWriterLockChmodPermissionErrorWithNonPrivateFileIsFatal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX file permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".writer.lock")
	orig := osChmod
	defer func() { osChmod = orig }()

	// Create file with permissive mode so lockFileIsPrivate returns false.
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("prep lock file: %v", err)
	}
	permErr := os.ErrPermission
	osChmod = func(_ string, _ os.FileMode) error { return permErr }

	_, err := AcquireWriterLock(path, "writing state")
	if err == nil {
		t.Fatal("AcquireWriterLock() error = nil, want permission chmod error on non-private file")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not mention path %q", err.Error(), path)
	}
}
