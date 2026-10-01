// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

const (
	vltCitekey = "cite1"
	vltItemKey = "ITEMKEY1"
	vltNoteKey = "NOTEKEY1"
)

// vltZotero is a stateful fake of one Zotero child note on the Web API. It
// serves the batch version map and the note, applies a PATCH only when its
// If-Unmodified-Since-Version matches the live version (412 otherwise), and
// records every request that could read or change the note.
type vltZotero struct {
	mu        sync.Mutex
	version   int
	html      string
	parent    string
	patches   []string // If-Unmodified-Since-Version of each PATCH, in order
	writes    int      // every non-GET request
	noteReads int      // GETs of the note itself
}

func (z *vltZotero) snapshot() (version int, html string, patches []string, writes, noteReads int) {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.version, z.html, append([]string(nil), z.patches...), z.writes, z.noteReads
}

// vltServe starts z and points the CLI at it. userID, when non-empty, is the
// cached personal user ID, which makes the active library "users/<userID>".
func vltServe(t *testing.T, z *vltZotero, userID string) {
	t.Helper()
	notePath := "/users/0/items/" + vltNoteKey
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		z.mu.Lock()
		defer z.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/0/items":
			_ = json.NewEncoder(w).Encode(map[string]int{vltNoteKey: z.version})
		case r.Method == http.MethodGet && r.URL.Path == notePath:
			z.noteReads++
			w.Header().Set("Last-Modified-Version", strconv.Itoa(z.version))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"key":     vltNoteKey,
				"version": z.version,
				"data":    map[string]string{"itemType": "note", "parentItem": z.parent, "note": z.html},
			})
		case r.Method == http.MethodPatch && r.URL.Path == notePath:
			z.writes++
			pre := r.Header.Get("If-Unmodified-Since-Version")
			z.patches = append(z.patches, pre)
			if pre != strconv.Itoa(z.version) {
				http.Error(w, "precondition failed", http.StatusPreconditionFailed)
				return
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode PATCH body: %v", err)
			}
			z.html, _ = body["note"].(string)
			z.version++
			w.WriteHeader(http.StatusNoContent)
		default:
			z.writes++
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_USER_ID", "")
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if userID != "" {
		writeFile(t, cfgPath, "user_id = \""+userID+"\"\n")
	}
	t.Setenv("ZOTERO_CONFIG", cfgPath)
}

// vltWriteNote writes a managed vault note bound to vltNoteKey whose last
// pushed baseline is version 5 with Notes region "local notes".
func vltWriteNote(t *testing.T, outDir, library, region string) (string, []byte) {
	t.Helper()
	st := pushState{
		Schema:      noteStateSchema,
		NoteKey:     vltNoteKey,
		NoteVersion: 5,
		SourceHash:  sha256hex("local notes"),
		RemoteHash:  sha256hex(markdownToNoteHTML(vltCitekey, "local notes")),
		Renderer:    vaultRenderer,
	}
	fm := "---\nzotero_key: " + vltItemKey + "\ncitekey: " + vltCitekey + "\n"
	if library != "" {
		fm += "zotero_library: \"" + library + "\"\n"
	}
	path := filepath.Join(outDir, vltCitekey+".md")
	writeFile(t, path, fm+"---\n\n## Notes\n"+vaultNotesBegin+"\n"+region+"\n"+vaultNotesEnd+"\n"+stateComment(st)+"\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return path, before
}

func vltRun(t *testing.T, newCmd func(*rootFlags) *cobra.Command, flags rootFlags, args ...string) (string, error) {
	t.Helper()
	f := flags
	cmd := newCmd(&f)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	return out.String(), err
}

func vltAssertUnchanged(t *testing.T, path string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("vault note changed:\n%s", after)
	}
}

