// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package zoteroprefs reads Zotero desktop's own profile preferences to learn
// where Zotero puts attachment FILES.
//
// This matters because Zotero's file storage is a client-side setting that the
// Zotero API does not report. A stored attachment uploaded through the Web
// API's file-upload protocol always lands in Zotero's own cloud storage
// ("zfs") and is billed against the account's storage plan — regardless of the
// desktop being configured to keep files on a personal WebDAV server. Reading
// the desktop's preference is the only way to notice that mismatch before
// consuming a plan the operator does not use.
//
// Only reads. Nothing here writes to a Zotero profile.
//
// # Two axes, not one
//
// Zotero models file storage as a MODE (Zotero's own storage vs WebDAV) and,
// separately, whether syncing is ENABLED for a library. LibraryStorage keeps
// them apart, mirroring Zotero.Sync.Storage.Local's getModeForLibrary and
// getEnabledForLibrary. Collapsing them loses the distinction between "files
// belong somewhere else" and "files are not synced at all", which are
// different problems with different remedies.
//
// # What this can and cannot prove
//
// Callers must treat a positive reading as evidence, never as proof:
//
//   - Zotero (a Firefox-based application) flushes prefs.js lazily, so the file
//     can lag the running application in EITHER direction.
//   - Zotero supports multiple profiles and Firefox's -P switch, so the profile
//     marked default in profiles.ini is not necessarily the running one. When
//     several profiles exist, Load reports Ambiguous and folds in every
//     profile's mode so the risky direction is not silently missed.
//
// An absent, ambiguous or undecodable value is reported as StorageUnknown
// rather than being defaulted to Zotero's cloud, because "unknown" is the only
// honest answer and callers key their safety decision on the difference.
package zoteroprefs

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// ProfileDirEnv overrides profile discovery, for operators with a
// non-standard Zotero install, for multi-profile setups where discovery cannot
// tell which profile is running, and for tests.
const ProfileDirEnv = "ZOTERO_PROFILE_DIR"

// Preference keys read from prefs.js. Zotero namespaces its preferences under
// "extensions.zotero.".
const (
	prefStorageProtocol      = "extensions.zotero.sync.storage.protocol"
	prefStorageEnabled       = "extensions.zotero.sync.storage.enabled"
	prefStorageGroupsEnabled = "extensions.zotero.sync.storage.groups.enabled"
	prefStorageURL           = "extensions.zotero.sync.storage.url"
	prefStorageVerified      = "extensions.zotero.sync.storage.verified"
)

// prefs.js is a small generated file. This bound keeps a corrupt or hostile
// profile from being read into memory unbounded; hitting it is reported as an
// error rather than silently parsing a prefix, since a truncated read would
// turn a present preference into an absent one.
const maxPrefsFileBytes = 8 << 20

// StorageMode names which file store Zotero uses for a library.
type StorageMode string

const (
	// StorageZoteroCloud is Zotero's own file storage ("zfs"), billed against
	// the account's storage plan. This is what a Web API file upload targets.
	StorageZoteroCloud StorageMode = "zotero"
	// StorageWebDAV is a personal WebDAV server. Zotero's own sync puts files
	// there; the Zotero Web API cannot.
	StorageWebDAV StorageMode = "webdav"
	// StorageUnknown means no readable profile was found, or a preference was
	// present but could not be decoded to a mode this package recognises.
	StorageUnknown StorageMode = "unknown"
)

// LibraryStorage is how Zotero handles attachment files for one library.
type LibraryStorage struct {
	// Mode is which file store Zotero uses.
	Mode StorageMode
	// Enabled reports whether Zotero syncs files for the library at all. A
	// library can be disabled while still having a mode: the two are separate
	// settings in Zotero and are kept separate here.
	Enabled bool
}

// FileStorage is Zotero desktop's attachment-file storage configuration.
type FileStorage struct {
	// ProfilePath is the profile the values came from. Empty when none was
	// found.
	ProfilePath string
	// Protocol is the decoded sync.storage.protocol value.
	Protocol string
	// ProtocolRecognised is false when a protocol preference was present but
	// decoded to something this package does not model. The mode is then
	// StorageUnknown rather than being assumed to be Zotero's cloud.
	ProtocolRecognised bool
	// Enabled mirrors sync.storage.enabled (personal-library file syncing).
	Enabled bool
	// GroupsEnabled mirrors sync.storage.groups.enabled.
	GroupsEnabled bool
	// URL is the configured WebDAV host and path. Zotero keeps the password in
	// the OS keychain, not prefs.js.
	URL string
	// Verified reports whether Zotero has verified the WebDAV server.
	Verified bool
	// Ambiguous is set when several Zotero profiles exist and discovery could
	// not establish which one is running.
	Ambiguous bool
	// ProfileCount is how many profiles discovery found.
	ProfileCount int
	// hazards are the safety-relevant facts observed across EVERY readable
	// profile, not just the represented one. The rest of this struct describes
	// a single profile so a message can name concrete values; the hazards are
	// the union, because -P means any profile might be the running one and a
	// mode-based choice of representative would otherwise discard a different
	// profile's positively decoded "syncing is off".
	hazards hazardSet

	found bool

	// unreadableProfiles lists the DISCOVERED profiles (see ProfileCount) that
	// failed to read for a reason other than simply not existing, even though
	// a different discovered profile could be read. Kept separate from
	// ProfileCount, which counts every profile discovery found regardless of
	// whether it could be evaluated. Paths, not a count, so a refusal can name
	// the file an operator has to go and fix.
	unreadableProfiles []string
}

