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

func TestParsePushNoteLogseqProperties(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logseq.md")
	writeFile(t, path, strings.Join([]string{
		"citekey:: smith2024",
		"zotero-key:: K1",
		"zotero-library:: users/42",
		"zotero:: zotero://select/library/items/K1",
		"",
		"## Notes",
		vaultNotesBegin,
		"body text",
		vaultNotesEnd,
	}, "\n"))

	note, err := parsePushNote(path)
	if err != nil {
		t.Fatalf("parse Logseq note: %v", err)
	}
	if note.itemKey != "K1" || note.citekey != "smith2024" || note.library != "users/42" || !note.hasRegion {
		t.Errorf("parsed Logseq note wrong: %+v", note)
	}
}

func TestVaultPushReportFailsForNoteFailures(t *testing.T) {
	for _, status := range []string{"error", "conflict", "remote_deleted"} {
		t.Run(status, func(t *testing.T) {
			cmd := newVaultPushCmd(&rootFlags{asJSON: true})
			var out bytes.Buffer
			cmd.SetOut(&out)

			err := printVaultWriteReport(cmd, []pushResult{{File: "note.md", Status: status}}, "vault", &rootFlags{asJSON: true}, false, "Pushed", "Would push")
			if code := ExitCode(err); code != 13 {
				t.Fatalf("exit code = %d, want 13 (err=%v)", code, err)
			}
			if !strings.Contains(out.String(), status) {
				t.Fatalf("report did not render %q: %s", status, out.String())
			}
		})
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

// TestVaultPushPreviewsWithoutWriting proves push's default gate: neither a
// bare invocation nor --agent applies a write. The fixture note has a local
// edit since its last push (so pushOne's non-preview branch would call
// patchWithConflict -> PUT), which the preview path must never reach: no
// Zotero write request, and the vault note itself is left byte-for-byte
// unchanged.
func TestVaultPushPreviewsWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags rootFlags
	}{
		{name: "bare", flags: rootFlags{asJSON: true}},
		{name: "agent", flags: rootFlags{asJSON: true, agent: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var violation string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					violation = r.Method + " " + r.URL.Path
					http.Error(w, "unexpected write under preview", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"NOTEKEY1":5}`))
			}))
			t.Cleanup(srv.Close)
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

			outDir := t.TempDir()
			region := "updated local notes"
			state := pushState{
				Schema:      noteStateSchema,
				NoteKey:     "NOTEKEY1",
				NoteVersion: 5,
				SourceHash:  sha256hex("original notes"),
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
			cmd := newVaultPushCmd(&flags)
			cmd.SetArgs([]string{"--out", outDir})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("vault push (%s): %v", tc.name, err)
			}
			if violation != "" {
				t.Fatalf("%s: preview issued a Zotero write request: %s", tc.name, violation)
			}

			var report vaultWriteReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatalf("decode report %q: %v", out.String(), err)
			}
			if !report.DryRun || report.Counts["would update"] != 1 {
				t.Errorf("%s report = %+v, want dry_run + 1 would-update note", tc.name, report)
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

// TestVaultResolvePreviewsWithoutWriting proves resolve's default gate: neither
// a bare invocation nor --agent applies a write, even though --keep-vault
// targets a note whose vault region differs from the last-pushed baseline (so
// the apply branch would PATCH). The fixture note key must be exactly 8
// uppercase-alphanumeric characters — validateZoteroKey rejects anything
// shorter before the gate is ever reached. --yes must still apply.
func TestVaultResolvePreviewsWithoutWriting(t *testing.T) {
	const citekey = "cite1"
	const noteKey = "NOTEKEY1" // validateZoteroKey: exactly 8 uppercase-alnum chars
	const region = "vault copy wins"

	newFixture := func(t *testing.T) (outDir, notePath string, before []byte, artifact string) {
		t.Helper()
		outDir = t.TempDir()
		state := pushState{
			Schema:      noteStateSchema,
			NoteKey:     noteKey,
			NoteVersion: 5,
			SourceHash:  sha256hex("original notes"),
			RemoteHash:  sha256hex(markdownToNoteHTML(citekey, "original notes")),
			Renderer:    vaultRenderer,
		}
		notePath = filepath.Join(outDir, "note.md")
		writeFile(t, notePath, "---\nzotero_key: K1\ncitekey: "+citekey+"\n---\n\n## Notes\n"+
			vaultNotesBegin+"\n"+region+"\n"+vaultNotesEnd+"\n"+stateComment(state)+"\n")
		var err error
		before, err = os.ReadFile(notePath)
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}

		// A conflict artifact resolve must leave untouched in preview mode
		// (removeConflictArtifacts matches on this exact citekey--noteKey prefix).
		confDir := filepath.Join(outDir, vaultConflictsDir)
		if err := os.MkdirAll(confDir, 0o755); err != nil {
			t.Fatalf("mkdir conflicts dir: %v", err)
		}
		artifact = filepath.Join(confDir, sanitizeVaultFilename(citekey+"--"+noteKey)+"--remote-v7.md")
		writeFile(t, artifact, "conflict body")
		return outDir, notePath, before, artifact
	}

	for _, tc := range []struct {
		name  string
		flags rootFlags
	}{
		{name: "bare", flags: rootFlags{}},
		{name: "agent", flags: rootFlags{agent: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var violation string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					violation = r.Method + " " + r.URL.Path
					http.Error(w, "unexpected write under preview", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Last-Modified-Version", "7")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"version":7,"data":{"note":"<p>live</p>"}}`))
			}))
			t.Cleanup(srv.Close)
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

			outDir, notePath, before, artifact := newFixture(t)

			flags := tc.flags
			cmd := newVaultResolveCmd(&flags)
			cmd.SetArgs([]string{citekey, "--keep-vault", "--out", outDir})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("vault resolve (%s): %v", tc.name, err)
			}
			if violation != "" {
				t.Fatalf("%s: preview issued a non-GET request: %s", tc.name, violation)
			}

			after, err := os.ReadFile(notePath)
			if err != nil {
				t.Fatalf("read note after preview: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("%s: preview modified the vault note", tc.name)
			}
			if _, err := os.Stat(artifact); err != nil {
				t.Fatalf("%s: preview removed the conflict artifact: %v", tc.name, err)
			}
		})
	}

	// --yes applies: the PATCH reaches the server, the vault note's baseline
	// advances, and the conflict artifact is cleared — the mirror image of the
	// preview assertions above, proving the gate (not something else) is what
	// blocked the write.
	t.Run("yes applies", func(t *testing.T) {
		var patched bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Last-Modified-Version", "7")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"version":7,"data":{"note":"<p>live</p>"}}`))
			case http.MethodPatch:
				patched = true
				w.WriteHeader(http.StatusNoContent)
			default:
				t.Fatalf("unexpected method %s %s", r.Method, r.URL.Path)
			}
		}))
		t.Cleanup(srv.Close)
		t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
		t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

		outDir, notePath, before, artifact := newFixture(t)

		flags := rootFlags{yes: true, maxChanges: -1}
		cmd := newVaultResolveCmd(&flags)
		cmd.SetArgs([]string{citekey, "--keep-vault", "--out", outDir})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("vault resolve --yes: %v", err)
		}
		if !patched {
			t.Fatalf("--yes did not PATCH the Zotero note")
		}

		after, err := os.ReadFile(notePath)
		if err != nil {
			t.Fatalf("read note after apply: %v", err)
		}
		if bytes.Equal(before, after) {
			t.Fatalf("--yes should have advanced the vault note's baseline state")
		}
		if _, err := os.Stat(artifact); !os.IsNotExist(err) {
			t.Fatalf("--yes should have removed the conflict artifact (err=%v)", err)
		}
	})
}

