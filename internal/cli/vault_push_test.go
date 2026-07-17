// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarkdownToNoteHTMLVerbatim(t *testing.T) {
	md := "First para line1\nline2\n\n**bold** [[wiki]] <x> & y"
	got := markdownToNoteHTML("smith2024", md)

	for _, want := range []string{
		"<h1>Obsidian notes — smith2024</h1>",
		"Managed from the vault",
		"First para line1<br>line2",
		"**bold** [[wiki]] &lt;x&gt; &amp; y", // escaped, not interpreted
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n%s", want, got)
		}
	}
	if strings.Contains(got, "<strong>") || strings.Contains(got, "<script>") {
		t.Errorf("renderer interpreted Markdown/HTML (should be verbatim):\n%s", got)
	}
	// Two prose paragraphs -> two <p> blocks after the managed prefix paragraph.
	if n := strings.Count(got, "<p>"); n != 3 {
		t.Errorf("expected 3 <p> (1 managed + 2 prose), got %d:\n%s", n, got)
	}
}

func TestSplitParagraphs(t *testing.T) {
	got := splitParagraphs("a\nb\n\n\nc\n\n   \n d ")
	want := []string{"a\nb", "c", " d "}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("splitParagraphs = %q, want %q", got, want)
	}
	if splitParagraphs("   \n\n  ") != nil {
		t.Errorf("all-blank input should yield nil")
	}
}

func TestStateCommentRoundTrip(t *testing.T) {
	st := pushState{Schema: 1, NoteKey: "N8K2QX7M", NoteVersion: 481, SourceHash: "aaa", RemoteHash: "bbb", Renderer: vaultRenderer}
	body := "intro\n" + stateComment(st) + "\ntail"
	got, err := parseStateComment(body)
	if err != nil {
		t.Fatalf("round-trip parse error: %v", err)
	}
	if got != st {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, st)
	}
	if absent, err := parseStateComment("no state here"); err != nil || absent != (pushState{}) {
		t.Errorf("absent state should be the zero value with no error, got %+v err=%v", absent, err)
	}
}

func TestStateCommentMalformedSurfacesError(t *testing.T) {
	body := "intro\n" + vaultStatePrefix + "{not valid json" + " -->" + "\ntail"
	got, err := parseStateComment(body)
	if err == nil {
		t.Fatalf("malformed state comment should surface a parse error, got silent %+v", got)
	}
	if got != (pushState{}) {
		t.Errorf("malformed state should yield zero value alongside error, got %+v", got)
	}
}

func TestExtractNotesRegion(t *testing.T) {
	body := "## Notes\n" + vaultNotesBegin + "\nhello\nworld\n" + vaultNotesEnd + "\n"
	region, ok := extractNotesRegion(body)
	if !ok || region != "hello\nworld" {
		t.Errorf("extractNotesRegion = (%q,%v)", region, ok)
	}
	if _, ok := extractNotesRegion("no markers"); ok {
		t.Errorf("missing markers should report not-found")
	}
}

func TestWriteToken(t *testing.T) {
	a := writeToken("P", "html")
	if len(a) != 32 {
		t.Errorf("write token len = %d, want 32", len(a))
	}
	if a != writeToken("P", "html") {
		t.Errorf("write token not deterministic")
	}
	if a == writeToken("P", "other") {
		t.Errorf("write token should differ for different payloads")
	}
}

func TestWriteNoteStateAppendThenReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.md")
	writeFile(t, path, "## Notes\n"+vaultNotesBegin+"\nmine\n"+vaultNotesEnd+"\n")

	st := pushState{Schema: 1, NoteKey: "K1", NoteVersion: 3, SourceHash: "s", RemoteHash: "r", Renderer: vaultRenderer}
	if err := writeNoteState(path, st); err != nil {
		t.Fatalf("append state: %v", err)
	}
	s := readNote(t, path)
	if got, err := parseStateComment(s); !strings.Contains(s, vaultStatePrefix) || err != nil || got != st {
		t.Fatalf("state not appended/parseable (err=%v):\n%s", err, s)
	}
	if !strings.Contains(s, "mine") {
		t.Errorf("user region lost on state append")
	}

	st.NoteVersion = 4
	st.SourceHash = "s2"
	if err := writeNoteState(path, st); err != nil {
		t.Fatalf("replace state: %v", err)
	}
	s = readNote(t, path)
	if c := strings.Count(s, vaultStatePrefix); c != 1 {
		t.Errorf("state comment duplicated (%d):\n%s", c, s)
	}
	if got, err := parseStateComment(s); err != nil || got != st {
		t.Errorf("state not updated in place (err=%v)", err)
	}
}