// hazardSet is the union of safety-relevant facts across all readable
// profiles. Each field lists the profile directories that positively said
// this; absence of evidence never appends one.
//
// Attribution is load-bearing, not diagnostic decoration. The representative
// profile is chosen by riskRank, which orders MODES only, so a hazard drawn
// from Enabled/GroupsEnabled can come from a profile that is not the
// representative. Reporting a refusal against the representative would then
// name a profile whose settings contradict the message.
type hazardSet struct {
	personalWebDAV      []string
	personalSyncOff     []string
	groupSyncOff        []string
	personalModeUnknown []string
}

func (h hazardSet) or(other hazardSet) hazardSet {
	return hazardSet{
		personalWebDAV:      mergeSources(h.personalWebDAV, other.personalWebDAV),
		personalSyncOff:     mergeSources(h.personalSyncOff, other.personalSyncOff),
		groupSyncOff:        mergeSources(h.groupSyncOff, other.groupSyncOff),
		personalModeUnknown: mergeSources(h.personalModeUnknown, other.personalModeUnknown),
	}
}

// mergeSources appends without duplicating, keeping discovery order so the
// preferred profile is named first when it is itself a contributor.
func mergeSources(dst, src []string) []string {
	for _, s := range src {
		if !slices.Contains(dst, s) {
			dst = append(dst, s)
		}
	}
	return dst
}

// AnyPersonalWebDAV reports whether any readable profile keeps personal-library
// files on WebDAV.
func (f FileStorage) AnyPersonalWebDAV() bool { return len(f.hazards.personalWebDAV) > 0 }

// AnyPersonalSyncDisabled reports whether any readable profile has
// personal-library file syncing switched off.
func (f FileStorage) AnyPersonalSyncDisabled() bool { return len(f.hazards.personalSyncOff) > 0 }

// AnyGroupSyncDisabled reports whether any readable profile has group-library
// file syncing switched off.
func (f FileStorage) AnyGroupSyncDisabled() bool { return len(f.hazards.groupSyncOff) > 0 }

// AnyPersonalModeUnknown reports whether any readable profile's storage
// protocol could not be decoded to a mode this package models.
func (f FileStorage) AnyPersonalModeUnknown() bool { return len(f.hazards.personalModeUnknown) > 0 }

// PersonalWebDAVProfiles lists the profile directories that positively said
// personal-library files live on WebDAV.
func (f FileStorage) PersonalWebDAVProfiles() []string { return slices.Clone(f.hazards.personalWebDAV) }

// PersonalSyncDisabledProfiles lists the profile directories that positively
// said personal-library file syncing is off.
func (f FileStorage) PersonalSyncDisabledProfiles() []string {
	return slices.Clone(f.hazards.personalSyncOff)
}

// GroupSyncDisabledProfiles lists the profile directories that positively said
// group-library file syncing is off.
func (f FileStorage) GroupSyncDisabledProfiles() []string {
	return slices.Clone(f.hazards.groupSyncOff)
}

// PersonalModeUnknownProfiles lists the profile directories whose storage
// protocol could not be decoded.
func (f FileStorage) PersonalModeUnknownProfiles() []string {
	return slices.Clone(f.hazards.personalModeUnknown)
}

// AnyUnreadableProfile reports whether at least one discovered profile could
// not be evaluated — a permission error, a corrupt file, an oversized file —
// even though a different discovered profile was readable. A profile
// directory that simply has no prefs.js is not unreadable; Zotero has not
// necessarily run there yet, and that is the ordinary case Found() already
// handles.
func (f FileStorage) AnyUnreadableProfile() bool { return len(f.unreadableProfiles) > 0 }

// UnreadableProfileCount reports how many discovered profiles could not be
// evaluated.
func (f FileStorage) UnreadableProfileCount() int { return len(f.unreadableProfiles) }

// UnreadableProfiles lists the discovered profiles that could not be
// evaluated, so a refusal can name the exact path rather than only a count.
func (f FileStorage) UnreadableProfiles() []string { return slices.Clone(f.unreadableProfiles) }

// Found reports whether a readable Zotero desktop profile was located.
func (f FileStorage) Found() bool { return f.found }

