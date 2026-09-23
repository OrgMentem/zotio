// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/config"
)

func TestHomeDependentPathsRejectRelativeHome(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("HOME", "rel")

	for _, tc := range []struct {
		name string
		path func() (string, error)
	}{
		{"profiles", profileStorePath},
		{"feedback", feedbackFilePath},
		{"writer lock", func() (string, error) { return installationWriterLockPath(&rootFlags{}) }},
		{"demo store", func() (string, error) { return demoDBPath("zotio") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := tc.path()
			if err == nil || !strings.Contains(err.Error(), `home directory "rel" is not absolute`) {
				t.Fatalf("path = %q, error = %v; want invalid home error", path, err)
			}
			if path != "" {
				t.Errorf("path = %q, want no path", path)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(cwd, "rel")); !os.IsNotExist(err) {
		t.Errorf("relative home created under working directory: %v", err)
	}
}

// A "~" that cannot be expanded must fail, not survive as a literal path:
// "~/notes" left unexpanded would make vault sync write under a directory
// named "~" in the working directory.
func TestVaultRootTildeRefusesRelativeHome(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("HOME", "rel")
	out, err := vaultResolveOut(&config.VaultConfig{Root: "~/notes"})
	if err == nil {
		t.Fatalf("vaultResolveOut with relative HOME = %q, nil; want an error", out)
	}
	if !strings.Contains(err.Error(), "not absolute") {
		t.Errorf("error = %v, want it to name the relative home", err)
	}
}