// TestVltVaultLibraryScopeGatesEveryDirection: a vault note that records its
// Zotero library is only read or written against that library. Pull and every
// resolve direction used to skip the check push makes, so a profile or account
// switch let them overwrite a same-key note in the wrong library. A recorded
// library with no resolvable active library fails closed.
func TestVltVaultLibraryScopeGatesEveryDirection(t *testing.T) {
	yes := rootFlags{yes: true, maxChanges: -1, asJSON: true}
	for _, tc := range []struct {
		name     string
		newCmd   func(*rootFlags) *cobra.Command
		userID   string
		region   string
		resolve  []string // resolve direction flags; nil for push/pull
		wantExit int
		wantSkip bool
	}{
		{name: "pull other library", newCmd: newVaultPullCmd, userID: "222", region: "local notes", wantSkip: true},
		{name: "pull unknown library", newCmd: newVaultPullCmd, region: "local notes", wantSkip: true},
		{name: "push unknown library", newCmd: newVaultPushCmd, region: "edited notes", wantSkip: true},
		{name: "resolve keep-vault", newCmd: newVaultResolveCmd, userID: "222", region: "edited notes", resolve: []string{"--keep-vault"}, wantExit: 9},
		{name: "resolve keep-remote", newCmd: newVaultResolveCmd, userID: "222", region: "edited notes", resolve: []string{"--keep-remote"}, wantExit: 9},
		{name: "resolve recreate", newCmd: newVaultResolveCmd, userID: "222", region: "edited notes", resolve: []string{"--keep-vault", "--recreate"}, wantExit: 9},
		{name: "resolve unknown library", newCmd: newVaultResolveCmd, region: "edited notes", resolve: []string{"--keep-vault"}, wantExit: 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z := &vltZotero{version: 6, html: markdownToNoteHTML(vltCitekey, "remote edit"), parent: vltItemKey}
			vltServe(t, z, tc.userID)
			outDir := t.TempDir()
			path, before := vltWriteNote(t, outDir, "users/111", tc.region)

			args := []string{"--out", outDir}
			if tc.resolve != nil {
				args = append(append([]string{vltCitekey}, tc.resolve...), args...)
			}
			out, err := vltRun(t, tc.newCmd, yes, args...)
			if tc.wantExit != 0 {
				if code := ExitCode(err); code != tc.wantExit {
					t.Fatalf("exit = %d (err %v), want %d", code, err, tc.wantExit)
				}
			} else if err != nil {
				t.Fatalf("run: %v", err)
			}
			if tc.wantSkip {
				var report vaultWriteReport
				if err := json.Unmarshal([]byte(out), &report); err != nil {
					t.Fatalf("decode report %q: %v", out, err)
				}
				if report.Counts["skipped"] != 1 {
					t.Fatalf("report = %+v, want the note skipped", report)
				}
			}
			_, _, _, writes, noteReads := z.snapshot()
			if writes != 0 || noteReads != 0 {
				t.Fatalf("library mismatch reached the note: writes=%d noteReads=%d", writes, noteReads)
			}
			vltAssertUnchanged(t, path, before)
		})
	}

	// Control: the same fixture in its own library pulls, so the check (not
	// the fixture) is what stopped every case above.
	t.Run("pull same library applies", func(t *testing.T) {
		z := &vltZotero{version: 6, html: markdownToNoteHTML(vltCitekey, "remote edit"), parent: vltItemKey}
		vltServe(t, z, "111")
		outDir := t.TempDir()
		path, _ := vltWriteNote(t, outDir, "users/111", "local notes")
		if _, err := vltRun(t, newVaultPullCmd, yes, "--out", outDir); err != nil {
			t.Fatalf("pull: %v", err)
		}
		if region, _ := extractNotesRegion(readNote(t, path)); region != "remote edit" {
			t.Fatalf("region = %q, want the pulled remote edit", region)
		}
	})
}

// TestVltVaultRefusesNoteOfAnotherParent: the state comment's note key is
// user-editable and survives copying a vault file, so a reachable key is not
// proof the note belongs to this item. Every direction must refuse a note that
// Zotero files under a different parent, before any write in either place.
func TestVltVaultRefusesNoteOfAnotherParent(t *testing.T) {
	yes := rootFlags{yes: true, maxChanges: -1, asJSON: true}
	for _, tc := range []struct {
		name     string
		newCmd   func(*rootFlags) *cobra.Command
		resolve  []string
		wantExit int
		wantErr  string
	}{
		{name: "push", newCmd: newVaultPushCmd, wantExit: 13, wantErr: "vault push:"},
		// Also the regression guard for pull failures that were reported as
		// "vault push" failures.
		{name: "pull", newCmd: newVaultPullCmd, wantExit: 13, wantErr: "vault pull:"},
		{name: "resolve keep-vault", newCmd: newVaultResolveCmd, resolve: []string{"--keep-vault"}, wantExit: 9, wantErr: "not a child note"},
		{name: "resolve keep-remote", newCmd: newVaultResolveCmd, resolve: []string{"--keep-remote"}, wantExit: 9, wantErr: "not a child note"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			foreign := markdownToNoteHTML(vltCitekey, "another item's note")
			z := &vltZotero{version: 6, html: foreign, parent: "OTHERKEY"}
			vltServe(t, z, "")
			outDir := t.TempDir()
			path, before := vltWriteNote(t, outDir, "", "edited notes")

			args := []string{"--out", outDir}
			if tc.resolve != nil {
				args = append(append([]string{vltCitekey}, tc.resolve...), args...)
			}
			_, err := vltRun(t, tc.newCmd, yes, args...)
			if code := ExitCode(err); code != tc.wantExit {
				t.Fatalf("exit = %d (err %v), want %d", code, err, tc.wantExit)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %q, want it to contain %q", err, tc.wantErr)
			}
			_, html, _, writes, _ := z.snapshot()
			if writes != 0 || html != foreign {
				t.Fatalf("foreign note was written: writes=%d", writes)
			}
			vltAssertUnchanged(t, path, before)
			if _, err := os.Stat(filepath.Join(outDir, vaultConflictsDir)); !os.IsNotExist(err) {
				t.Fatalf("refusal wrote a conflict artifact: %v", err)
			}
		})
	}
}