// Personal returns how Zotero handles files for the personal library. Mirrors
// getModeForLibrary for a 'user' library: webdav iff sync.storage.protocol is
// "webdav", else Zotero's own storage.
func (f FileStorage) Personal() LibraryStorage {
	if !f.found {
		return LibraryStorage{Mode: StorageUnknown}
	}
	return LibraryStorage{Mode: f.mode(), Enabled: f.Enabled}
}

// Group returns how Zotero handles files for group libraries. Zotero always
// uses its own storage for groups — WebDAV is a personal-library setting — so
// the mode is never StorageWebDAV.
func (f FileStorage) Group() LibraryStorage {
	if !f.found {
		return LibraryStorage{Mode: StorageUnknown}
	}
	return LibraryStorage{Mode: StorageZoteroCloud, Enabled: f.GroupsEnabled}
}

func (f FileStorage) mode() StorageMode {
	if !f.ProtocolRecognised {
		return StorageUnknown
	}
	if f.Protocol == "webdav" {
		return StorageWebDAV
	}
	return StorageZoteroCloud
}

// Describe renders a short human description of a mode, naming the WebDAV host
// when there is one.
func (f FileStorage) Describe(mode StorageMode) string {
	switch mode {
	case StorageWebDAV:
		if host := f.WebDAVHost(); host != "" {
			return "WebDAV (" + host + ")"
		}
		return "WebDAV"
	case StorageZoteroCloud:
		return "Zotero cloud storage"
	default:
		return "unknown"
	}
}

