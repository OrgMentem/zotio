// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
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
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "quoted double attribute", in: `<a title="Click > Here">text</a>`, want: "text"},
		{name: "quoted single attribute", in: `<span data-x='a > b'>y</span>`, want: "y"},
		{name: "single quote within double-quoted attribute", in: `<a title="Click ' > Here">text</a>`, want: "text"},
		{name: "double quote within single-quoted attribute", in: `<span data-x='a " > b'>y</span>`, want: "y"},
		{name: "ordinary tags", in: "<b>bold</b>", want: "bold"},
		{name: "bare greater-than text", in: "a > b", want: "a > b"},
		{name: "unterminated quote", in: `<a title="Click > Here`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripHTMLTags(tc.in); got != tc.want {
				t.Errorf("stripHTMLTags(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
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
	if got, err := parseStateComment(s); err != nil || got != st {
		t.Errorf("state not written in same pass: %+v (err=%v)", got, err)
	}
	if strings.Contains(s, "old local") {
		t.Errorf("old region not replaced:\n%s", s)
	}
}

func TestVaultPullDryRunConflictDoesNotWriteArtifact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":2,"data":{"note":"<p>remote changed</p>"}}`))
	}))
	t.Cleanup(srv.Close)
	c := client.New(&config.Config{BaseURL: srv.URL}, time.Second, 0)
	c.NoCache = true

	outDir := t.TempDir()
	note := &pushNote{
		path:      filepath.Join(outDir, "cite.md"),
		citekey:   "cite",
		hasRegion: true,
		region:    "local changed",
		state: pushState{
			NoteKey:    "ABCD1234",
			SourceHash: sha256hex("baseline"),
		},
	}
	result := pullOne(c, outDir, note, &rootFlags{dryRun: true}, true)
	if result.Status != "would conflict" {
		t.Fatalf("dry-run conflict status = %q (%s), want would conflict", result.Status, result.Note)
	}
	if _, err := os.Stat(filepath.Join(outDir, vaultConflictsDir)); !os.IsNotExist(err) {
		t.Fatalf("dry-run created conflict artifact directory: %v", err)
	}
}

// TestVaultPullPreviewsWithoutWriting proves pull's default gate: neither a
// bare invocation nor --agent applies a write. The fixture note is a clean
// fast-forward candidate (local region unchanged, remote note moved), so
// pullOne's non-preview branch would call applyPulledRegion and rewrite the
// vault file. The preview path must never reach that write: no Zotero write
// request, and the note file is left byte-for-byte unchanged.
func TestVaultPullPreviewsWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags rootFlags
	}{
		{name: "bare", flags: rootFlags{asJSON: true}},
		{name: "agent", flags: rootFlags{asJSON: true, agent: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			region := "original local notes"
			remoteHTML := markdownToNoteHTML("cite1", "remote updated notes")
			respBody, err := json.Marshal(map[string]any{
				"version": 6,
				"data":    map[string]string{"note": remoteHTML},
			})
			if err != nil {
				t.Fatalf("marshal fixture response: %v", err)
			}

			var violation string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					violation = r.Method + " " + r.URL.Path
					http.Error(w, "unexpected write under preview", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(respBody)
			}))
			t.Cleanup(srv.Close)
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

			outDir := t.TempDir()
			state := pushState{
				Schema:      noteStateSchema,
				NoteKey:     "NOTEKEY1",
				NoteVersion: 5,
				SourceHash:  sha256hex(region),
				RemoteHash:  sha256hex(markdownToNoteHTML("cite1", "original notes")),
				Renderer:    vaultRenderer,
			}
			notePath := filepath.Join(outDir, "note.md")
			writeFile(t, notePath, "---\nzotero_key: K1\ncitekey: cite1\n---\n\n## Notes\n"+
				vaultNotesBegin+"\n"+region+"\n"+vaultNotesEnd+"\n"+stateComment(state)+"\n")
			before, err := os.ReadFile(notePath)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}

			flags := tc.flags
			cmd := newVaultPullCmd(&flags)
			cmd.SetArgs([]string{"--out", outDir})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("vault pull (%s): %v", tc.name, err)
			}
			if violation != "" {
				t.Fatalf("%s: preview issued a Zotero write request: %s", tc.name, violation)
			}

			var report vaultWriteReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatalf("decode report %q: %v", out.String(), err)
			}
			if !report.DryRun || report.Counts["would pull"] != 1 {
				t.Errorf("%s report = %+v, want dry_run + 1 would-pull note", tc.name, report)
			}

			after, err := os.ReadFile(notePath)
			if err != nil {
				t.Fatalf("read note after preview: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("%s: preview modified the vault note", tc.name)
			}
			if _, err := os.Stat(filepath.Join(outDir, vaultConflictsDir)); !os.IsNotExist(err) {
				t.Fatalf("%s: preview wrote a conflict artifact", tc.name)
			}
		})
	}
}