// TestVltVaultPushDivergedNoteWaitsForResolve drives the 412 arm end to end:
// both sides changed since the last push, so the guarded PATCH must 412 and the
// diverged Zotero note must survive a --yes push untouched (conflict artifact,
// exit 13). Only the explicit keep-vault resolution may overwrite it, and then
// only under the live version as precondition.
func TestVltVaultPushDivergedNoteWaitsForResolve(t *testing.T) {
	remote := markdownToNoteHTML(vltCitekey, "edited on another device")
	z := &vltZotero{version: 9, html: remote, parent: vltItemKey}
	vltServe(t, z, "")
	outDir := t.TempDir()
	path, before := vltWriteNote(t, outDir, "", "edited notes")
	yes := rootFlags{yes: true, maxChanges: -1, asJSON: true}

	out, err := vltRun(t, newVaultPushCmd, yes, "--out", outDir)
	if code := ExitCode(err); code != 13 {
		t.Fatalf("push exit = %d (err %v), want 13", code, err)
	}
	var report vaultWriteReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode report %q: %v", out, err)
	}
	if len(report.Results) != 1 || report.Results[0].Status != "conflict" {
		t.Fatalf("push results = %+v, want one conflict", report.Results)
	}
	version, html, patches, _, _ := z.snapshot()
	if html != remote || version != 9 {
		t.Fatalf("push overwrote the diverged Zotero note: version=%d html=%q", version, html)
	}
	if len(patches) != 1 || patches[0] != "5" {
		t.Fatalf("PATCH preconditions = %v, want exactly one at the recorded baseline 5", patches)
	}
	vltAssertUnchanged(t, path, before)
	confDir := filepath.Join(outDir, vaultConflictsDir)
	entries, err := os.ReadDir(confDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("conflict artifacts = %v (err %v), want 1", entries, err)
	}
	art, _ := os.ReadFile(filepath.Join(confDir, entries[0].Name()))
	if !strings.Contains(string(art), remote) {
		t.Fatalf("conflict artifact lacks the remote body:\n%s", art)
	}

	if _, err := vltRun(t, newVaultResolveCmd, yes, vltCitekey, "--keep-vault", "--out", outDir); err != nil {
		t.Fatalf("resolve --keep-vault --yes: %v", err)
	}
	version, html, patches, _, _ = z.snapshot()
	if want := markdownToNoteHTML(vltCitekey, "edited notes"); html != want {
		t.Fatalf("resolve did not write the vault copy: html=%q", html)
	}
	if len(patches) != 2 || patches[1] != "9" {
		t.Fatalf("PATCH preconditions = %v, want the resolve at live version 9", patches)
	}
	st, err := parseStateComment(readNote(t, path))
	if err != nil || st.NoteVersion != version {
		t.Fatalf("baseline = %+v (err %v), want version %d", st, err, version)
	}
	if _, err := os.Stat(filepath.Join(confDir, entries[0].Name())); !os.IsNotExist(err) {
		t.Fatalf("resolve left the conflict artifact (err=%v)", err)
	}
}

// TestVltVaultResolveJSONReport: under --json (which --agent implies) vault
// resolve reports a parseable result naming the direction and which side it
// changed, for previews and applied resolutions alike.
func TestVltVaultResolveJSONReport(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags rootFlags
		dir   string
		want  vaultResolveReport
	}{
		{
			name: "keep-vault preview", flags: rootFlags{asJSON: true}, dir: "--keep-vault",
			want: vaultResolveReport{DryRun: true, Direction: "keep-vault", Status: "would_resolve"},
		},
		{
			name: "keep-vault applied", flags: rootFlags{asJSON: true, yes: true, maxChanges: -1}, dir: "--keep-vault",
			want: vaultResolveReport{Direction: "keep-vault", Status: "resolved", ZoteroChanged: true},
		},
		{
			name: "keep-remote applied", flags: rootFlags{asJSON: true, yes: true, maxChanges: -1}, dir: "--keep-remote",
			want: vaultResolveReport{Direction: "keep-remote", Status: "resolved", VaultChanged: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z := &vltZotero{version: 6, html: markdownToNoteHTML(vltCitekey, "remote edit"), parent: vltItemKey}
			vltServe(t, z, "")
			outDir := t.TempDir()
			vltWriteNote(t, outDir, "", "edited notes")

			out, err := vltRun(t, newVaultResolveCmd, tc.flags, vltCitekey, tc.dir, "--out", outDir)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			var got vaultResolveReport
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("stdout is not a JSON report: %q (%v)", out, err)
			}
			want := tc.want
			want.CiteKey, want.ItemKey, want.NoteKey = vltCitekey, vltItemKey, vltNoteKey
			if got != want {
				t.Fatalf("report = %+v\nwant %+v", got, want)
			}
		})
	}
}

