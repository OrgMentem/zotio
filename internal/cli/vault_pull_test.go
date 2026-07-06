// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean 15e0): unit coverage for vault pull — push/pull round-trip
// losslessness, managed-shape gating, tag stripping, region replacement, and the
// atomic region+state write.

package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderPullRoundTrip is the core guarantee: content this CLI pushes survives
// a pull byte-for-byte (verbatim renderer + conservative HTML->text reverse).
func TestRenderPullRoundTrip(t *testing.T) {
	cases := []string{
		"First line one\nline two\n\nSecond **bold** [[wiki]] <x> & y",
		"single paragraph",
		"a & b < c > d \"quoted\" 'apos'",
		"- not a list, just text\n- still text\n\n| table | row |",
	}
	for _, md := range cases {
		htmlOut := markdownToNoteHTML("smith2024", md)
		got := htmlNoteToMarkdown(htmlOut)
		if got != md {
			t.Errorf("round-trip mismatch:\n in  %q\n got %q", md, got)
		}
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
