// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package zoteroprefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProfile(t *testing.T, prefs string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prefs.js"), []byte(prefs), 0o600); err != nil {
		t.Fatalf("write prefs.js: %v", err)
	}
	return dir
}

// The operator's real configuration: files belong on a personal WebDAV server,
// so a Web API upload into Zotero's cloud storage is a misroute.
func TestPersonalLibraryReportsWebDAVWhenProtocolIsWebDAV(t *testing.T) {
	dir := writeProfile(t, `
user_pref("extensions.zotero.dataDir", "/Users/me/Zotero");
user_pref("extensions.zotero.sync.storage.protocol", "webdav");
user_pref("extensions.zotero.sync.storage.url", "webdav.example.com/home/Sources");
user_pref("extensions.zotero.sync.storage.verified", true);
`)
	fs, err := LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if !fs.Found() {
		t.Fatal("Found() = false, want a located profile")
	}
	if got := fs.Personal(); got.Mode != StorageWebDAV || !got.Enabled {
		t.Fatalf("Personal() = %+v, want WebDAV and enabled", got)
	}
	if !fs.Verified {
		t.Fatal("Verified = false, want true")
	}
	if got := fs.WebDAVHost(); got != "webdav.example.com" {
		t.Fatalf("WebDAVHost() = %q, want the bare host", got)
	}
	if got := fs.Describe(StorageWebDAV); got != "WebDAV (webdav.example.com)" {
		t.Fatalf("Describe = %q", got)
	}
}

// Zotero uses its own storage for groups regardless of the WebDAV protocol
// setting, which is personal-library only (storageLocal.js getModeForLibrary).
func TestGroupLibrariesAlwaysUseZoteroCloudEvenUnderWebDAV(t *testing.T) {
	dir := writeProfile(t, `user_pref("extensions.zotero.sync.storage.protocol", "webdav");`)
	fs, err := LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if got := fs.Group().Mode; got != StorageZoteroCloud {
		t.Fatalf("Group().Mode = %q, want %q", got, StorageZoteroCloud)
	}
}

// prefs.js records only non-default values, so absent keys must resolve to
// Zotero's shipped defaults (sync.storage.enabled true, protocol "zotero").
func TestAbsentKeysResolveToZoteroDefaults(t *testing.T) {
	dir := writeProfile(t, `user_pref("extensions.zotero.dataDir", "/Users/me/Zotero");`)
	fs, err := LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if got := fs.Personal(); got.Mode != StorageZoteroCloud || !got.Enabled {
		t.Fatalf("Personal() = %+v, want zotero cloud and enabled", got)
	}
	if got := fs.Group(); got.Mode != StorageZoteroCloud || !got.Enabled {
		t.Fatalf("Group() = %+v, want zotero cloud and enabled", got)
	}
}

// Mode and Enabled are separate settings in Zotero (getModeForLibrary vs
// getEnabledForLibrary); collapsing them would lose the destination whenever
// syncing happens to be switched off.
func TestDisabledSyncingKeepsTheStorageModeVisible(t *testing.T) {
	dir := writeProfile(t, `
user_pref("extensions.zotero.sync.storage.protocol", "webdav");
user_pref("extensions.zotero.sync.storage.enabled", false);
user_pref("extensions.zotero.sync.storage.groups.enabled", false);
`)
	fs, err := LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if got := fs.Personal(); got.Mode != StorageWebDAV || got.Enabled {
		t.Fatalf("Personal() = %+v, want the WebDAV mode preserved with Enabled=false", got)
	}
	if got := fs.Group(); got.Mode != StorageZoteroCloud || got.Enabled {
		t.Fatalf("Group() = %+v, want zotero cloud with Enabled=false", got)
	}
}

// A machine with no Zotero desktop must report unknown, never "Zotero cloud":
// callers treat unknown as "no evidence of a misroute" and proceed.
func TestMissingProfileReportsUnknownRatherThanCloud(t *testing.T) {
	fs, err := LoadProfile(t.TempDir())
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if fs.Found() {
		t.Fatal("Found() = true for a directory with no prefs.js")
	}
	if got := fs.Personal().Mode; got != StorageUnknown {
		t.Fatalf("Personal().Mode = %q, want %q", got, StorageUnknown)
	}
	if got := fs.Group().Mode; got != StorageUnknown {
		t.Fatalf("Group().Mode = %q, want %q", got, StorageUnknown)
	}
}

