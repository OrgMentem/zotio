// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package zoteroprefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDataDirFollowsUseDataDirLikeZotero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	custom := filepath.Join(t.TempDir(), "Research", "Zotero")
	quoted := strings.ReplaceAll(custom, `\`, `\\`)

	cases := []struct {
		name  string
		prefs string
		want  string
	}{
		{"chosen directory", `user_pref("extensions.zotero.useDataDir", true);` + "\n" +
			`user_pref("extensions.zotero.dataDir", "` + quoted + `");`, custom},
		// Zotero.DataDirectory.init ignores dataDir unless useDataDir is set.
		{"dataDir without useDataDir", `user_pref("extensions.zotero.dataDir", "` + quoted + `");`, filepath.Join(home, "Zotero")},
		{"useDataDir false", `user_pref("extensions.zotero.useDataDir", false);` + "\n" +
			`user_pref("extensions.zotero.dataDir", "` + quoted + `");`, filepath.Join(home, "Zotero")},
		{"no choice recorded", `user_pref("extensions.zotero.sync.storage.protocol", "zotero");`, filepath.Join(home, "Zotero")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DataDir(writeProfile(t, tc.prefs))
			if err != nil {
				t.Fatalf("DataDir: %v", err)
			}
			if got != tc.want {
				t.Fatalf("DataDir = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("no prefs.js", func(t *testing.T) {
		got, err := DataDir(t.TempDir())
		if err != nil || got != filepath.Join(home, "Zotero") {
			t.Fatalf("DataDir(no prefs.js) = %q, %v; want the default", got, err)
		}
	})
}

// A pre-5.0 persistent descriptor (or any relative value) names no location
// this package can use; guessing one would watch the wrong directory.
func TestDataDirRefusesANonAbsoluteChoice(t *testing.T) {
	dir := writeProfile(t, `user_pref("extensions.zotero.useDataDir", true);`+"\n"+
		`user_pref("extensions.zotero.dataDir", "Zotero");`)
	if got, err := DataDir(dir); err == nil {
		t.Fatalf("DataDir = %q, nil; want an error for a relative dataDir", got)
	}
}

// Snap and Flatpak give the Zotero process its own home, so the default data
// directory sits beside the sandboxed .zotero tree, not in the user's home.
func TestDefaultDataDirUsesTheSandboxHomeOnLinux(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	setGOOS(t, "linux")
	snapHome := filepath.Join(home, "snap", "zotero", "common")
	profile := filepath.Join(snapHome, ".zotero", "zotero", "aaaaaaaa.default")
	if err := os.MkdirAll(profile, 0o750); err != nil {
		t.Fatal(err)
	}
	got, err := DataDir(profile)
	if err != nil {
		t.Fatalf("DataDir: %v", err)
	}
	if want := filepath.Join(snapHome, "Zotero"); got != want {
		t.Fatalf("DataDir(snap profile) = %q, want %q", got, want)
	}
}

func TestProfilesHonoursThePinAndRefusesABadOne(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ProfileDirEnv, dir)
	all, preferred, err := Profiles()
	if err != nil || len(all) != 1 || all[0] != dir || preferred != dir {
		t.Fatalf("Profiles() with a pin = %v, %q, %v; want exactly the pin", all, preferred, err)
	}

	// Unlike Load, a pin need not hold a prefs.js: the lock file lives in the
	// directory whatever the preferences say. It must exist, though.
	t.Setenv(ProfileDirEnv, filepath.Join(dir, "missing"))
	if all, _, err := Profiles(); err == nil {
		t.Fatalf("Profiles() with a pin at a missing directory = %v, nil; want an error", all)
	}
	t.Setenv(ProfileDirEnv, "relative")
	if _, _, err := Profiles(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("Profiles() with a relative pin error = %v, want it to say the pin must be absolute", err)
	}
}