// TestVltSyncVaultNoteTransitions covers the per-note write state machine:
// a read failure is a per-note error, preview and unchanged content never
// write, a concurrent edit is re-merged exactly once, and a second concurrent
// edit leaves the file as the other writer left it (file_busy).
func TestVltSyncVaultNoteTransitions(t *testing.T) {
	oldMeta := vaultMeta{Key: vltItemKey, CiteKey: vltCitekey, Title: "Old title", ItemType: "journalArticle"}
	newMeta := oldMeta
	newMeta.Title = "New title"
	const filename = vltCitekey + ".md"

	setHook := func(t *testing.T, fn func()) {
		t.Helper()
		old := vaultBeforeFinalCompare
		vaultBeforeFinalCompare = fn
		t.Cleanup(func() { vaultBeforeFinalCompare = old })
	}
	appendLine := func(t *testing.T, path, line string) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read for concurrent edit: %v", err)
		}
		if err := os.WriteFile(path, append(b, []byte(line+"\n")...), 0o644); err != nil {
			t.Fatalf("concurrent edit: %v", err)
		}
	}
	seed := func(t *testing.T) (outDir, path string) {
		t.Helper()
		outDir = t.TempDir()
		res, err := syncVaultNote(oldMeta, nil, "obsidian", outDir, filename, false)
		if err != nil || res.Status != "created" {
			t.Fatalf("seed = %+v (err %v), want created", res, err)
		}
		return outDir, filepath.Join(outDir, filename)
	}

	t.Run("read error is a per-note error", func(t *testing.T) {
		outDir := t.TempDir()
		if err := os.Mkdir(filepath.Join(outDir, filename), 0o755); err != nil {
			t.Fatal(err)
		}
		res, err := syncVaultNote(oldMeta, nil, "obsidian", outDir, filename, false)
		if err != nil || res.Status != "error" || res.Note == "" {
			t.Fatalf("result = %+v (err %v), want status error with the read error", res, err)
		}
	})

	t.Run("preview never writes", func(t *testing.T) {
		outDir := t.TempDir()
		res, err := syncVaultNote(oldMeta, nil, "obsidian", outDir, filename, true)
		if err != nil || res.Status != "created" {
			t.Fatalf("result = %+v (err %v), want created", res, err)
		}
		if _, err := os.Stat(filepath.Join(outDir, filename)); !os.IsNotExist(err) {
			t.Fatalf("preview wrote the note (err=%v)", err)
		}
	})

	t.Run("unchanged never writes", func(t *testing.T) {
		outDir, _ := seed(t)
		setHook(t, func() { t.Error("unchanged note reached a write") })
		res, err := syncVaultNote(oldMeta, nil, "obsidian", outDir, filename, false)
		if err != nil || res.Status != "unchanged" {
			t.Fatalf("result = %+v (err %v), want unchanged", res, err)
		}
	})

	t.Run("one concurrent edit is re-merged", func(t *testing.T) {
		outDir, path := seed(t)
		calls := 0
		setHook(t, func() {
			calls++
			if calls == 1 {
				appendLine(t, path, "concurrent line")
			}
		})
		res, err := syncVaultNote(newMeta, nil, "obsidian", outDir, filename, false)
		if err != nil || res.Status != "updated" {
			t.Fatalf("result = %+v (err %v), want updated", res, err)
		}
		got := readNote(t, path)
		if !strings.Contains(got, "concurrent line") || !strings.Contains(got, "New title") {
			t.Fatalf("re-merge lost an edit (calls=%d):\n%s", calls, got)
		}
		if calls != 2 {
			t.Fatalf("write attempts = %d, want 2 (one re-merge)", calls)
		}
	})

	t.Run("repeated concurrent edits leave file_busy", func(t *testing.T) {
		outDir, path := seed(t)
		calls := 0
		setHook(t, func() {
			calls++
			appendLine(t, path, "concurrent edit "+strconv.Itoa(calls))
		})
		res, err := syncVaultNote(newMeta, nil, "obsidian", outDir, filename, false)
		if err != nil || res.Status != "file_busy" {
			t.Fatalf("result = %+v (err %v), want file_busy", res, err)
		}
		if calls != 2 {
			t.Fatalf("write attempts = %d, want 2 (exactly one re-merge)", calls)
		}
		got := readNote(t, path)
		if !strings.Contains(got, "concurrent edit 2") || strings.Contains(got, "New title") {
			t.Fatalf("file_busy clobbered the concurrent edit:\n%s", got)
		}
	})
}
