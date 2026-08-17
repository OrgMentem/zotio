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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
	// not establish which one is running. The reported mode then folds in every
	// profile, so a WebDAV profile is never missed just because a different
	// profile is marked default.
	Ambiguous bool
	// ProfileCount is how many profiles discovery found.
	ProfileCount int

	found bool
}

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

// loadAcross reads every discovered profile and reports the one that argues
// most strongly against a Web API upload.
//
// Zotero's -P switch means the profile marked default may not be the running
// one, so the riskiest reading wins: WebDAV beats unknown, unknown beats
// Zotero's cloud, and the preferred profile wins ties. The whole struct is
// taken from a single profile rather than merging fields from several, so the
// reported configuration is always one that actually exists somewhere.
//
// A profile that fails to read is skipped, INCLUDING the preferred one:
// aborting there would suppress the very sibling scan that exists to catch a
// WebDAV profile hiding behind an unreadable default. The error is returned
// only when no profile could be read at all.
func loadAcross(dirs []string, preferred string) (FileStorage, error) {
	var (
		best     FileStorage
		haveBest bool
		firstErr error
	)
	consider := func(dir string) {
		fs, err := LoadProfile(dir)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return
		}
		if !fs.found {
			return
		}
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
			return FileStorage{}, firstErr
		}
		return FileStorage{}, nil
	}
	best.ProfileCount = len(dirs)
	best.Ambiguous = len(dirs) > 1
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
	// preferences fall back to Zotero's cloud defaults. The byte total is
	// measured by counting what the reader actually consumes — reconstructing
	// it from token lengths miscounts, because bufio.ScanLines strips the \r of
	// a CRLF line and drops the missing final newline of an unterminated file.
	counter := &countingReader{r: io.LimitReader(f, maxPrefsFileBytes+1)}
	values, err := parsePrefs(counter)
	if err != nil {
		return FileStorage{}, fmt.Errorf("parsing Zotero prefs %s: %w", path, err)
	}
	if counter.n > maxPrefsFileBytes {
		return FileStorage{}, fmt.Errorf("Zotero prefs %s exceeds %d bytes; refusing to read a partial preference set", path, maxPrefsFileBytes)
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
	if v, ok := values[prefStorageProtocol]; ok {
		fs.Protocol = strings.ToLower(strings.TrimSpace(v.str))
		// An unrecognised protocol must not be read as "therefore Zotero's
		// cloud": that is the fail-open direction. Report it as unknown and let
		// the caller decide.
		fs.ProtocolRecognised = !v.undecodable && (fs.Protocol == "zotero" || fs.Protocol == "webdav")
	}
	// An undecodable boolean keeps the shipped default. truthy() reads a
	// garbled value as false, which would turn a parse failure into a
	// "syncing is off" refusal — a wrong answer invented from no evidence.
	if v, ok := values[prefStorageEnabled]; ok && !v.undecodable {
		fs.Enabled = v.truthy()
	}
	if v, ok := values[prefStorageGroupsEnabled]; ok && !v.undecodable {
		fs.GroupsEnabled = v.truthy()
	}
	if v, ok := values[prefStorageURL]; ok {
		fs.URL = v.str
	}
	if v, ok := values[prefStorageVerified]; ok && !v.undecodable {
		fs.Verified = v.truthy()
	}
	return fs, nil
}

// countingReader records how many bytes were actually consumed.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// DiscoverProfileDir returns the profile discovery would read from, or "" when
// Zotero is not installed for this user.
func DiscoverProfileDir() (string, error) {
	if override := strings.TrimSpace(os.Getenv(ProfileDirEnv)); override != "" {
		return override, nil
	}
	_, preferred, err := discoverProfiles()
	return preferred, err
}

// discoverProfiles returns every profile directory it can see plus the one to
// prefer for reporting (profiles.ini's Default=1, else the only one).
func discoverProfiles() (all []string, preferred string, err error) {
	root, err := profileRoot()
	if err != nil || root == "" {
		return nil, "", err
	}
	all, preferred, err = profilesFromINI(filepath.Join(root, "profiles.ini"), root)
	if err != nil {
		return nil, "", err
	}
	if len(all) > 0 {
		if preferred == "" {
			preferred = all[0]
		}
		return all, preferred, nil
	}
	// No usable profiles.ini: fall back to the directories under Profiles/ so a
	// hand-copied or partially migrated install still reports its configuration.
	all, err = profileDirs(filepath.Join(root, "Profiles"))
	if err != nil || len(all) == 0 {
		return nil, "", err
	}
	return all, all[0], nil
}

// profileRoot returns the directory holding profiles.ini for this platform, or
// "" when it does not exist. Zotero's DATA directory (~/Zotero by default) is
// deliberately not a candidate: it is a different thing from the profile
// directory and must not be probed for prefs.js.
func profileRoot() (string, error) {
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home dir: %w", err)
		}
		candidates = append(candidates, filepath.Join(home, "Library", "Application Support", "Zotero"))
	case "windows":
		if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
			candidates = append(candidates, filepath.Join(appData, "Zotero", "Zotero"))
		}
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home dir: %w", err)
		}
		candidates = append(candidates, filepath.Join(home, ".zotero", "zotero"))
	}
	for _, dir := range candidates {
		// #nosec G703 -- candidates are fixed per-platform Zotero locations built
		// from os.UserHomeDir/APPDATA, never from user input, and this only stats
		// them. The one caller-supplied path is ZOTERO_PROFILE_DIR, handled in Load.
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, nil
		}
	}
	return "", nil
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

// truthy reports a boolean pref, treating a non-zero integer as true the way
// the preferences system does.
func (v prefValue) truthy() bool {
	switch {
	case v.isBool:
		return v.b
	case v.isNum:
		return v.n != 0
	default:
		return strings.EqualFold(strings.TrimSpace(v.str), "true")
	}
}

// parsePrefs reads the user_pref("key", value); lines of a prefs.js, returning
// the decoded values and the number of bytes consumed. A line that is not a
// well-formed call is skipped, but a well-formed call whose value cannot be
// decoded is recorded as undecodable rather than dropped: silently dropping it
// would present a configured preference as absent, and absent preferences fall
// back to Zotero's cloud defaults.
func parsePrefs(r io.Reader) (map[string]prefValue, error) {
	values := make(map[string]prefValue, 16)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
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
		args := strings.TrimSuffix(strings.TrimSuffix(line[len(call):], ";"), ")")
		key, rest, ok := cutQuotedString(args)
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, ",") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(rest, ","))
		raw = strings.TrimSuffix(strings.TrimSpace(strings.TrimSuffix(raw, ")")), ";")
		values[key] = parsePrefValue(strings.TrimSpace(raw))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func parsePrefValue(raw string) prefValue {
	switch raw {
	case "true":
		return prefValue{isBool: true, b: true}
	case "false":
		return prefValue{isBool: true}
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return prefValue{isNum: true, n: n}
	}
	if s, _, ok := cutQuotedString(raw); ok {
		return prefValue{str: s}
	}
	return prefValue{str: raw, undecodable: true}
}

// stripBOM removes a leading UTF-8 byte order mark.
func stripBOM(s string) string { return strings.TrimPrefix(s, "\ufeff") }

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