// WebDAVHost returns the host portion of the configured WebDAV URL. Zotero
// stores this without a scheme (for example "example.com/zotero").
func (f FileStorage) WebDAVHost() string {
	raw := strings.TrimSpace(f.URL)
	if raw == "" {
		return ""
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	}
	// Strip credentials BEFORE splitting off the path. Doing it the other way
	// round truncates at the first /?# — which may be inside a hand-typed
	// password — discarding the '@' along with the host and returning the
	// credential prefix itself. This value reaches doctor output and the
	// refusal envelope, so leaking it is not acceptable.
	if i := strings.LastIndex(raw, "@"); i >= 0 {
		raw = raw[i+1:]
	}
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	// A prefs.js value is not trusted display text: decoded \u escapes can
	// carry C0 controls that would rewrite the terminal line this is reported
	// on. Keep only printable characters.
	return strings.Map(func(r rune) rune {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, raw)
}

// Load discovers Zotero desktop's profiles and reads the file-storage
// configuration.
//
// A missing Zotero installation is not an error: zotio is routinely run
// against the Web API on machines with no desktop. The result then reports
// Found() == false and StorageUnknown, which callers must not treat as
// permission to assume Zotero's cloud.
//
// When several profiles exist and none is pinned by ProfileDirEnv, Zotero's -P
// switch means the profile marked default in profiles.ini may not be the
// running one. Rather than guess, Load reads them all: the default profile
// supplies the reported detail, but any profile using WebDAV promotes the mode
// to WebDAV and sets Ambiguous. That keeps the dangerous direction — assuming
// Zotero's cloud when the running profile actually uses WebDAV — closed.
func Load() (FileStorage, error) {
	if override := strings.TrimSpace(os.Getenv(ProfileDirEnv)); override != "" {
		fs, err := LoadProfile(override)
		if err != nil {
			return FileStorage{}, err
		}
		// An explicit pin is a positive assertion. Resolving it to nothing is
		// operator error, not absence of evidence, and reporting it as the
		// latter would silently disable the guard for every later invocation.
		if !fs.Found() {
			return FileStorage{}, fmt.Errorf("%s points at %s, which has no readable prefs.js", ProfileDirEnv, override)
		}
		return fs, nil
	}
	dirs, preferred, err := discoverProfiles()
	if err != nil {
		return FileStorage{}, err
	}
	if len(dirs) == 0 {
		return FileStorage{}, nil
	}
	return loadAcross(dirs, preferred)
}

// LoadAcrossForTest exposes multi-profile folding to tests in other
// packages, which otherwise cannot construct an ambiguous, multi-profile
// reading without hand-setting fields — exactly the shortcut that let a
// previous guard test pass without ever exercising a real read failure. It
// is not test-file-scoped because it is called from package cli's tests.
func LoadAcrossForTest(dirs []string, preferred string) (FileStorage, error) {
	return loadAcross(dirs, preferred)
}

// loadAcross reads every discovered profile and reports one representative
// reading plus the UNION of the safety-relevant facts across all of them.
//
// Those are deliberately two different things. Zotero's -P switch means any
// profile might be the running one, so a hazard observed anywhere has to count;
// but a message needs concrete values, and merging fields from several profiles
// would describe a configuration that exists nowhere. So the representative is
// chosen by riskiest mode (WebDAV beats unknown, unknown beats Zotero's cloud,
// preferred wins ties) while hazards accumulate independently — otherwise a
// profile positively saying "file syncing is off" would be discarded merely
// because a different profile had a riskier MODE.
//
// A profile that fails to read is skipped when choosing the representative,
// INCLUDING the preferred one: aborting there would suppress the very sibling
// scan that exists to catch a WebDAV profile hiding behind an unreadable
// default. The error is returned only when no profile could be read at all;
// otherwise the failure is recorded as an unreadable-profile hazard so the
// caller can see the reading is incomplete rather than treating it as clean.
func loadAcross(dirs []string, preferred string) (FileStorage, error) {
	var (
		best       FileStorage
		haveBest   bool
		hazards    hazardSet
		firstErr   error
		unreadable []string
	)
	consider := func(dir string) {
		fs, err := LoadProfile(dir)
		if err != nil {
			// LoadProfile already collapses a simply-missing profile into
			// (zero value, nil), so any error reaching here is a genuine
			// read failure — permission denied, a corrupt or oversized
			// file, an unsupported encoding — never a benign absence.
			if firstErr == nil {
				firstErr = err
			}
			unreadable = mergeSources(unreadable, []string{dir})
			return
		}
		if !fs.found {
			return
		}
		hazards = hazards.or(fs.hazards)
		if !haveBest || riskRank(fs.mode()) > riskRank(best.mode()) {
			best, haveBest = fs, true
		}
	}
	// Preferred first, so it wins ties: siblings replace it only when strictly
	// riskier.
	consider(preferred)
	for _, dir := range dirs {
		if dir != preferred {
			consider(dir)
		}
	}

	if !haveBest {
		if firstErr != nil {
			// Every discovered profile failed to read. This is the case where
			// naming the paths matters most - the operator has no other way to
			// learn which file to fix - so the error travels WITH the evidence
			// rather than instead of it. found stays false, so no caller can
			// mistake this for a successful reading.
			return FileStorage{unreadableProfiles: unreadable}, firstErr
		}
		return FileStorage{}, nil
	}
	best.ProfileCount = len(dirs)
	best.Ambiguous = len(dirs) > 1
	best.hazards = hazards
	best.unreadableProfiles = unreadable
	return best, nil
}

// riskRank orders storage modes by how strongly they argue against uploading
// through the Web API. Unknown outranks Zotero's cloud because an undecodable
// profile must not be reported as a positive cloud reading.
func riskRank(m StorageMode) int {
	switch m {
	case StorageWebDAV:
		return 2
	case StorageUnknown:
		return 1
	default:
		return 0
	}
}

// LoadProfile reads file-storage configuration from one profile directory.
func LoadProfile(profileDir string) (FileStorage, error) {
	path := filepath.Join(profileDir, "prefs.js")
	// Only a regular file is read. os.Open on a FIFO blocks in open(2) until a
	// writer appears, which no read limit can bound, and the guard is consulted
	// on every mutating invocation — a hung zotio is worse than an unread
	// profile. The residual stat/open race is benign: the worst case is the
	// behaviour we would have had anyway.
	// #nosec G703 -- same taint source and read-only use as the Open below.
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FileStorage{}, nil
		}
		return FileStorage{}, fmt.Errorf("reading Zotero prefs %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return FileStorage{}, fmt.Errorf("Zotero prefs %s is not a regular file (%s)", path, info.Mode().Type())
	}
	// #nosec G304,G703 -- profileDir comes from platform discovery or the
	// operator's own ZOTERO_PROFILE_DIR, and this only opens it for reading.
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FileStorage{}, nil
		}
		return FileStorage{}, fmt.Errorf("reading Zotero prefs %s: %w", path, err)
	}
	defer f.Close()

	// Read one byte past the cap so truncation is detectable: silently parsing
	// a prefix would turn a present preference into an absent one, and absent
	// preferences fall back to Zotero's cloud defaults.
	data, err := io.ReadAll(io.LimitReader(f, maxPrefsFileBytes+1))
	if err != nil {
		return FileStorage{}, fmt.Errorf("reading Zotero prefs %s: %w", path, err)
	}
	if len(data) > maxPrefsFileBytes {
		return FileStorage{}, fmt.Errorf("Zotero prefs %s exceeds %d bytes; refusing to read a partial preference set", path, maxPrefsFileBytes)
	}
	// Zotero (a Firefox-based application) always writes prefs.js as UTF-8. A
	// differently encoded file — most plausibly UTF-16, from a profile a
	// stray tool has touched — makes every "user_pref(" prefix match fail,
	// since that ASCII byte sequence never starts a UTF-16 line. Every
	// preference would then read as absent, which resolves to Zotero's cloud
	// defaults: a confident wrong answer built from a decode failure, not
	// from evidence. Surfacing the encoding mismatch as an error instead lets
	// the caller treat it as the evaluation failure it is.
	if reason := notUTF8Reason(data); reason != "" {
		return FileStorage{}, fmt.Errorf("Zotero prefs %s is not readable as UTF-8 (%s)", path, reason)
	}

	values, err := parsePrefs(bytes.NewReader(data))
	if err != nil {
		return FileStorage{}, fmt.Errorf("parsing Zotero prefs %s: %w", path, err)
	}

	// Defaults come from Zotero's defaults/preferences/zotero.js: file syncing
	// is on and the protocol is Zotero's own storage. prefs.js records only
	// values that differ from those defaults, so an absent key means default.
	fs := FileStorage{
		ProfilePath:        profileDir,
		Protocol:           "zotero",
		ProtocolRecognised: true,
		Enabled:            true,
		GroupsEnabled:      true,
		ProfileCount:       1,
		found:              true,
	}
	// A key that is PRESENT but could not be decoded to the type it should
	// hold is different from an absent one: an absent key means Zotero's
	// shipped default genuinely applies, but a present, malformed value means
	// the real configuration is unknown. Both booleans keep their shipped
	// default in that case — never collapsing an undecodable value to false,
	// which would manufacture a "syncing is off" refusal from no evidence —
	// while the malformed flags feed AnyPersonalModeUnknown so the caller
	// knows the reading is not a confident one.
	var enabledMalformed, groupsEnabledMalformed bool
	if v, ok := values[prefStorageProtocol]; ok {
		fs.Protocol = strings.ToLower(strings.TrimSpace(v.str))
		// An unrecognised or undecodable protocol must not be read as
		// "therefore Zotero's cloud": that is the fail-open direction.
		// Report it as unknown and let the caller decide.
		fs.ProtocolRecognised = !v.undecodable && (fs.Protocol == "zotero" || fs.Protocol == "webdav")
	}
	if v, ok := values[prefStorageEnabled]; ok {
		if b, bok := v.asBool(); bok {
			fs.Enabled = b
		} else {
			enabledMalformed = true
		}
	}
	if v, ok := values[prefStorageGroupsEnabled]; ok {
		if b, bok := v.asBool(); bok {
			fs.GroupsEnabled = b
		} else {
			groupsEnabledMalformed = true
		}
	}
	if v, ok := values[prefStorageURL]; ok {
		fs.URL = v.str
	}
	if v, ok := values[prefStorageVerified]; ok {
		if b, bok := v.asBool(); bok {
			fs.Verified = b
		}
	}
	// Each profile contributes its own positively decoded hazards, attributed
	// to itself; loadAcross unions them so a hazard in one profile is never
	// lost by choosing another profile as the representative, and never
	// reported against a profile that did not evidence it.
	self := []string{profileDir}
	fs.hazards = hazardSet{}
	if fs.mode() == StorageWebDAV {
		fs.hazards.personalWebDAV = self
	}
	if !fs.Enabled {
		fs.hazards.personalSyncOff = self
	}
	if !fs.GroupsEnabled {
		fs.hazards.groupSyncOff = self
	}
	if fs.mode() == StorageUnknown || enabledMalformed || groupsEnabledMalformed {
		fs.hazards.personalModeUnknown = self
	}
	return fs, nil
}

