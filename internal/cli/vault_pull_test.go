// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderPullRoundTripPreservesTextSafely pins the renderer's contract:
// source text remains readable after a pull, while text that looks like HTML
// remains inert in the resulting Markdown.
func TestRenderPullRoundTripPreservesTextSafely(t *testing.T) {
	cases := []struct {
		md   string
		want string
	}{
		{
			md:   "First line one\nline two\n\nSecond **bold** [[wiki]] <x> & y",
			want: "First line one\nline two\n\nSecond **bold** [[wiki]] &lt;x&gt; &amp; y",
		},
		{md: "single paragraph", want: "single paragraph"},
		{md: "a & b < c > d \"quoted\" 'apos'", want: "a &amp; b &lt; c &gt; d \"quoted\" 'apos'"},
		{md: "- not a list, just text\n- still text\n\n| table | row |", want: "- not a list, just text\n- still text\n\n| table | row |"},
	}
	for _, tc := range cases {
		got := htmlNoteToMarkdown(markdownToNoteHTML("smith2024", tc.md))
		if got != tc.want {
			t.Errorf("round-trip mismatch:\n in   %q\n got  %q\n want %q", tc.md, got, tc.want)
		}
	}
}

func TestVaultPullPushEntityRoundTripIsIdempotent(t *testing.T) {
	original := "<h1>Obsidian notes — paper</h1><p><em>Managed from the vault by zotio. Edit in Obsidian.</em></p>" +
		"<p>A &amp; B</p><p>x &lt; y &gt; z</p><p>literal &amp;amp;</p>"
	pulled := htmlNoteToMarkdown(original)
	wantPulled := "A &amp; B\n\nx &lt; y &gt; z\n\nliteral &amp;amp;"
	if pulled != wantPulled {
		t.Fatalf("pulled Markdown = %q, want %q", pulled, wantPulled)
	}

	pushed := markdownToNoteHTML("paper", pulled)
	if pushed != original {
		t.Fatalf("pushed HTML = %q, want original %q", pushed, original)
	}
	if pulledAgain := htmlNoteToMarkdown(pushed); pulledAgain != pulled {
		t.Fatalf("second pull = %q, want %q", pulledAgain, pulled)
	}
}

func TestHTMLNoteToMarkdownEscapesDecodedRemoteMarkup(t *testing.T) {
	noteHTML := "<h1>Obsidian notes — paper</h1>" +
		"<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>" +
		"<p>&lt;img src=x onerror=alert(1)&gt;</p>" +
		"<p>&amp;lt;iframe src=evil&amp;gt;</p>"

	got := htmlNoteToMarkdown(noteHTML)
	want := "&lt;script&gt;alert(1)&lt;/script&gt;\n\n" +
		"&lt;img src=x onerror=alert(1)&gt;\n\n" +
		"&amp;lt;iframe src=evil&amp;gt;"
	if got != want {
		t.Fatalf("htmlNoteToMarkdown() = %q, want %q", got, want)
	}
	if strings.Contains(got, "<") {
		t.Fatalf("htmlNoteToMarkdown emitted raw markup: %q", got)
	}
}

func TestIsManagedNoteHTML(t *testing.T) {
	if !isManagedNoteHTML(markdownToNoteHTML("k", "hi")) {
		t.Errorf("our own rendered note should be recognized as managed")
	}
	if isManagedNoteHTML("<p>some unrelated Zotero note</p>") {
		t.Errorf("foreign note must not be treated as managed")
	}
}

func TestStripHTMLTags(t *testing.T) {
	if got := stripHTMLTags("<p>hello <strong>world</strong></p>"); got != "hello world" {
		t.Errorf("stripHTMLTags = %q, want 'hello world'", got)
	}
}

func TestReplaceNotesRegion(t *testing.T) {
	body := "head\n## Notes\n" + vaultNotesBegin + "\nold\n" + vaultNotesEnd + "\ntail\n"
	got, ok := replaceNotesRegion(body, "new\nlines")
	if !ok {
		t.Fatalf("markers not found")
	}
	region, _ := extractNotesRegion(got)
	if region != "new\nlines" {
		t.Errorf("region = %q, want 'new\\nlines'", region)
	}
	if !strings.Contains(got, "head") || !strings.Contains(got, "tail") || strings.Contains(got, "old") {
		t.Errorf("surrounding content not preserved / old not replaced:\n%s", got)
	}
	if _, ok := replaceNotesRegion("no markers", "x"); ok {
		t.Errorf("missing markers should report not-found")
	}
}

func TestApplyPulledRegion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "n.md")
	writeFile(t, p, "---\nzotero_key: K1\n---\n\n## Notes\n"+vaultNotesBegin+"\nold local\n"+vaultNotesEnd+"\n")
	st := pushState{Schema: 1, NoteKey: "N1", NoteVersion: 7, SourceHash: "s", RemoteHash: "r", Renderer: vaultRenderer}

	if err := applyPulledRegion(p, "new pulled\ncontent", st); err != nil {
		t.Fatalf("applyPulledRegion: %v", err)
	}
	s := readNote(t, p)
	region, ok := extractNotesRegion(s)
	if !ok || region != "new pulled\ncontent" {
		t.Errorf("region = (%q,%v)", region, ok)
	}
	if parseStateComment(s) != st {
		t.Errorf("state not written in same pass: %+v", parseStateComment(s))
	}
	if strings.Contains(s, "old local") {
		t.Errorf("old region not replaced:\n%s", s)
	}
}