// Reading "not webdav" out of a value that was never decoded is the fail-open
// direction: it would silently permit the cloud upload this package exists to
// catch. An undecodable or unmodelled protocol must report unknown.
func TestUnrecognisedProtocolReportsUnknownNotCloud(t *testing.T) {
	for _, prefs := range []string{
		`user_pref("extensions.zotero.sync.storage.protocol", "s3");`,
		`user_pref("extensions.zotero.sync.storage.protocol", 42);`,
	} {
		fs, err := LoadProfile(writeProfile(t, prefs))
		if err != nil {
			t.Fatalf("LoadProfile(%q): %v", prefs, err)
		}
		if got := fs.Personal().Mode; got != StorageUnknown {
			t.Errorf("Personal().Mode for %q = %q, want %q", prefs, got, StorageUnknown)
		}
	}
}

// A \uXXXX escape decoded by merely dropping the backslash turns "webdav" into
// a string that no longer compares equal, silently downgrading a WebDAV
// profile to Zotero's cloud.
func TestUnicodeEscapedProtocolStillDecodesToWebDAV(t *testing.T) {
	dir := writeProfile(t, `user_pref("extensions.zotero.sync.storage.protocol", "webd\u0061v");`)
	fs, err := LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if fs.Protocol != "webdav" {
		t.Fatalf("Protocol = %q, want the escape decoded to webdav", fs.Protocol)
	}
	if got := fs.Personal().Mode; got != StorageWebDAV {
		t.Fatalf("Personal().Mode = %q, want %q", got, StorageWebDAV)
	}
}