// goos selects Zotero's per-platform profile layout. A package variable
// rather than a direct runtime.GOOS reference so tests can drive the
// Linux/Unix discovery path — including the Snap and Flatpak candidates —
// without needing to run on Linux.
var goos = runtime.GOOS

// discoverProfiles returns every profile directory it can see across every
// platform root plus the one to prefer for reporting (profiles.ini's
// Default=1 in whichever root it is found, else the first profile
// discovered). Snap and Flatpak installs redirect Zotero's view of $HOME, so
// more than one root can plausibly hold profiles on the same machine; results
// are merged and deduplicated by cleaned absolute path rather than stopping at
// the first root that exists.
func discoverProfiles() (all []string, preferred string, err error) {
	roots, err := profileRoots()
	if err != nil {
		return nil, "", err
	}
	seen := make(map[string]bool)
	for _, root := range roots {
		info, statErr := os.Stat(root)
		if statErr != nil || !info.IsDir() {
			continue
		}
		rootAll, rootPreferred, rErr := profilesInRoot(root)
		if rErr != nil {
			return nil, "", rErr
		}
		for _, dir := range rootAll {
			clean := filepath.Clean(dir)
			if seen[clean] {
				continue
			}
			seen[clean] = true
			all = append(all, dir)
		}
		if preferred == "" {
			preferred = rootPreferred
		}
	}
	if len(all) == 0 {
		return nil, "", nil
	}
	if preferred == "" {
		preferred = all[0]
	}
	return all, preferred, nil
}

