// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean 15e0): unit coverage for the pull-free pieces of vault push —
// paragraph-verbatim rendering (no Markdown interpreted, everything escaped),
// state comment round-trip, notes-region extraction, deterministic write token,
// atomic state writes, and output-dir resolution.

package cli

import (
	"fmt"
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
	got := parseStateComment(body)
	if got != st {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, st)
	}
	if (parseStateComment("no state here") != pushState{}) {
		t.Errorf("absent state should be the zero value")
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
	if !strings.Contains(s, vaultStatePrefix) || parseStateComment(s) != st {
		t.Fatalf("state not appended/parseable:\n%s", s)
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
	if parseStateComment(s) != st {
		t.Errorf("state not updated in place")
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
