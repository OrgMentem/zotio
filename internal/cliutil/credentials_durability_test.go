// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cliutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// seedCredentialsFile writes a credentials file under the isolated data dir and
// returns its path.
func seedCredentialsFile(t *testing.T, body string) string {
	t.Helper()
	resetPathEnv(t)
	path, err := credentialsPath()
	if err != nil {
		t.Fatalf("credentialsPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	return path
}

func stubSyncDir(t *testing.T, fn func(string) error) {
	t.Helper()
	orig := syncDir
	t.Cleanup(func() { syncDir = orig })
	syncDir = fn
}

// A reported logout must survive power loss: the unlink only reaches stable
// storage once the parent directory is synced, so RemoveCredentials must run
// that barrier after the unlink, still run it when the file is already gone
// (the retry of a logout whose sync failed), and surface a failed sync instead
// of reporting the credential removed.
func TestRemoveCredentialsSyncsDirectoryAfterUnlink(t *testing.T) {
	path := seedCredentialsFile(t, "api_key = \"secret\"\n")

	var syncedDir string
	var existedAtSync bool
	stubSyncDir(t, func(dir string) error {
		syncedDir = dir
		_, statErr := os.Stat(path)
		existedAtSync = statErr == nil
		return nil
	})
	if err := RemoveCredentials(); err != nil {
		t.Fatalf("RemoveCredentials: %v", err)
	}
	if syncedDir != filepath.Dir(path) {
		t.Fatalf("synced dir = %q, want %q", syncedDir, filepath.Dir(path))
	}
	if existedAtSync {
		t.Fatal("directory was synced before the credentials file was unlinked, so the sync cannot cover the removal")
	}

	syncedDir = ""
	if err := RemoveCredentials(); err != nil {
		t.Fatalf("RemoveCredentials on an already-removed file: %v", err)
	}
	if syncedDir != filepath.Dir(path) {
		t.Fatalf("retry synced dir = %q, want %q: a retry after a failed sync must complete the barrier", syncedDir, filepath.Dir(path))
	}

	boom := errors.New("fsync failed")
	stubSyncDir(t, func(string) error { return boom })
	if err := RemoveCredentials(); !errors.Is(err, boom) {
		t.Fatalf("RemoveCredentials with a failing sync = %v, want %v", err, boom)
	}
}

// A kill inside atomicWrite leaves ".credentials.toml.<digits>.tmp" holding the
// complete secret. Logout must remove it, and must not touch files that merely
// look similar.
func TestRemoveCredentialsSweepsOrphanedAtomicTemps(t *testing.T) {
	path := seedCredentialsFile(t, "api_key = \"live-secret\"\n")
	dir := filepath.Dir(path)
	orphan := filepath.Join(dir, ".credentials.toml.1234567.tmp")
	keep := []string{
		filepath.Join(dir, ".credentials.toml.bak.tmp"), // not CreateTemp's digits
		filepath.Join(dir, ".config.toml.1234567.tmp"),  // another target's temp
		filepath.Join(dir, "credentials.toml.1234567.tmp"),
	}
	for _, p := range append([]string{orphan}, keep...) {
		if err := os.WriteFile(p, []byte("api_key = \"orphan-secret\"\n"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	if err := RemoveCredentials(); err != nil {
		t.Fatalf("RemoveCredentials: %v", err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned temp file survived logout: stat err = %v", err)
	}
	for _, p := range keep {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("RemoveCredentials removed unrelated %s: %v", filepath.Base(p), err)
		}
	}
}

func TestRemoveAtomicTempsSkipsDirectoriesAndMissingDir(t *testing.T) {
	path := seedCredentialsFile(t, "")
	lookalike := filepath.Join(filepath.Dir(path), ".credentials.toml.42.tmp")
	if err := os.Mkdir(lookalike, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAtomicTemps(path); err != nil {
		t.Fatalf("RemoveAtomicTemps: %v", err)
	}
	if info, err := os.Stat(lookalike); err != nil || !info.IsDir() {
		t.Fatalf("directory named like a temp was removed or replaced: %v", err)
	}
	if err := RemoveAtomicTemps(filepath.Join(t.TempDir(), "absent", "credentials.toml")); err != nil {
		t.Fatalf("RemoveAtomicTemps in a missing dir = %v, want nil", err)
	}
}
