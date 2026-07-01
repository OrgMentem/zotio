// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean 15e0): cover commit-1 format/safety hardening — identity keys and
// managed/user fences on create, stable lookup by zotero_key (no duplicate on
// citekey change), filename collision avoidance, legacy "## Notes" migration,
// needs_notes_boundary, compare-before-replace writes, read-error handling,
// rune-safe filenames, and library identity resolution.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"zotio/internal/config"
	"zotio/internal/store"
)

func TestVaultSyncWritesIdentityAndFences(t *testing.T) {
	seedVaultStore(t)
	vault := t.TempDir()
	runVaultSync(t, &rootFlags{asJSON: true}, []string{"--out", vault})

	s := readNote(t, filepath.Join(vault, "vaswani2017.md"))
	for _, want := range []string{
		"zotero_key: K1",
		vaultTitleBegin, vaultTitleEnd,
		vaultAbstractBegin, vaultAbstractEnd,
		vaultNotesBegin, vaultNotesEnd,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("new note missing %q\n---\n%s", want, s)
		}
	}
}

// TestVaultSyncStableLookupByKey: a managed note whose citation key changed must
// be updated in place, not duplicated under the new citekey filename.
func TestVaultSyncStableLookupByKey(t *testing.T) {
	seedVaultStore(t)
	vault := t.TempDir()
	old := filepath.Join(vault, "oldcitekey.md")
	writeFile(t, old, "---\nzotero_key: K1\ncitekey: oldcitekey\n---\n\n## Notes\n"+
		vaultNotesBegin+"\nmine\n"+vaultNotesEnd+"\n")

	runVaultSync(t, &rootFlags{asJSON: true}, []string{"--out", vault})

	if _, err := os.Stat(filepath.Join(vault, "vaswani2017.md")); !os.IsNotExist(err) {
		t.Errorf("created a duplicate note instead of updating the existing one by key")
	}
	s := readNote(t, old)
	if !strings.Contains(s, "citekey: vaswani2017") {
		t.Errorf("citekey not refreshed in existing note:\n%s", s)
	}
	if !strings.Contains(s, "mine") {
		t.Errorf("user notes clobbered:\n%s", s)
	}
}

// TestVaultSyncFilenameCollision: the citekey filename is taken by a foreign
// (unmanaged) file, so the new note must be disambiguated and the foreign file
// left untouched.
func TestVaultSyncFilenameCollision(t *testing.T) {
	seedVaultStore(t)
	vault := t.TempDir()
	foreign := filepath.Join(vault, "vaswani2017.md")
	writeFile(t, foreign, "not mine\n")

	runVaultSync(t, &rootFlags{asJSON: true}, []string{"--out", vault})

	if got := readNote(t, foreign); got != "not mine\n" {
		t.Errorf("clobbered foreign file: %q", got)
	}
	if _, err := os.Stat(filepath.Join(vault, "vaswani2017--K1.md")); err != nil {
		t.Errorf("disambiguated note not created: %v", err)
	}
}

// TestVaultSyncMigratesLegacyNotes: a pre-markers note with a single "## Notes"
// heading gets the notes fence injected around the existing prose.
func TestVaultSyncMigratesLegacyNotes(t *testing.T) {
	seedVaultStore(t)
	vault := t.TempDir()
	writeFile(t, filepath.Join(vault, "vaswani2017.md"),
		"---\nzotero_key: K1\ncitekey: vaswani2017\n---\n\n## Notes\n\nlegacy prose\n")

	runVaultSync(t, &rootFlags{asJSON: true}, []string{"--out", vault})

	s := readNote(t, filepath.Join(vault, "vaswani2017.md"))
	bi, ei := strings.Index(s, vaultNotesBegin), strings.Index(s, vaultNotesEnd)
	pi := strings.Index(s, "legacy prose")
	if bi < 0 || ei < 0 {
		t.Fatalf("notes markers not injected:\n%s", s)
	}
	if !(bi < pi && pi < ei) {
		t.Errorf("legacy prose not inside the notes region:\n%s", s)
	}
}