// profilesInRoot returns every profile directory discoverable under one
// platform root, plus which one profiles.ini marks Default=1 (or, failing
// that, the first profile found).
//
// profiles.ini is authoritative when it names a profile that still exists on
// disk, but a stale entry — the profile directory renamed or removed since
// the INI was last written — must not suppress the directory scan that would
// otherwise find a live, unlisted profile; Zotero does not require
// profiles.ini to be reconciled with what is actually on disk. So both
// sources are gathered and merged, deduplicated by cleaned absolute path,
// rather than the INI short-circuiting the fallback whenever it parses.
func profilesInRoot(root string) (all []string, preferred string, err error) {
	iniAll, iniPreferred, err := profilesFromINI(filepath.Join(root, "profiles.ini"), root)
	if err != nil {
		return nil, "", err
	}

	bases := profileFallbackBases(root)
	// A base is a container of profiles, never a profile. On Linux the root is
	// scanned directly (profiles live at ~/.zotero/zotero/<name>), so a
	// sibling Profiles/ folder would otherwise be discovered as a profile in
	// its own right — inflating ProfileCount, making the reading look
	// Ambiguous, and attaching a directory nobody configured to every refusal.
	container := make(map[string]bool, len(bases))
	for _, base := range bases {
		container[filepath.Clean(base)] = true
	}

	seen := make(map[string]bool, len(iniAll))
	addUsable := func(dir string) {
		if !profileDirLooksUsable(dir) {
			return
		}
		clean := filepath.Clean(dir)
		if seen[clean] || container[clean] {
			return
		}
		seen[clean] = true
		all = append(all, dir)
	}
	for _, dir := range iniAll {
		addUsable(dir)
	}
	if iniPreferred != "" && profileDirLooksUsable(iniPreferred) {
		preferred = iniPreferred
	}

	for _, base := range bases {
		dirs, dErr := profileDirs(base)
		if dErr != nil {
			return nil, "", dErr
		}
		for _, dir := range dirs {
			addUsable(dir)
		}
	}

	if preferred == "" && len(all) > 0 {
		preferred = all[0]
	}
	return all, preferred, nil
}

// profileDirLooksUsable reports whether dir is worth treating as a profile
// candidate. A stale profiles.ini entry naming a directory that no longer
// exists must not be accepted just because the INI parsed cleanly —
// LoadProfile will read prefs.js there anyway, and a genuinely missing
// directory is never a signal, just leftover configuration.
func profileDirLooksUsable(dir string) bool {
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// profileFallbackBases lists the directories, under one platform root, that a
// directory scan should treat as potentially holding profile folders
// directly. Every platform gets the standard Profiles/ subfolder; Linux and
// other Unix systems additionally get the root itself, because Zotero's
// documented Linux layout puts the profile directory directly under
// ~/.zotero/zotero rather than inside a Profiles/ subfolder the way macOS and
// Windows do.
func profileFallbackBases(root string) []string {
	bases := []string{filepath.Join(root, "Profiles")}
	switch goos {
	case "darwin", "windows":
	default:
		bases = append(bases, root)
	}
	return bases
}

// profileRoots returns every directory this platform might hold a Zotero
// profiles.ini or Profiles/ folder under, in priority order. Zotero's DATA
// directory (~/Zotero by default) is deliberately not a candidate: it is a
// different thing from the profile directory and must not be probed for
// prefs.js.
//
// Snap and Flatpak packaging sandbox $HOME for the Zotero process, so a Snap
// or Flatpak install's profile lives under a package-specific tree instead of
// the native ~/.zotero/zotero; more than one of these can exist on the same
// machine (one per install method actually used), so all are candidates
// rather than the first that exists winning outright.
func profileRoots() ([]string, error) {
	switch goos {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolving home dir: %w", err)
		}
		return []string{filepath.Join(home, "Library", "Application Support", "Zotero")}, nil
	case "windows":
		var roots []string
		if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
			roots = append(roots, filepath.Join(appData, "Zotero", "Zotero"))
		}
		return roots, nil
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolving home dir: %w", err)
		}
		return []string{
			filepath.Join(home, ".zotero", "zotero"),
			filepath.Join(home, "snap", "zotero", "common", ".zotero", "zotero"),
			filepath.Join(home, ".var", "app", "org.zotero.Zotero", ".zotero", "zotero"),
		}, nil
	}
}

