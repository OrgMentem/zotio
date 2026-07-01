// Copyright 2026 enieuwy and contributors. Licensed under Apache-2.0. See LICENSE.

package cliutil

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMigrateLegacyDirsPlatformDefaultMovesAllKinds(t *testing.T) {
	home := resetPathEnv(t)

	cases := []struct {
		name     string
		legacy   string
		current  string
		fileName string
		content  string
	}{
		{
			name:     "config",
			legacy:   filepath.Join(home, ".config", legacyAppName),
			current:  filepath.Join(home, ".config", appName),
			fileName: "config.toml",
			content:  "config-sentinel",
		},
		{
			name:     "data",
			legacy:   filepath.Join(home, ".local", "share", legacyAppName),
			current:  filepath.Join(home, ".local", "share", appName),
			fileName: "data.db",
			content:  "data-sentinel",
		},
		{
			name:     "state",
			legacy:   filepath.Join(home, ".local", "state", legacyAppName),
			current:  filepath.Join(home, ".local", "state", appName),
			fileName: "x",
			content:  "state-sentinel",
		},
		{
			name:     "cache",
			legacy:   filepath.Join(home, ".cache", legacyAppName),
			current:  filepath.Join(home, ".cache", appName),
			fileName: "y",
			content:  "cache-sentinel",
		},
	}
	for _, tt := range cases {
		writeMigrationFile(t, filepath.Join(tt.legacy, tt.fileName), tt.content)
	}

	captureStderr(t, migrateLegacyDirs)

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			requireNoPath(t, tt.legacy)
			requireDirExists(t, tt.current)
			requireFileContent(t, filepath.Join(tt.current, tt.fileName), tt.content)
		})
	}
}

func TestMigrateLegacyDirsSkipsExistingNewDirWithoutClobber(t *testing.T) {
	home := resetPathEnv(t)
	legacyDir := filepath.Join(home, ".config", legacyAppName)
	currentDir := filepath.Join(home, ".config", appName)
	writeMigrationFile(t, filepath.Join(legacyDir, "old.txt"), "legacy-content")
	writeMigrationFile(t, filepath.Join(currentDir, "new.txt"), "current-content")

	migrateLegacyDirs()

	requireFileContent(t, filepath.Join(currentDir, "new.txt"), "current-content")
	requireFileContent(t, filepath.Join(legacyDir, "old.txt"), "legacy-content")
	requireNoPath(t, filepath.Join(currentDir, "old.txt"))
}

func TestMigrateLegacyDirsNoLegacyDirsIsCleanNoOp(t *testing.T) {
	home := resetPathEnv(t)
	currentDirs := []string{
		filepath.Join(home, ".config", appName),
		filepath.Join(home, ".local", "share", appName),
		filepath.Join(home, ".local", "state", appName),
		filepath.Join(home, ".cache", appName),
	}

	migrateLegacyDirs()

	for _, dir := range currentDirs {
		requireNoPath(t, dir)
	}
}

func TestMigrateLegacyDirsSkipsPerKindEnvOverride(t *testing.T) {
	home := resetPathEnv(t)
	customParent := t.TempDir()
	customConfig := filepath.Join(customParent, "custom")
	t.Setenv(envPrefix+"_CONFIG_DIR", customConfig)

	defaultLegacy := filepath.Join(home, ".config", legacyAppName)
	writeMigrationFile(t, filepath.Join(defaultLegacy, "config.toml"), "default-legacy")
	customLegacy := filepath.Join(customParent, legacyAppName)
	writeMigrationFile(t, filepath.Join(customLegacy, "override.txt"), "override-legacy")

	migrateLegacyDirs()

	requireFileContent(t, filepath.Join(defaultLegacy, "config.toml"), "default-legacy")
	requireNoPath(t, filepath.Join(home, ".config", appName))
	requireFileContent(t, filepath.Join(customLegacy, "override.txt"), "override-legacy")
	requireNoPath(t, filepath.Join(customParent, appName))
}

func TestMigrateLegacyDirsSkipsZoteroHomeRung(t *testing.T) {
	home := resetPathEnv(t)
	t.Setenv(envPrefix+"_HOME", filepath.Join(t.TempDir(), "zh"))
	defaultLegacy := filepath.Join(home, ".config", legacyAppName)
	writeMigrationFile(t, filepath.Join(defaultLegacy, "config.toml"), "home-env-legacy")

	migrateLegacyDirs()

	requireFileContent(t, filepath.Join(defaultLegacy, "config.toml"), "home-env-legacy")
	requireNoPath(t, filepath.Join(home, ".config", appName))
}

func TestMigrateLegacyDirsXDGRungMigratesConfig(t *testing.T) {
	resetPathEnv(t)
	xdg := filepath.Join(t.TempDir(), "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	legacyDir := filepath.Join(xdg, legacyAppName)
	currentDir := filepath.Join(xdg, appName)
	writeMigrationFile(t, filepath.Join(legacyDir, "config.toml"), "xdg-config")

	captureStderr(t, migrateLegacyDirs)

	requireNoPath(t, legacyDir)
	requireDirExists(t, currentDir)
	requireFileContent(t, filepath.Join(currentDir, "config.toml"), "xdg-config")
}

func TestMigrateLegacyDirsExportedRunsOnlyOnce(t *testing.T) {
	migrateLegacyOnce = sync.Once{}
	t.Cleanup(func() {
		migrateLegacyOnce = sync.Once{}
	})
	home := resetPathEnv(t)
	legacyDir := filepath.Join(home, ".config", legacyAppName)
	currentDir := filepath.Join(home, ".config", appName)

	MigrateLegacyDirs()
	writeMigrationFile(t, filepath.Join(legacyDir, "config.toml"), "created-after-first-call")
	MigrateLegacyDirs()

	requireFileContent(t, filepath.Join(legacyDir, "config.toml"), "created-after-first-call")
	requireNoPath(t, currentDir)
}

func writeMigrationFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func requireFileContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s content = %q, want %q", path, got, want)
	}
}

func requireDirExists(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", path)
	}
}

func requireNoPath(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s exists, want absent", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s: %v", path, err)
	}
}