func TestResolveVaultOutDir(t *testing.T) {
	// Explicit --out wins.
	if got, err := resolveVaultOutDir(&rootFlags{}, "/x/y"); err != nil || got != "/x/y" {
		t.Errorf("flag out = (%q,%v), want /x/y", got, err)
	}
	// Config fallback.
	root := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, cfg, fmt.Sprintf("[vault]\nroot = %q\nnotes_dir = \"refs\"\n", root))
	if got, err := resolveVaultOutDir(&rootFlags{configPath: cfg}, ""); err != nil || got != filepath.Join(root, "refs") {
		t.Errorf("config out = (%q,%v), want %s/refs", got, err, root)
	}
	// Neither -> error.
	if _, err := resolveVaultOutDir(&rootFlags{configPath: filepath.Join(t.TempDir(), "missing.toml")}, ""); err == nil {
		t.Errorf("expected error with no --out and no config")
	}
}

func TestLoadPushNotesParsesStateAndRegion(t *testing.T) {
	dir := t.TempDir()
	st := pushState{Schema: 1, NoteKey: "N1", NoteVersion: 2, SourceHash: "h", RemoteHash: "r", Renderer: vaultRenderer}
	writeFile(t, filepath.Join(dir, "a.md"),
		"---\nzotero_key: K1\ncitekey: smith2024\nzotero_library: users/42\n---\n\n## Notes\n"+
			vaultNotesBegin+"\nbody text\n"+vaultNotesEnd+"\n"+stateComment(st)+"\n")
	writeFile(t, filepath.Join(dir, "no-region.md"), "---\nzotero_key: K2\n---\n\nplain\n")

	notes, err := loadPushNotes(dir)
	if err != nil {
		t.Fatalf("loadPushNotes: %v", err)
	}
	if len(notes) != 2 {
		t.Fatalf("loaded %d notes, want 2", len(notes))
	}
	var a *pushNote
	for _, n := range notes {
		if n.itemKey == "K1" {
			a = n
		}
	}
	if a == nil {
		t.Fatalf("note K1 not loaded")
	}
	if a.citekey != "smith2024" || a.library != "users/42" || !a.hasRegion || strings.TrimSpace(a.region) != "body text" {
		t.Errorf("parsed note wrong: %+v", a)
	}
	if a.state != st {
		t.Errorf("state mismatch: got %+v want %+v", a.state, st)
	}
}

func TestVaultPushReportsUnreadableNotes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod(0000) is not enforced for root; unreadable-note path cannot be exercised")
	}
	// No note is pushable (one is unreadable, the other unmanaged), so any
	// outbound request is a bug.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("unexpected request while all notes are unreadable/unmanaged")
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	dir := t.TempDir()
	// A real managed note made unreadable (permission denied) — distinct from a
	// MISSING file, which readVaultFile legitimately treats as absent. This is
	// the swallowed-read-failure the fix must surface.
	unreadable := filepath.Join(dir, "unreadable.md")
	writeFile(t, unreadable, "---\nzotero_key: ABCD1234\n---\nbody")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("chmod unreadable note: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })
	writeFile(t, filepath.Join(dir, "plain.md"), "not a managed note")

	notes, warnings, err := loadPushNotesWithWarnings(dir)
	if err != nil {
		t.Fatalf("loadPushNotesWithWarnings: %v", err)
	}
	if len(notes) != 1 || len(warnings) != 1 {
		t.Fatalf("got %d notes and %d warnings, want 1 note and 1 warning", len(notes), len(warnings))
	}
	if !strings.Contains(warnings[0], "unreadable.md") {
		t.Fatalf("warning does not name unreadable note: %q", warnings[0])
	}

	cmd := newVaultPushCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--out", dir})
	err = cmd.Execute()
	if code := ExitCode(err); code != 13 {
		t.Fatalf("vault push exit code = %d, want 13 (err=%v)", code, err)
	}
	var report vaultWriteReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, stdout.String())
	}
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "unreadable.md") {
		t.Fatalf("report warnings = %#v", report.Warnings)
	}
	if stderr.Len() != 0 {
		t.Fatalf("JSON mode should not print warnings to stderr: %s", stderr.String())
	}
}

func TestVaultPushAllReadableNotesExitZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatalf("unexpected request for an unbound note")
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "plain.md"), "not a managed note")
	cmd := newVaultPushCmd(&rootFlags{asJSON: true})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--out", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("vault push with readable note: %v", err)
	}
}