// profilesFromINI lists every profile in profiles.ini and reports which one is
// marked Default=1.
func profilesFromINI(iniPath, root string) (all []string, preferred string, err error) {
	f, err := os.Open(iniPath) // #nosec G304 -- platform Zotero profile root, read-only.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("reading Zotero profiles.ini %s: %w", iniPath, err)
	}
	defer f.Close()

	type profile struct {
		path       string
		isRelative bool
		isDefault  bool
	}
	var profiles []profile
	inProfile := false
	cur := profile{isRelative: true}
	flush := func() {
		if inProfile && cur.path != "" {
			profiles = append(profiles, cur)
		}
		cur = profile{isRelative: true}
	}

	scanner := bufio.NewScanner(io.LimitReader(f, maxPrefsFileBytes))
	// profiles.ini lines are short; raise the token cap only so a pathological
	// single line fails the same bounded way prefs.js does rather than with the
	// default 64 KiB limit.
	scanner.Buffer(make([]byte, 0, 8<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(stripBOM(scanner.Text()))
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			flush()
			inProfile = strings.HasPrefix(line, "[Profile")
			continue
		}
		if !inProfile {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch strings.ToLower(key) {
		case "path":
			cur.path = value
		case "isrelative":
			cur.isRelative = value != "0"
		case "default":
			cur.isDefault = value == "1"
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, "", fmt.Errorf("reading Zotero profiles.ini %s: %w", iniPath, err)
	}

	resolve := func(p profile) string {
		if p.isRelative {
			return filepath.Join(root, filepath.FromSlash(p.path))
		}
		return filepath.FromSlash(p.path)
	}
	for _, p := range profiles {
		dir := resolve(p)
		all = append(all, dir)
		if p.isDefault && preferred == "" {
			preferred = dir
		}
	}
	return all, preferred, nil
}

// profileDirs lists the directories under a Profiles/ folder.
func profileDirs(profilesDir string) ([]string, error) {
	entries, err := os.ReadDir(profilesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading Zotero profiles dir %s: %w", profilesDir, err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(profilesDir, e.Name()))
		}
	}
	return dirs, nil
}

// prefValue is one decoded user_pref argument. Zotero writes strings, booleans
// and integers; the raw string form is kept for string prefs.
type prefValue struct {
	str    string
	isBool bool
	b      bool
	isNum  bool
	n      int64
	// undecodable marks a value that was present but could not be decoded,
	// which callers must distinguish from an absent preference.
	undecodable bool
}

// asBool decodes v as a boolean pref, accepting a genuine boolean token or an
// integer — Zotero's own preferences system treats a non-zero integer as
// true. Any other decoded type, most notably a string, is a type mismatch:
// the writer never stores a boolean that way, so treating a stray string as
// evidence of true or false would invent a reading from the wrong kind of
// value rather than report one this package actually observed.
func (v prefValue) asBool() (b bool, ok bool) {
	switch {
	case v.undecodable:
		return false, false
	case v.isBool:
		return v.b, true
	case v.isNum:
		return v.n != 0, true
	default:
		return false, false
	}
}

// parsePrefs reads the user_pref("key", value); lines of a prefs.js, returning
// the decoded values. A line that is not a well-formed call is skipped, but a
// well-formed call whose value cannot be decoded is recorded as undecodable
// rather than dropped: silently dropping it would present a configured
// preference as absent, and absent preferences fall back to Zotero's cloud
// defaults.
func parsePrefs(r io.Reader) (map[string]prefValue, error) {
	values := make(map[string]prefValue, 16)
	scanner := bufio.NewScanner(r)
	// The caller has already bounded the whole file to maxPrefsFileBytes, so
	// the per-line buffer only needs to reach that same size: a lower cap here
	// would let one long unrelated line fail the read even though the file as
	// a whole is within bounds.
	scanner.Buffer(make([]byte, 0, 64<<10), maxPrefsFileBytes)
	first := true
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			line = stripBOM(line)
			first = false
		}
		line = strings.TrimSpace(line)
		const call = "user_pref("
		if !strings.HasPrefix(line, call) {
			continue
		}
		key, rest, ok := cutQuotedString(line[len(call):])
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, ",") {
			continue
		}
		values[key] = parsePrefValue(strings.TrimSpace(strings.TrimPrefix(rest, ",")))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

// parsePrefValue decodes the value argument of a user_pref call: everything
// after the key's trailing comma, up to end of line. That text still carries
// the call's own closing paren and whatever the writer put after it, and both
// are checked, not just discarded — a value only counts as decoded when
// nothing but the call's own framing (a ")", an optional ";", an optional
// trailing "//" comment) follows it. Anything else, such as string
// concatenation the writer never emits, means the line does not actually
// carry the value it appears to at a glance, and reporting a truncated prefix
// as decoded would turn writer-controlled text into evidence this package
// never really observed.
func parsePrefValue(s string) prefValue {
	if b, tail, ok := cutBoolLiteral(s); ok && validCallTail(tail) {
		return prefValue{isBool: true, b: b}
	}
	if n, tail, ok := cutIntLiteral(s); ok && validCallTail(tail) {
		return prefValue{isNum: true, n: n}
	}
	if str, tail, ok := cutQuotedString(s); ok && validCallTail(tail) {
		return prefValue{str: str}
	}
	return prefValue{str: s, undecodable: true}
}

