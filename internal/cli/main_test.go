// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Package-wide isolation from the developer's real zotio data directory.

package cli

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points this package's tests at a throwaway HOME.
//
// Several tests install the production mutation hooks or execute commands whose
// applied writes are journaled. Any test that forgot `t.Setenv("HOME", ...)`
// appended its fixture run to the developer's real journal at
// ~/.local/share/zotio/journal — 16 entries per full-suite run, with keys like
// K1/K2, ITEM0001 and "Example 0..50" sitting in the same file as genuine
// library history, where `journal list` shows them and `journal undo` offers to
// reverse them.
//
// Isolating the whole package makes that structurally impossible instead of
// relying on every current and future test to remember. Per-test
// `t.Setenv("HOME", ...)` calls still work and still scope to their own test.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "zotio-cli-test-home")
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating isolated test HOME: %v\n", err)
		os.Exit(1)
	}

	// HOME becomes the sole data-dir root. The XDG overrides are CLEARED rather
	// than pointed at the temp dir: XDG_DATA_HOME outranks $HOME/.local/share,
	// so setting it would defeat the per-test `t.Setenv("HOME", t.TempDir())`
	// isolation many tests rely on and make them share one store.
	for key, value := range map[string]string{
		"HOME":            home,
		"XDG_DATA_HOME":   "",
		"XDG_CONFIG_HOME": "",
	} {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintf(os.Stderr, "isolating %s: %v\n", key, err)
			os.Exit(1)
		}
	}

	code := m.Run()
	// os.Exit skips defers, so clean up explicitly.
	_ = os.RemoveAll(home)
	os.Exit(code)
}