// TestVaultSyncNeedsNotesBoundary: an ambiguous note (two "## Notes" headings)
// is reported and left without injected markers.
func TestVaultSyncNeedsNotesBoundary(t *testing.T) {
	seedVaultStore(t)
	vault := t.TempDir()
	writeFile(t, filepath.Join(vault, "vaswani2017.md"),
		"---\nzotero_key: K1\n---\n\n## Notes\na\n\n## Notes\nb\n")

	out := runVaultSync(t, &rootFlags{asJSON: true}, []string{"--out", vault})
	var report struct {
		Results []vaultSyncResult
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	found := false
	for _, r := range report.Results {
		if r.Key == "K1" && strings.Contains(r.Note, "needs_notes_boundary") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected needs_notes_boundary in results, got %s", out)
	}
	if strings.Contains(readNote(t, filepath.Join(vault, "vaswani2017.md")), vaultNotesBegin) {
		t.Errorf("notes markers injected despite ambiguous boundary")
	}
}

func TestAtomicReplaceCompareBeforeReplace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "n.md")
	writeFile(t, p, "A")

	if err := atomicReplace(p, []byte("WRONG"), []byte("B")); !errors.Is(err, errVaultFileBusy) {
		t.Fatalf("stale expected: got err=%v, want errVaultFileBusy", err)
	}
	if got := readNote(t, p); got != "A" {
		t.Errorf("file changed despite busy guard: %q", got)
	}
	if err := atomicReplace(p, []byte("A"), []byte("B")); err != nil {
		t.Fatalf("atomicReplace with matching expected: %v", err)
	}
	if got := readNote(t, p); got != "B" {
		t.Errorf("file not replaced: %q", got)
	}
}

func TestReadVaultFileDistinguishesErrors(t *testing.T) {
	dir := t.TempDir()
	if data, err := readVaultFile(filepath.Join(dir, "nope.md")); data != nil || err != nil {
		t.Errorf("missing file: data=%v err=%v, want nil,nil", data, err)
	}
	if _, err := readVaultFile(dir); err == nil {
		t.Errorf("reading a directory should be a real error, not swallowed")
	}
}

func TestEnsureNotesRegion(t *testing.T) {
	good := "## Notes\n" + vaultNotesBegin + "\nx\n" + vaultNotesEnd + "\n"
	if out, boundary := ensureNotesRegion(good); boundary || out != good {
		t.Errorf("well-formed region changed (boundary=%v):\n%s", boundary, out)
	}
	if out, boundary := ensureNotesRegion("## Notes\nprose\n"); boundary || !strings.Contains(out, vaultNotesBegin) {
		t.Errorf("single heading not migrated (boundary=%v):\n%s", boundary, out)
	}
	if _, boundary := ensureNotesRegion("no notes here\n"); !boundary {
		t.Errorf("zero headings should report boundary")
	}
	dup := vaultNotesBegin + "\n" + vaultNotesBegin + "\n" + vaultNotesEnd + "\n"
	if _, boundary := ensureNotesRegion(dup); !boundary {
		t.Errorf("duplicate begin marker should report boundary")
	}
}

func TestSanitizeVaultFilenameRuneSafe(t *testing.T) {
	got := sanitizeVaultFilename(strings.Repeat("é", 200)) // 400 bytes
	if !utf8.ValidString(got) {
		t.Errorf("rune-unsafe truncation produced invalid UTF-8: %q", got)
	}
	if len(got) > 120 {
		t.Errorf("truncated length = %d bytes, want <= 120", len(got))
	}
}

func TestVaultLibraryID(t *testing.T) {
	saved := activeGroupID
	defer func() { activeGroupID = saved }()

	activeGroupID = "999"
	if got := vaultLibraryID(&rootFlags{}); got != "groups/999" {
		t.Errorf("group library = %q, want groups/999", got)
	}

	activeGroupID = ""
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	// PATCH(glean 61a2a8a9): synthetic user id (was a real account id).
	writeFile(t, cfgPath, "user_id = \"99999\"\n")
	if got := vaultLibraryID(&rootFlags{configPath: cfgPath}); got != "users/99999" {
		t.Errorf("personal library = %q, want users/99999", got)
	}

	if got := vaultLibraryID(&rootFlags{configPath: filepath.Join(t.TempDir(), "missing.toml")}); got != "" {
		t.Errorf("no user id = %q, want empty", got)
	}
}

