// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cliutil

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resetPathEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{
		envPrefix + "_CONFIG_DIR",
		envPrefix + "_DATA_DIR",
		envPrefix + "_STATE_DIR",
		envPrefix + "_CACHE_DIR",
		envPrefix + "_HOME",
		"XDG_CONFIG_HOME",
		"XDG_DATA_HOME",
		"XDG_STATE_HOME",
		"XDG_CACHE_HOME",
	} {
		t.Setenv(name, "")
	}
	pathHomeOverrideMu.Lock()
	prev := pathHomeOverride
	pathHomeOverride = ""
	pathHomeOverrideMu.Unlock()
	t.Cleanup(func() {
		pathHomeOverrideMu.Lock()
		pathHomeOverride = prev
		pathHomeOverrideMu.Unlock()
	})
	return home
}

func TestKindDirDefaultsMatchLegacyLayout(t *testing.T) {
	home := resetPathEnv(t)

	tests := []struct {
		kind PathKind
		want string
	}{
		{PathKindConfig, filepath.Join(home, ".config", appName)},
		{PathKindData, filepath.Join(home, ".local", "share", appName)},
		{PathKindState, filepath.Join(home, ".local", "state", appName)},
		{PathKindCache, filepath.Join(home, ".cache", appName)},
	}
	for _, tt := range tests {
		got, err := KindDir(tt.kind)
		if err != nil {
			t.Fatalf("KindDir(%s) error = %v", kindName(tt.kind), err)
		}
		if got != tt.want {
			t.Fatalf("KindDir(%s) = %q, want %q", kindName(tt.kind), got, tt.want)
		}
	}
}

func TestKindDirHomeEnvUsesFlatKindLayout(t *testing.T) {
	resetPathEnv(t)
	root := filepath.Join(t.TempDir(), "persist")
	t.Setenv(envPrefix+"_HOME", root)

	tests := map[PathKind]string{
		PathKindConfig: filepath.Join(root, "config"),
		PathKindData:   filepath.Join(root, "data"),
		PathKindState:  filepath.Join(root, "state"),
		PathKindCache:  filepath.Join(root, "cache"),
	}
	for kind, want := range tests {
		got, err := KindDir(kind)
		if err != nil {
			t.Fatalf("KindDir(%s) error = %v", kindName(kind), err)
		}
		if got != want {
			t.Fatalf("KindDir(%s) = %q, want %q", kindName(kind), got, want)
		}
	}
}

func TestKindDirPerKindEnvBeatsHomeEnv(t *testing.T) {
	resetPathEnv(t)
	root := filepath.Join(t.TempDir(), "root")
	data := filepath.Join(t.TempDir(), "secure-data")
	t.Setenv(envPrefix+"_HOME", root)
	t.Setenv(envPrefix+"_DATA_DIR", data)

	got, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir() error = %v", err)
	}
	if got != data {
		t.Fatalf("DataDir() = %q, want literal per-kind dir %q", got, data)
	}
	configDir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir() error = %v", err)
	}
	if want := filepath.Join(root, "config"); configDir != want {
		t.Fatalf("ConfigDir() = %q, want %q", configDir, want)
	}
}

func TestKindDirXDGAddsAppName(t *testing.T) {
	resetPathEnv(t)
	xdg := filepath.Join(t.TempDir(), "xdg-data")
	t.Setenv("XDG_DATA_HOME", xdg)

	got, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir() error = %v", err)
	}
	if want := filepath.Join(xdg, appName); got != want {
		t.Fatalf("DataDir() = %q, want %q", got, want)
	}
}

func TestKindDirRelativeOverridesWarnAndFallThrough(t *testing.T) {
	home := resetPathEnv(t)
	t.Setenv(envPrefix+"_HOME", "relative/home")
	t.Setenv("XDG_DATA_HOME", "relative/xdg")

	stderr := captureStderr(t, func() {
		got, err := DataDir()
		if err != nil {
			t.Fatalf("DataDir() error = %v", err)
		}
		if want := filepath.Join(home, ".local", "share", appName); got != want {
			t.Fatalf("DataDir() = %q, want %q", got, want)
		}
	})
	for _, want := range []string{envPrefix + "_HOME", "relative/home", "XDG_DATA_HOME", "relative/xdg"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr %q does not mention %q", stderr, want)
		}
	}
}

func TestAtomicWriteFilePreservesContentAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "entry.json")
	want := []byte(`{"private":true}`)

	if err := AtomicWriteFile(path, want, 0o600, 0o700); err != nil {
		t.Fatalf("AtomicWriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("written content = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat written file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("written file mode = %04o, want 0600", got)
	}
}

func assertDataDir(t *testing.T, want string) {
	t.Helper()
	got, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir() error = %v", err)
	}
	if got != want {
		t.Fatalf("DataDir() = %q, want %q", got, want)
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}