// cutBoolLiteral matches a literal true/false keyword at the start of s and
// returns what follows it. Zotero only ever writes the bare JavaScript
// keyword for a boolean pref, never a quoted "true", so a quoted string is
// deliberately not accepted here — that would make a type mismatch pass as a
// genuine boolean reading.
func cutBoolLiteral(s string) (b bool, tail string, ok bool) {
	if rest, has := strings.CutPrefix(s, "true"); has {
		return true, rest, true
	}
	if rest, has := strings.CutPrefix(s, "false"); has {
		return false, rest, true
	}
	return false, "", false
}

// cutIntLiteral matches a leading base-10 integer literal in s and returns
// what follows it.
func cutIntLiteral(s string) (n int64, tail string, ok bool) {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return 0, "", false
	}
	v, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		return 0, "", false
	}
	return v, s[i:], true
}

// validCallTail reports whether s is everything a genuine user_pref(...) call
// may still carry after its value: the call's own closing paren, an optional
// semicolon, and an optional trailing "//" comment running to end of line.
// Anything else means the writer put more into the value position than a
// clean decode captured, so the decode must not be trusted.
func validCallTail(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, ")") {
		return false
	}
	s = strings.TrimSpace(s[1:])
	s = strings.TrimPrefix(s, ";")
	s = strings.TrimSpace(s)
	return s == "" || strings.HasPrefix(s, "//")
}

// stripBOM removes a leading UTF-8 byte order mark.
func stripBOM(s string) string { return strings.TrimPrefix(s, "\ufeff") }

// notUTF8Reason reports why data cannot be the UTF-8 text Zotero always
// writes prefs.js as, or "" when it can. A BOM identifies the two encodings a
// stray tool would plausibly leave behind; an embedded NUL byte or any other
// invalid UTF-8 sequence catches the rest, since genuine prefs.js text is
// generated JavaScript source that never contains one.
func notUTF8Reason(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte{0xff, 0xfe}):
		return "UTF-16LE byte order mark"
	case bytes.HasPrefix(data, []byte{0xfe, 0xff}):
		return "UTF-16BE byte order mark"
	case bytes.IndexByte(data, 0) >= 0:
		return "embedded NUL byte"
	case !utf8.Valid(data):
		return "invalid UTF-8"
	}
	return ""
}

// cutQuotedString decodes a leading double-quoted JavaScript string literal and
// returns it with the remainder of the input.
//
// The escape set is the one the preferences writer emits — \\, \", \n, \r, \t
// and \uXXXX — plus the remaining single-character C escapes for robustness.
// Decoding \u properly matters: dropping the backslash would turn
// "webd\u0061v" into a value that does not compare equal to "webdav", which
// would silently downgrade a WebDAV profile to Zotero's cloud.
func cutQuotedString(s string) (string, string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, `"`) {
		return "", "", false
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			if i+1 >= len(s) {
				return "", "", false
			}
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'v':
				b.WriteByte('\v')
			case '0':
				b.WriteByte(0)
			case 'x':
				r, next, ok := decodeHexEscape(s, i+1, 2)
				if !ok {
					return "", "", false
				}
				b.WriteRune(r)
				i = next - 1
			case 'u':
				r, next, ok := decodeUnicodeEscape(s, i+1)
				if !ok {
					return "", "", false
				}
				b.WriteRune(r)
				i = next - 1
			default:
				b.WriteByte(s[i])
			}
		case '"':
			return b.String(), s[i+1:], true
		default:
			b.WriteByte(c)
		}
	}
	return "", "", false
}

// decodeUnicodeEscape decodes \uXXXX (and a \uXXXX\uXXXX surrogate pair)
// starting at the character after the 'u'. It returns the rune and the index
// just past the escape.
func decodeUnicodeEscape(s string, i int) (rune, int, bool) {
	r, next, ok := decodeHexEscape(s, i, 4)
	if !ok {
		return 0, 0, false
	}
	if utf16.IsSurrogate(r) {
		// A lone surrogate is not encodable; pair it when the low half follows.
		if next+1 < len(s) && s[next] == '\\' && s[next+1] == 'u' {
			if v2, next2, ok2 := decodeHexEscape(s, next+2, 4); ok2 {
				if combined := utf16.DecodeRune(r, v2); combined != utf8.RuneError {
					return combined, next2, true
				}
			}
		}
		return utf8.RuneError, next, true
	}
	return r, next, true
}

// decodeHexEscape reads exactly n hex digits starting at i and returns them as
// a rune. n is at most 4 here, so the value cannot exceed 0xFFFF, but the
// range is checked explicitly rather than assumed.
func decodeHexEscape(s string, i, n int) (rune, int, bool) {
	if i+n > len(s) {
		return 0, 0, false
	}
	v, err := strconv.ParseUint(s[i:i+n], 16, 32)
	if err != nil || v > unicode.MaxRune {
		return 0, 0, false
	}
	return rune(v), i + n, true
}