func readNote(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestKeyFromZoteroSelect(t *testing.T) {
	cases := map[string]string{
		"zotero://select/library/items/9UXV5R7L":           "9UXV5R7L",
		"zotero://select/groups/123/items/ABCD1234":        "ABCD1234",
		"zotero://open-pdf/library/items/K1?annotation=A1": "K1",
		"":              "",
		"no items here": "",
	}
	for in, want := range cases {
		if got := keyFromZoteroSelect(in); got != want {
			t.Errorf("keyFromZoteroSelect(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestVaultSyncRecognizesLegacyNoteByLink: a note created before zotero_key
// existed (identity only in the zotero:// link) must be updated in place and
// upgraded with zotero_key, never duplicated under the citekey filename.
func TestVaultSyncRecognizesLegacyNoteByLink(t *testing.T) {
	seedVaultStore(t)
	vault := t.TempDir()
	legacy := filepath.Join(vault, "stalename.md")
	writeFile(t, legacy, "---\ncitekey: vaswani2017\nzotero: \"zotero://select/library/items/K1\"\n---\n\n## Notes\n\nkeep me\n")

	runVaultSync(t, &rootFlags{asJSON: true}, []string{"--out", vault})

	if _, err := os.Stat(filepath.Join(vault, "vaswani2017.md")); !os.IsNotExist(err) {
		t.Errorf("legacy note duplicated instead of updated in place")
	}
	s := readNote(t, legacy)
	if !strings.Contains(s, "zotero_key: K1") {
		t.Errorf("legacy note not upgraded with zotero_key:\n%s", s)
	}
	if !strings.Contains(s, "keep me") {
		t.Errorf("legacy user prose lost:\n%s", s)
	}
}

func TestVaultResolveOut(t *testing.T) {
	home, _ := os.UserHomeDir()
	cases := []struct {
		root, notesDir, want string
	}{
		{"/v", "refs", "/v/refs"},
		{"/v", "", "/v"},
		{"", "refs", ""},
		{"~/v", "refs", filepath.Join(home, "v", "refs")},
	}
	for _, c := range cases {
		got := vaultResolveOut(&config.VaultConfig{Root: c.root, NotesDir: c.notesDir})
		if got != c.want {
			t.Errorf("vaultResolveOut(%q,%q) = %q, want %q", c.root, c.notesDir, got, c.want)
		}
	}
}

func TestResolveCollectionNames(t *testing.T) {
	names := map[string]string{"C1": "Transformers", "C2": "Attention"}
	got := resolveCollectionNames([]string{"C1", "CX", "C2"}, names)
	want := []string{"Transformers", "CX", "Attention"} // unknown key falls back to the key
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("resolveCollectionNames = %v, want %v", got, want)
	}
	if resolveCollectionNames(nil, names) != nil {
		t.Errorf("empty keys should yield nil")
	}
}

// TestVaultSyncCollectionNames: a synced collection's display name is rendered
// into the managed collection_names frontmatter key.
func TestVaultSyncCollectionNames(t *testing.T) {
	seedVaultStore(t)
	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("collections", []json.RawMessage{
		json.RawMessage(`{"key":"COL1","data":{"key":"COL1","name":"Transformers"}}`),
	}); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	_ = db.Close()

	vault := t.TempDir()
	runVaultSync(t, &rootFlags{asJSON: true}, []string{"--out", vault})
	s := readNote(t, filepath.Join(vault, "vaswani2017.md"))
	if !strings.Contains(s, "collection_names:") || !strings.Contains(s, "Transformers") {
		t.Errorf("collection_names not resolved from store:\n%s", s)
	}
}

// TestVaultSyncResolvesOutFromConfig: with no --out, the output dir comes from
// [vault].root + notes_dir.
func TestVaultSyncResolvesOutFromConfig(t *testing.T) {
	seedVaultStore(t)
	root := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, cfgPath, fmt.Sprintf("[vault]\nroot = %q\nnotes_dir = \"refs\"\n", root))

	runVaultSync(t, &rootFlags{asJSON: true, configPath: cfgPath}, nil)

	if _, err := os.Stat(filepath.Join(root, "refs", "vaswani2017.md")); err != nil {
		t.Errorf("note not written to config-resolved dir: %v", err)
	}
}

// TestVaultSyncOutFlagOverridesConfig: an explicit --out wins over [vault].root.
func TestVaultSyncOutFlagOverridesConfig(t *testing.T) {
	seedVaultStore(t)
	cfgRoot := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, cfgPath, fmt.Sprintf("[vault]\nroot = %q\n", cfgRoot))
	out := t.TempDir()

	runVaultSync(t, &rootFlags{asJSON: true, configPath: cfgPath}, []string{"--out", out})

	if _, err := os.Stat(filepath.Join(out, "vaswani2017.md")); err != nil {
		t.Errorf("note not written to --out dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfgRoot, "vaswani2017.md")); !os.IsNotExist(err) {
		t.Errorf("note wrongly written to config root despite --out override")
	}
}
