// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKeepRemoteResolve is the core guarantee: a managed remote note overwrites
// the local region, the per-note baseline advances to the live version, and the
// matching conflict artifact is removed.
func TestKeepRemoteResolve(t *testing.T) {
	outDir := t.TempDir()
	notePath := filepath.Join(outDir, "smith2024.md")
	writeFile(t, notePath, "---\nzotero_key: K1\n---\n\n## Notes\n"+vaultNotesBegin+"\nlocal stale edit\n"+vaultNotesEnd+"\n")

	old := pushState{Schema: noteStateSchema, NoteKey: "N1", NoteVersion: 3, SourceHash: "stale", RemoteHash: "stale", Renderer: vaultRenderer}
	if err := writeNoteState(notePath, old); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	// A conflict artifact that keepRemoteResolve must clear (matched by citekey--noteKey prefix).
	confDir := filepath.Join(outDir, vaultConflictsDir)
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(confDir, sanitizeVaultFilename("smith2024--N1")+"--remote-v5.md")
	writeFile(t, artifact, "conflict body")

	n := &pushNote{path: notePath, citekey: "smith2024", itemKey: "K1", region: "local stale edit", hasRegion: true, state: old}

	const remoteMD = "remote wins\nnew body"
	liveHTML := markdownToNoteHTML("smith2024", remoteMD)
	const liveVer = 5

	if err := keepRemoteResolve(outDir, n, liveVer, liveHTML); err != nil {
		t.Fatalf("keepRemoteResolve: %v", err)
	}

	s := readNote(t, notePath)
	region, ok := extractNotesRegion(s)
	if !ok || region != remoteMD {
		t.Errorf("region = (%q, %v), want %q", region, ok, remoteMD)
	}
	if strings.Contains(s, "local stale edit") {
		t.Errorf("local edit not discarded:\n%s", s)
	}
	got, perr := parseStateComment(s)
	if perr != nil {
		t.Fatalf("parse state: %v", perr)
	}
	want := pushState{Schema: noteStateSchema, NoteKey: "N1", NoteVersion: liveVer, SourceHash: sha256hex(remoteMD), RemoteHash: sha256hex(liveHTML), Renderer: vaultRenderer}
	if got != want {
		t.Errorf("baseline not refreshed:\n got %+v\nwant %+v", got, want)
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Errorf("conflict artifact not removed (err=%v)", err)
	}
}

// TestKeepRemoteResolveRefusesForeignHTML guards against importing an arbitrary
// Zotero note (one this CLI did not write) into the managed region.
func TestKeepRemoteResolveRefusesForeignHTML(t *testing.T) {
	outDir := t.TempDir()
	notePath := filepath.Join(outDir, "x.md")
	body := "---\n---\n\n## Notes\n" + vaultNotesBegin + "\nkeep me\n" + vaultNotesEnd + "\n"
	writeFile(t, notePath, body)

	n := &pushNote{path: notePath, citekey: "x", itemKey: "K", region: "keep me", hasRegion: true, state: pushState{NoteKey: "N9"}}

	err := keepRemoteResolve(outDir, n, 4, "<p>some unrelated Zotero note</p>")
	if err == nil || !strings.Contains(err.Error(), "managed shape") {
		t.Fatalf("err = %v, want a managed-shape refusal", err)
	}
	if got := readNote(t, notePath); got != body {
		t.Errorf("note must be untouched when remote is foreign:\n%s", got)
	}
}

// TestVaultResolveDirectionValidation: exactly one resolution direction is
// required, and the opposite/incompatible combinations are rejected before any
// network or filesystem access.
func TestVaultResolveDirectionValidation(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"smith2024", "--keep-vault", "--keep-remote"}, "opposite directions"},
		{[]string{"smith2024", "--keep-remote", "--recreate"}, "not valid with --keep-remote"},
		{[]string{"smith2024"}, "without a direction"},
	}
	for _, tc := range cases {
		cmd := newVaultResolveCmd(&rootFlags{})
		cmd.SetArgs(tc.args)
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("args %v: err = %v, want containing %q", tc.args, err, tc.want)
		}
	}
}