func TestWebDAVHostStripsSchemePathAndCredentials(t *testing.T) {
	cases := map[string]string{
		"webdav.example.com/home/Sources":          "webdav.example.com",
		"https://webdav.example.com/zotero":        "webdav.example.com",
		"https://user:secret@webdav.example.com/z": "webdav.example.com",
		"webdav.example.com:8443/zotero":           "webdav.example.com:8443",
		"":                                         "",
	}
	for in, want := range cases {
		fs := FileStorage{URL: in}
		if got := fs.WebDAVHost(); got != want {
			t.Errorf("WebDAVHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParsePrefsHandlesBOMEscapesBooleansAndIntegers(t *testing.T) {
	dir := writeProfile(t, "\ufeff// Mozilla User Preferences\r\n"+`
user_pref("extensions.zotero.sync.storage.protocol", "web\"dav");
user_pref("extensions.zotero.sync.storage.enabled", 0);
user_pref("malformed_line";
user_pref("extensions.zotero.sync.storage.groups.enabled", true);
`)
	fs, err := LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if fs.Protocol != `web"dav` {
		t.Fatalf("Protocol = %q, want the unescaped string", fs.Protocol)
	}
	// A non-zero integer is true; 0 is false, the same way the preferences
	// system coerces it.
	if fs.Enabled {
		t.Fatal("Enabled = true, want the integer 0 read as false")
	}
	if !fs.GroupsEnabled {
		t.Fatal("GroupsEnabled = false, want true")
	}
}

// A BOM on the first pref line must not hide that preference, which would
// present it as absent and fall back to the cloud default.
func TestBOMOnAPreferenceLineDoesNotHideIt(t *testing.T) {
	dir := writeProfile(t, "\ufeff"+`user_pref("extensions.zotero.sync.storage.protocol", "webdav");`)
	fs, err := LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if got := fs.Personal().Mode; got != StorageWebDAV {
		t.Fatalf("Personal().Mode = %q, want %q", got, StorageWebDAV)
	}
}

// Later duplicates win, matching the preferences system.
func TestDuplicatePreferencesResolveLastOneWins(t *testing.T) {
	dir := writeProfile(t, `
user_pref("extensions.zotero.sync.storage.protocol", "zotero");
user_pref("extensions.zotero.sync.storage.protocol", "webdav");
`)
	fs, err := LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if got := fs.Personal().Mode; got != StorageWebDAV {
		t.Fatalf("Personal().Mode = %q, want the last value to win", got)
	}
}

// Silently parsing a prefix would turn a present preference into an absent one,
// and absent preferences fall back to Zotero's cloud defaults.
func TestOversizedPrefsFileIsAnErrorNotAPartialRead(t *testing.T) {
	dir := t.TempDir()
	padding := strings.Repeat("// padding\n", (maxPrefsFileBytes/11)+16)
	body := padding + `user_pref("extensions.zotero.sync.storage.protocol", "webdav");` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "prefs.js"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfile(dir); err == nil {
		t.Fatal("LoadProfile returned nil error for an oversized prefs.js")
	}
}

// Discovery must follow profiles.ini rather than guessing, so the reported
// configuration is the one Zotero most likely starts with.
func TestDiscoverPrefersTheDefaultProfileFromProfilesINI(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"Profiles/aaaaaaaa.other", "Profiles/bbbbbbbb.default"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(name)), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	ini := "[General]\nStartWithLastProfile=1\n\n" +
		"[Profile0]\nName=other\nIsRelative=1\nPath=Profiles/aaaaaaaa.other\n\n" +
		"[Profile1]\nName=default\nIsRelative=1\nPath=Profiles/bbbbbbbb.default\nDefault=1\n"
	if err := os.WriteFile(filepath.Join(root, "profiles.ini"), []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}

	all, preferred, err := profilesFromINI(filepath.Join(root, "profiles.ini"), root)
	if err != nil {
		t.Fatalf("profilesFromINI: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("profiles = %v, want both listed", all)
	}
	want := filepath.Join(root, "Profiles", "bbbbbbbb.default")
	if preferred != want {
		t.Fatalf("preferred = %q, want %q", preferred, want)
	}
}

// Zotero supports Firefox's -P switch, so Default=1 does not prove which
// profile is running. With several profiles a WebDAV one must still surface,
// flagged ambiguous, rather than being masked by a cloud-configured default.
func TestMultipleProfilesFoldInWebDAVAndReportAmbiguity(t *testing.T) {
	root := t.TempDir()
	mk := func(name, prefs string) string {
		dir := filepath.Join(root, "Profiles", name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "prefs.js"), []byte(prefs), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	mk("aaaaaaaa.default", `user_pref("extensions.zotero.sync.storage.protocol", "zotero");`)
	mk("bbbbbbbb.work", `
user_pref("extensions.zotero.sync.storage.protocol", "webdav");
user_pref("extensions.zotero.sync.storage.url", "nas.example.com/zotero");
`)
	ini := "[Profile0]\nIsRelative=1\nPath=Profiles/aaaaaaaa.default\nDefault=1\n\n" +
		"[Profile1]\nIsRelative=1\nPath=Profiles/bbbbbbbb.work\n"
	if err := os.WriteFile(filepath.Join(root, "profiles.ini"), []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}

	all, preferred, err := profilesFromINI(filepath.Join(root, "profiles.ini"), root)
	if err != nil {
		t.Fatalf("profilesFromINI: %v", err)
	}
	fs, err := loadAcross(all, preferred)
	if err != nil {
		t.Fatalf("loadAcross: %v", err)
	}
	if got := fs.Personal().Mode; got != StorageWebDAV {
		t.Fatalf("Personal().Mode = %q, want the WebDAV profile folded in", got)
	}
	if !fs.Ambiguous {
		t.Fatal("Ambiguous = false, want the multi-profile uncertainty reported")
	}
	if fs.ProfileCount != 2 {
		t.Fatalf("ProfileCount = %d, want 2", fs.ProfileCount)
	}
	if got := fs.WebDAVHost(); got != "nas.example.com" {
		t.Fatalf("WebDAVHost() = %q, want the folded-in profile's host", got)
	}
}

// Pinning the profile removes the ambiguity entirely.
func TestProfileDirOverrideWinsAndIsNotAmbiguous(t *testing.T) {
	dir := writeProfile(t, `user_pref("extensions.zotero.sync.storage.protocol", "webdav");`)
	t.Setenv(ProfileDirEnv, dir)

	fs, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := fs.Personal().Mode; got != StorageWebDAV {
		t.Fatalf("Personal().Mode = %q, want %q", got, StorageWebDAV)
	}
	if fs.Ambiguous {
		t.Fatal("Ambiguous = true for an explicitly pinned profile")
	}
	if fs.ProfilePath != dir {
		t.Fatalf("ProfilePath = %q, want %q", fs.ProfilePath, dir)
	}
}

// Zotero's data directory (~/Zotero by default) is not its profile directory
// and must never be probed for prefs.js.
func TestProfileRootDoesNotProbeTheDataDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "Zotero"), 0o750); err != nil {
		t.Fatal(err)
	}
	root, err := profileRoot()
	if err != nil {
		t.Fatalf("profileRoot: %v", err)
	}
	if root == filepath.Join(home, "Zotero") {
		t.Fatal("profileRoot returned the Zotero DATA directory")
	}
}
