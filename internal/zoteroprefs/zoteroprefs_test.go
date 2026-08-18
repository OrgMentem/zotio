// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package zoteroprefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func writeProfile(t *testing.T, prefs string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prefs.js"), []byte(prefs), 0o600); err != nil {
		t.Fatalf("write prefs.js: %v", err)
	}
	return dir
}

// setGOOS points the package's platform switch at value for the duration of
// the test, restoring it afterwards. Tests use this to drive the Linux/Unix
// discovery path — including the Snap and Flatpak candidates — without
// needing to run on Linux.
func setGOOS(t *testing.T, value string) {
	t.Helper()
	old := goos
	goos = value
	t.Cleanup(func() { goos = old })
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
	if fs.AnyPersonalModeUnknown() {
		t.Fatal("AnyPersonalModeUnknown() = true for a profile with no malformed known key")
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

// A string built from concatenation ("zotero" + "-future") must not decode as
// a truncated prefix of its first operand: reading exactly "zotero" out of
// that line would report confident cloud-mode evidence the writer never
// actually wrote.
func TestConcatenatedStringExpressionIsUndecodable(t *testing.T) {
	dir := writeProfile(t, `user_pref("extensions.zotero.sync.storage.protocol", "zotero" + "-future");`)
	fs, err := LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if got := fs.Personal().Mode; got != StorageUnknown {
		t.Fatalf("Personal().Mode = %q, want %q (a truncated read would wrongly report cloud storage)", got, StorageUnknown)
	}
	if !fs.AnyPersonalModeUnknown() {
		t.Fatal("AnyPersonalModeUnknown() = false, want the undecodable protocol recorded")
	}
}

// A quoted string handed to a boolean key is a type mismatch, not evidence.
// The old truthy() coercion read a non-"true" string as a decoded false,
// which would manufacture a "syncing is off" refusal from a value the writer
// never wrote as a boolean at all.
func TestStringValueForABooleanKeyIsUndecodableNotFalse(t *testing.T) {
	dir := writeProfile(t, `user_pref("extensions.zotero.sync.storage.enabled", "garbage");`)
	fs, err := LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if !fs.Enabled {
		t.Fatal("Enabled = false, want the shipped default kept rather than a decoded false invented from a type mismatch")
	}
	if fs.AnyPersonalSyncDisabled() {
		t.Fatal("AnyPersonalSyncDisabled() = true, want a type mismatch to never become a positive hazard")
	}
	if !fs.AnyPersonalModeUnknown() {
		t.Fatal("AnyPersonalModeUnknown() = false, want the malformed known key recorded as indeterminate")
	}
}

// A well-formed value followed only by a trailing line comment is a normal,
// legitimate prefs.js line: a comment is not corruption and must still parse.
func TestTrailingLineCommentAfterAWellFormedValueStillParses(t *testing.T) {
	dir := writeProfile(t, `user_pref("extensions.zotero.sync.storage.enabled", false); // disabled by operator`)
	fs, err := LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if fs.Enabled {
		t.Fatal("Enabled = true, want the trailing comment to not prevent decoding false")
	}
	if !fs.AnyPersonalSyncDisabled() {
		t.Fatal("AnyPersonalSyncDisabled() = false, want the decoded false to count as a hazard")
	}
	if fs.AnyPersonalModeUnknown() {
		t.Fatal("AnyPersonalModeUnknown() = true, want a genuine decode followed by a comment to not read as malformed")
	}
}

// Zotero always writes prefs.js as UTF-8. A UTF-16 file makes every
// "user_pref(" prefix match fail, so every key would read as absent and
// resolve to the confident cloud+enabled defaults; that must surface as an
// evaluation failure instead.
func TestUTF16PrefsFileIsAnErrorNotAnEmptyReading(t *testing.T) {
	dir := t.TempDir()
	prefs := `user_pref("extensions.zotero.sync.storage.protocol", "webdav");` + "\n"
	units := utf16.Encode([]rune(prefs))
	buf := make([]byte, 0, 2+2*len(units))
	buf = append(buf, 0xff, 0xfe) // UTF-16LE byte order mark.
	for _, u := range units {
		buf = append(buf, byte(u), byte(u>>8))
	}
	if err := os.WriteFile(filepath.Join(dir, "prefs.js"), buf, 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := LoadProfile(dir)
	if err == nil {
		t.Fatalf("LoadProfile returned nil error for a UTF-16 prefs.js (fs = %+v)", fs)
	}
}

// The per-line scanner buffer must not fire below the whole-file cap: one
// long unrelated line must not fail the read when the file itself is well
// within maxPrefsFileBytes.
func TestALongUnrelatedLineDoesNotFailTheRead(t *testing.T) {
	dir := t.TempDir()
	body := "// " + strings.Repeat("x", 2<<20) + "\n" +
		`user_pref("extensions.zotero.sync.storage.protocol", "webdav");` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "prefs.js"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	fs, err := LoadProfile(dir)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if got := fs.Personal().Mode; got != StorageWebDAV {
		t.Fatalf("Personal().Mode = %q, want %q", got, StorageWebDAV)
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

// The hazard union is independent of which profile is chosen to describe the
// configuration. Choosing a representative by MODE alone would discard a
// different profile's positively decoded "file syncing is off" — the reported
// struct describes one profile, but any profile could be the running one.
func TestHazardsAccumulateAcrossProfilesIndependentlyOfTheRepresentative(t *testing.T) {
	mk := func(name, prefs string) string {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "prefs.js"), []byte(prefs), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	// A: Zotero cloud, personal syncing OFF. B: unrecognised protocol, so its
	// mode is unknown, which outranks cloud and makes B the representative.
	a := mk("a", `
user_pref("extensions.zotero.sync.storage.protocol", "zotero");
user_pref("extensions.zotero.sync.storage.enabled", false);
`)
	b := mk("b", `user_pref("extensions.zotero.sync.storage.protocol", "s3");`)

	fs, err := loadAcross([]string{a, b}, a)
	if err != nil {
		t.Fatalf("loadAcross: %v", err)
	}
	if got := fs.Personal().Mode; got != StorageUnknown {
		t.Fatalf("Personal().Mode = %q, want the riskier unknown representative", got)
	}
	if !fs.AnyPersonalSyncDisabled() {
		t.Fatal("AnyPersonalSyncDisabled() = false; profile A's decoded syncing-off evidence was discarded")
	}
	if !fs.AnyPersonalModeUnknown() {
		t.Fatal("AnyPersonalModeUnknown() = false, want profile B's undecodable protocol recorded")
	}
	if fs.AnyPersonalWebDAV() {
		t.Fatal("AnyPersonalWebDAV() = true with no WebDAV profile present")
	}
}

// Same failure on the group axis, and with no mode difference to mask it: the
// preferred profile wins the tie, so a sibling's disabled group syncing has to
// survive on its own.
func TestGroupSyncDisabledInASiblingProfileSurvivesTheModeTie(t *testing.T) {
	mk := func(name, prefs string) string {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "prefs.js"), []byte(prefs), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	a := mk("a", `user_pref("extensions.zotero.sync.storage.protocol", "zotero");`)
	b := mk("b", `user_pref("extensions.zotero.sync.storage.groups.enabled", false);`)

	fs, err := loadAcross([]string{a, b}, a)
	if err != nil {
		t.Fatalf("loadAcross: %v", err)
	}
	if fs.ProfilePath != a {
		t.Fatalf("ProfilePath = %q, want the preferred profile to win the mode tie", fs.ProfilePath)
	}
	if !fs.AnyGroupSyncDisabled() {
		t.Fatal("AnyGroupSyncDisabled() = false; the sibling's decoded evidence was lost to the tie")
	}
}

// An unreadable preferred profile must not suppress the sibling scan that
// exists to catch a WebDAV profile hiding behind it, and the failure itself
// must now be visible through the unreadable-profile accessors rather than
// vanishing silently.
func TestUnreadablePreferredProfileDoesNotHideAWebDAVSibling(t *testing.T) {
	root := t.TempDir()
	// A directory sitting where prefs.js should be is a genuine read failure
	// (not a regular file), unlike a simply-missing path, which LoadProfile
	// treats as benign absence rather than an unreadable profile.
	preferred := filepath.Join(root, "a")
	sibling := filepath.Join(root, "b")
	if err := os.MkdirAll(filepath.Join(preferred, "prefs.js"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "prefs.js"),
		[]byte(`user_pref("extensions.zotero.sync.storage.protocol", "webdav");`), 0o600); err != nil {
		t.Fatal(err)
	}

	fs, err := loadAcross([]string{preferred, sibling}, preferred)
	if err != nil {
		t.Fatalf("loadAcross: %v", err)
	}
	if !fs.AnyPersonalWebDAV() {
		t.Fatal("AnyPersonalWebDAV() = false; an unreadable default profile hid the WebDAV sibling")
	}
	// The sibling's own defaults must come through, not a zero struct.
	if !fs.GroupsEnabled {
		t.Fatal("GroupsEnabled = false, want the sibling's shipped default rather than a zero struct")
	}
	if !fs.AnyUnreadableProfile() {
		t.Fatal("AnyUnreadableProfile() = false, want the directory-shaped prefs.js counted as unreadable")
	}
	if got := fs.UnreadableProfileCount(); got != 1 {
		t.Fatalf("UnreadableProfileCount() = %d, want 1", got)
	}
}

// A profile directory that simply has no prefs.js is the ordinary case
// (Zotero has not necessarily run there), not an unreadable one.
func TestAbsentPrefsFileIsNotCountedAsUnreadable(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if err := os.MkdirAll(a, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(b, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "prefs.js"),
		[]byte(`user_pref("extensions.zotero.sync.storage.protocol", "webdav");`), 0o600); err != nil {
		t.Fatal(err)
	}

	fs, err := loadAcross([]string{a, b}, a)
	if err != nil {
		t.Fatalf("loadAcross: %v", err)
	}
	if fs.AnyUnreadableProfile() {
		t.Fatal("AnyUnreadableProfile() = true for a profile that simply has no prefs.js")
	}
	if got := fs.UnreadableProfileCount(); got != 0 {
		t.Fatalf("UnreadableProfileCount() = %d, want 0", got)
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
	roots, err := profileRoots()
	if err != nil {
		t.Fatalf("profileRoots: %v", err)
	}
	for _, root := range roots {
		if root == filepath.Join(home, "Zotero") {
			t.Fatal("profileRoots returned the Zotero DATA directory")
		}
	}
}

// Zotero's documented Linux layout puts the profile directory directly under
// ~/.zotero/zotero rather than inside a Profiles/ subfolder, unlike macOS and
// Windows. Discovery must scan the root itself on that platform, not only
// Profiles/.
func TestLinuxFallbackScansTheRootDirectlyNotOnlyProfilesSubfolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	setGOOS(t, "linux")

	profile := filepath.Join(home, ".zotero", "zotero", "abcdefgh.default")
	if err := os.MkdirAll(profile, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "prefs.js"),
		[]byte(`user_pref("extensions.zotero.sync.storage.protocol", "webdav");`), 0o600); err != nil {
		t.Fatal(err)
	}

	all, preferred, err := discoverProfiles()
	if err != nil {
		t.Fatalf("discoverProfiles: %v", err)
	}
	if len(all) != 1 || all[0] != profile {
		t.Fatalf("discoverProfiles all = %v, want [%s]", all, profile)
	}
	if preferred != profile {
		t.Fatalf("preferred = %q, want %q", preferred, profile)
	}
}

// A stale profiles.ini entry naming a directory that no longer exists must
// not suppress the directory scan that would otherwise find a live, unlisted
// profile.
func TestStaleINIEntryDoesNotSuppressALiveUnlistedProfile(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "Profiles", "bbbbbbbb.default")
	if err := os.MkdirAll(live, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "prefs.js"),
		[]byte(`user_pref("extensions.zotero.sync.storage.protocol", "webdav");`), 0o600); err != nil {
		t.Fatal(err)
	}
	// profiles.ini names a profile directory that was never created (renamed
	// or removed since the INI was last written) and marks it Default=1.
	ini := "[Profile0]\nIsRelative=1\nPath=Profiles/aaaaaaaa.gone\nDefault=1\n"
	if err := os.WriteFile(filepath.Join(root, "profiles.ini"), []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}

	all, preferred, err := profilesInRoot(root)
	if err != nil {
		t.Fatalf("profilesInRoot: %v", err)
	}
	if len(all) != 1 || all[0] != live {
		t.Fatalf("profilesInRoot all = %v, want [%s]", all, live)
	}
	if preferred != live {
		t.Fatalf("preferred = %q, want the live profile since the INI's declared default no longer exists", preferred)
	}
}

// Snap and Flatpak packaging sandbox $HOME for the Zotero process, so their
// profiles live under package-specific trees that discovery must also treat
// as candidates.
func TestSnapAndFlatpakRootsAreAdditionalCandidates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	setGOOS(t, "linux")

	snapProfile := filepath.Join(home, "snap", "zotero", "common", ".zotero", "zotero", "aaaaaaaa.default")
	flatpakProfile := filepath.Join(home, ".var", "app", "org.zotero.Zotero", ".zotero", "zotero", "bbbbbbbb.default")
	for _, dir := range []string{snapProfile, flatpakProfile} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "prefs.js"),
			[]byte(`user_pref("extensions.zotero.sync.storage.protocol", "webdav");`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	all, _, err := discoverProfiles()
	if err != nil {
		t.Fatalf("discoverProfiles: %v", err)
	}
	found := make(map[string]bool, len(all))
	for _, dir := range all {
		found[dir] = true
	}
	if !found[snapProfile] {
		t.Fatalf("discoverProfiles = %v, want the Snap profile included", all)
	}
	if !found[flatpakProfile] {
		t.Fatalf("discoverProfiles = %v, want the Flatpak profile included", all)
	}
}