// TestVaultResolvePreviewDetectsRemotelyDeletedNote is the regression guard for
// d047d69: gating vault resolve dropped the unconditional c.DryRun = false,
// which had a second purpose beyond forcing writes off -- it forced reads
// live. client.DryRun suppresses every HTTP verb, GET included, and returns a
// stubbed, error-free response, so under --dry-run (including inside a
// `workflow run` preview, which injects --dry-run into every step) getNote
// silently returned a fabricated zero-version note instead of the server's
// 404, and preview printed a confident but false "would resolve" message.
// Both --dry-run and a bare invocation must surface the read error instead.
func TestVaultResolvePreviewDetectsRemotelyDeletedNote(t *testing.T) {
	const citekey = "cite1"
	const noteKey = "NOTEKEY1" // validateZoteroKey: exactly 8 uppercase-alnum chars
	const region = "vault copy wins"

	for _, tc := range []struct {
		name  string
		flags rootFlags
	}{
		{name: "dry-run", flags: rootFlags{dryRun: true}},
		{name: "bare", flags: rootFlags{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var nonGET string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					nonGET = r.Method + " " + r.URL.Path
					http.Error(w, "unexpected write under preview", http.StatusInternalServerError)
					return
				}
				// The remote child note is gone: Zotero answers 404.
				http.Error(w, `{"message":"Not found"}`, http.StatusNotFound)
			}))
			t.Cleanup(srv.Close)
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

			outDir := t.TempDir()
			state := pushState{
				Schema:      noteStateSchema,
				NoteKey:     noteKey,
				NoteVersion: 5,
				SourceHash:  sha256hex("original notes"),
				RemoteHash:  sha256hex(markdownToNoteHTML(citekey, "original notes")),
				Renderer:    vaultRenderer,
			}
			writeFile(t, filepath.Join(outDir, "note.md"), "---\nzotero_key: K1\ncitekey: "+citekey+"\n---\n\n## Notes\n"+
				vaultNotesBegin+"\n"+region+"\n"+vaultNotesEnd+"\n"+stateComment(state)+"\n")

			flags := tc.flags
			cmd := newVaultResolveCmd(&flags)
			cmd.SetArgs([]string{citekey, "--keep-vault", "--out", outDir})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.Execute()
			if nonGET != "" {
				t.Fatalf("%s: preview issued a non-GET request: %s", tc.name, nonGET)
			}
			if err == nil {
				t.Fatalf("%s: remotely-deleted note was not surfaced as an error during preview; output: %q", tc.name, out.String())
			}
			if strings.Contains(out.String(), "Would resolve") {
				t.Fatalf("%s: preview printed a false success message despite the remote 404: %q", tc.name, out.String())
			}
		})
	}
}

// TestVaultResolvePreviewPerformsLiveRead is the read half of the same
// regression guard: it proves the preview's getNote call reaches the server
// as a real GET rather than degrading to a client.DryRun stub, while still
// issuing zero non-GET requests. An accurate preview requires real remote
// state; the write-safety property is that it performs no writes, not that it
// performs no requests.
func TestVaultResolvePreviewPerformsLiveRead(t *testing.T) {
	const citekey = "cite1"
	const noteKey = "NOTEKEY1"
	const region = "vault copy wins"

	for _, tc := range []struct {
		name  string
		flags rootFlags
	}{
		{name: "dry-run", flags: rootFlags{dryRun: true}},
		{name: "bare", flags: rootFlags{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotLiveGET bool
			var nonGET string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					nonGET = r.Method + " " + r.URL.Path
					http.Error(w, "unexpected write under preview", http.StatusInternalServerError)
					return
				}
				if r.URL.Path == "/users/0/items/"+noteKey {
					gotLiveGET = true
				}
				w.Header().Set("Last-Modified-Version", "7")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"version":7,"data":{"note":"<p>live</p>"}}`))
			}))
			t.Cleanup(srv.Close)
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

			outDir := t.TempDir()
			state := pushState{
				Schema:      noteStateSchema,
				NoteKey:     noteKey,
				NoteVersion: 5,
				SourceHash:  sha256hex("original notes"),
				RemoteHash:  sha256hex(markdownToNoteHTML(citekey, "original notes")),
				Renderer:    vaultRenderer,
			}
			writeFile(t, filepath.Join(outDir, "note.md"), "---\nzotero_key: K1\ncitekey: "+citekey+"\n---\n\n## Notes\n"+
				vaultNotesBegin+"\n"+region+"\n"+vaultNotesEnd+"\n"+stateComment(state)+"\n")

			flags := tc.flags
			cmd := newVaultResolveCmd(&flags)
			cmd.SetArgs([]string{citekey, "--keep-vault", "--out", outDir})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("vault resolve preview (%s): %v", tc.name, err)
			}
			if nonGET != "" {
				t.Fatalf("%s: preview issued a non-GET request: %s", tc.name, nonGET)
			}
			if !gotLiveGET {
				t.Fatalf("%s: preview never reached the server with a live GET on the note endpoint (read was stubbed)", tc.name)
			}
			if !strings.Contains(out.String(), "Would resolve") {
				t.Fatalf("%s: expected a would-resolve preview message, got %q", tc.name, out.String())
			}
		})
	}
}
