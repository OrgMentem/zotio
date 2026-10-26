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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/config"
)

const (
	vaultRecoveryParent = "PARENTK2"
	vaultRecoveryKey    = "ABCD2345"
	vaultRecoveryCite   = "recovery2026"
)

type vaultRecoveryRemote struct {
	mu           sync.Mutex
	html         string
	version      int
	posts        int
	patches      int
	failReadBack bool
}

func (z *vaultRecoveryRemote) serve(w http.ResponseWriter, r *http.Request) {
	z.mu.Lock()
	defer z.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/items/"+vaultRecoveryParent+"/children":
		rows := []map[string]any{}
		if z.version != 0 {
			rows = append(rows, map[string]any{"key": vaultRecoveryKey, "data": map[string]any{
				"itemType": "note", "parentItem": vaultRecoveryParent, "note": z.html,
			}})
		}
		_ = json.NewEncoder(w).Encode(rows)
	case r.Method == http.MethodPost && r.URL.Path == "/items":
		var rows []struct {
			Note string `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&rows); err != nil || len(rows) != 1 {
			http.Error(w, "bad create", http.StatusBadRequest)
			return
		}
		z.posts++
		z.html, z.version = rows[0].Note, 8
		_ = json.NewEncoder(w).Encode(map[string]any{"success": map[string]string{"0": vaultRecoveryKey}})
	case r.Method == http.MethodGet && r.URL.Path == "/items/"+vaultRecoveryKey:
		if z.failReadBack && z.posts+z.patches > 0 {
			http.Error(w, "read-back unavailable", http.StatusForbidden)
			return
		}
		w.Header().Set("Last-Modified-Version", strconv.Itoa(z.version))
		_ = json.NewEncoder(w).Encode(map[string]any{"key": vaultRecoveryKey, "version": z.version,
			"data": map[string]any{"itemType": "note", "parentItem": vaultRecoveryParent, "note": z.html},
		})
	case r.Method == http.MethodPatch && r.URL.Path == "/items/"+vaultRecoveryKey:
		if r.Header.Get("If-Unmodified-Since-Version") != strconv.Itoa(z.version) {
			http.Error(w, "stale version", http.StatusPreconditionFailed)
			return
		}
		var body struct {
			Note string `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad patch", http.StatusBadRequest)
			return
		}
		z.patches++
		z.version++
		z.html = body.Note
		w.Header().Set("Last-Modified-Version", strconv.Itoa(z.version))
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func vaultRecoveryClient(t *testing.T, z *vaultRecoveryRemote) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(z.serve))
	t.Cleanup(srv.Close)
	c := client.New(&config.Config{BaseURL: srv.URL}, time.Second, 0)
	c.NoCache = true
	return c
}

func vaultRecoveryParse(t *testing.T, path string) *pushNote {
	t.Helper()
	n, err := parsePushNote(path)
	if err != nil || n == nil {
		t.Fatalf("parse note: %v, note=%+v", err, n)
	}
	return n
}

func vaultRecoveryFixture(t *testing.T, region string, st pushState) *pushNote {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recovery.md")
	body := "---\nzotero_key: " + vaultRecoveryParent + "\ncitekey: " + vaultRecoveryCite + "\n---\n\n## Notes\n" +
		vaultNotesBegin + "\n" + region + "\n" + vaultNotesEnd + "\n"
	if st.NoteKey != "" {
		body += stateComment(st) + "\n"
	}
	writeFile(t, path, body)
	return vaultRecoveryParse(t, path)
}

func vaultRecoveryEdit(t *testing.T, path, region string) {
	t.Helper()
	body, ok := replaceNotesRegion(readNote(t, path), region)
	if !ok {
		t.Fatal("fixture lost its notes markers")
	}
	writeFile(t, path, body)
}

func vaultRecoveryBaseline(region string) pushState {
	return pushState{Schema: noteStateSchema, NoteKey: vaultRecoveryKey, NoteVersion: 7,
		SourceHash: sha256hex(region), RemoteHash: sha256hex(markdownToNoteHTML(vaultRecoveryCite, region)), Renderer: vaultRenderer}
}

func TestVaultPullRejectsEditAfterSnapshot(t *testing.T) {
	st := vaultRecoveryBaseline("original")
	n := vaultRecoveryFixture(t, "original", st)
	outDir := filepath.Dir(n.path)
	notes, err := loadPushNotes(outDir)
	if err != nil || len(notes) != 1 {
		t.Fatalf("load notes: %v, %d notes", err, len(notes))
	}
	z := &vaultRecoveryRemote{version: 8, html: markdownToNoteHTML(vaultRecoveryCite, "remote edit")}
	c := vaultRecoveryClient(t, z)
	flags := &rootFlags{maxChanges: -1}
	remote := &remoteNoteState{}
	if plan := pullOne(c, outDir, "", notes[0], flags, true, remote); plan.Status != "would_pull" {
		t.Fatalf("plan = %+v", plan)
	}
	vaultRecoveryEdit(t, n.path, "typed during pull")
	res := pullOne(c, outDir, "", notes[0], flags, false, remote)
	if res.Status != "conflict" || !strings.Contains(res.Note, "retry") {
		t.Fatalf("pull = %+v, want conflict with retry guidance", res)
	}
	after := vaultRecoveryParse(t, n.path)
	if after.region != "typed during pull" || after.state != st {
		t.Fatalf("pull lost local text or advanced baseline: %+v", after)
	}
}

func TestVaultPushRecoversLostBinding(t *testing.T) {
	for _, failure := range []string{"file_busy", "read_back"} {
		t.Run(failure, func(t *testing.T) {
			z := &vaultRecoveryRemote{failReadBack: failure == "read_back"}
			c := vaultRecoveryClient(t, z)
			n := vaultRecoveryFixture(t, "first write", pushState{})
			outDir := filepath.Dir(n.path)
			oldHook := vaultBeforeFinalCompare
			t.Cleanup(func() { vaultBeforeFinalCompare = oldHook })
			if failure == "file_busy" {
				vaultBeforeFinalCompare = func() {
					vaultBeforeFinalCompare = nil
					vaultRecoveryEdit(t, n.path, "edited after create")
				}
			}
			flags := &rootFlags{maxChanges: -1}
			first := pushOne(c, outDir, "", n, nil, flags, false)
			if first.Status != "failed" {
				t.Fatalf("first push = %+v, want failed baseline save", first)
			}
			z.mu.Lock()
			z.failReadBack = false
			z.mu.Unlock()
			vaultRecoveryEdit(t, n.path, "edited after create")
			n = vaultRecoveryParse(t, n.path)
			if n.state.NoteKey != "" {
				t.Fatalf("fixture did not lose the binding: %+v", n.state)
			}
			second := pushOne(c, outDir, "", n, nil, flags, false)
			if second.Status != "reconciled" || second.NoteKey != vaultRecoveryKey {
				t.Fatalf("retry = %+v, want adopted original note", second)
			}
			after := vaultRecoveryParse(t, n.path)
			if after.state.NoteKey != vaultRecoveryKey || after.region != "edited after create" || after.state.SourceHash != sha256hex("first write") {
				t.Fatalf("adoption lost edits or claimed they were already pushed: %+v", after)
			}
			third := pushOne(c, outDir, "", after, map[string]int{vaultRecoveryKey: 8}, flags, false)
			if third.Status != "updated" {
				t.Fatalf("push after adoption = %+v", third)
			}
			z.mu.Lock()
			defer z.mu.Unlock()
			if z.posts != 1 || z.patches != 1 || z.html != markdownToNoteHTML(vaultRecoveryCite, "edited after create") {
				t.Fatalf("posts=%d patches=%d remote=%q", z.posts, z.patches, z.html)
			}
		})
	}
}

func TestVaultPushRecoversLostBaseline(t *testing.T) {
	for _, direction := range []string{"push", "pull"} {
		for _, failure := range []string{"file_busy", "read_back"} {
			for _, furtherEdit := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/further_edit=%v", direction, failure, furtherEdit), func(t *testing.T) {
					st := vaultRecoveryBaseline("original")
					n := vaultRecoveryFixture(t, "first update", st)
					outDir := filepath.Dir(n.path)
					z := &vaultRecoveryRemote{version: 7, html: markdownToNoteHTML(vaultRecoveryCite, "original"), failReadBack: failure == "read_back"}
					c := vaultRecoveryClient(t, z)
					oldHook := vaultBeforeFinalCompare
					t.Cleanup(func() { vaultBeforeFinalCompare = oldHook })
					if failure == "file_busy" {
						vaultBeforeFinalCompare = func() {
							z.mu.Lock()
							landed := z.patches > 0
							z.mu.Unlock()
							if landed {
								vaultBeforeFinalCompare = nil
								writeFile(t, n.path, readNote(t, n.path)+"\nconcurrent outside edit\n")
							}
						}
					}
					flags := &rootFlags{maxChanges: -1}
					first := pushOne(c, outDir, "", n, map[string]int{vaultRecoveryKey: 7}, flags, false)
					if first.Status != "failed" {
						t.Fatalf("first push = %+v, want lost baseline", first)
					}
					z.mu.Lock()
					z.failReadBack = false
					z.mu.Unlock()
					wantRegion := "first update"
					if furtherEdit {
						wantRegion = "second local edit"
						vaultRecoveryEdit(t, n.path, wantRegion)
					}
					n = vaultRecoveryParse(t, n.path)
					if n.state.NoteVersion != 7 || n.state.PendingSourceHash != sha256hex("first update") || n.state.PendingRemoteHash != sha256hex(markdownToNoteHTML(vaultRecoveryCite, "first update")) {
						t.Fatalf("pending write not retained: %+v", n.state)
					}
					before := readNote(t, n.path)
					var plan, result pushResult
					if direction == "push" {
						plan = pushOne(c, outDir, "", n, map[string]int{vaultRecoveryKey: 8}, flags, true)
					} else {
						plan = pullOne(c, outDir, "", n, flags, true)
					}
					if strings.Contains(plan.Status, "conflict") || readNote(t, n.path) != before || n.state.PendingSourceHash == "" {
						t.Fatalf("preview consumed recovery or reported a conflict: %+v", plan)
					}
					if direction == "push" {
						result = pushOne(c, outDir, "", n, map[string]int{vaultRecoveryKey: 8}, flags, false)
					} else {
						result = pullOne(c, outDir, "", n, flags, false)
					}
					if result.Status != "converged" && result.Status != "updated" {
						t.Fatalf("recovery = %+v", result)
					}
					after := vaultRecoveryParse(t, n.path)
					wantSource := "first update"
					if direction == "push" {
						wantSource = wantRegion
					}
					if after.region != wantRegion || after.state.PendingRemoteHash != "" || after.state.PendingSourceHash != "" || after.state.SourceHash != sha256hex(wantSource) || after.state.NoteVersion < 8 {
						t.Fatalf("bad recovered baseline or lost local text: %+v", after)
					}
					if _, err := os.Stat(filepath.Join(outDir, vaultConflictsDir)); !os.IsNotExist(err) {
						t.Fatalf("recovery wrote conflict artifacts: %v", err)
					}
				})
			}
		}
	}
}

func TestVaultReportsDistinguishPreviewFromDryRun(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		yes, dryRun, preview bool
	}{
		{name: "default", preview: true},
		{name: "explicit dry run", dryRun: true, preview: true},
		{name: "dry run beats yes", yes: true, dryRun: true, preview: true},
		{name: "apply", yes: true},
	} {
		for _, direction := range []string{"sync", "push", "pull"} {
			t.Run(direction+"/"+tc.name, func(t *testing.T) {
				flags := &rootFlags{asJSON: true, yes: tc.yes, dryRun: tc.dryRun, maxChanges: -1}
				cmd := &cobra.Command{Use: direction}
				var out bytes.Buffer
				cmd.SetOut(&out)
				var err error
				if direction == "sync" {
					err = printVaultSyncReport(cmd, nil, "/vault", "obsidian", flags)
				} else {
					err = printVaultWriteReport(cmd, nil, "/vault", flags, !resolveMutationMode(flags).Apply, "Done", "Would write")
				}
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]any
				if err := json.Unmarshal(out.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got["preview"] != tc.preview || got["dry_run"] != tc.dryRun || got["out"] != "/vault" || got["counts"] == nil {
					t.Fatalf("report = %s, want preview=%v dry_run=%v and the bespoke fields", out.String(), tc.preview, tc.dryRun)
				}
			})
		}
	}
}

func TestVaultPushRefusesAmbiguousLostBinding(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			posts++
			_, _ = w.Write([]byte(`{"success":{"0":"ABCD2347"}}`))
			return
		}
		rows := []map[string]any{}
		for _, key := range []string{"ABCD2345", "ABCD2346"} {
			rows = append(rows, map[string]any{"key": key, "data": map[string]any{
				"itemType": "note", "parentItem": vaultRecoveryParent,
				"note": markdownToNoteHTML(vaultRecoveryCite, "previous notes"),
			}})
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(srv.Close)
	c := client.New(&config.Config{BaseURL: srv.URL}, time.Second, 0)
	c.NoCache = true
	n := vaultRecoveryFixture(t, "changed local notes", pushState{})
	before := readNote(t, n.path)
	res := pushOne(c, filepath.Dir(n.path), "", n, nil, &rootFlags{maxChanges: -1}, false)
	if res.Status != "conflict" || res.Recovery == nil || len(res.Recovery.CandidateKeys) != 2 {
		t.Fatalf("ambiguous adoption = %+v, want both candidates and no new note", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 0 || readNote(t, n.path) != before {
		t.Fatalf("ambiguous adoption wrote a note: posts=%d", posts)
	}
}

func TestVaultPendingIntentClearedAfterRejectedPatch(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusPreconditionFailed} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			baseline := vaultRecoveryBaseline("original")
			n := vaultRecoveryFixture(t, "local edit", baseline)
			before := readNote(t, n.path)
			desired := markdownToNoteHTML(vaultRecoveryCite, "local edit")
			sourceHash := sha256hex("local edit")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, readErr := os.ReadFile(n.path)
				pending, parseErr := parseStateComment(string(data))
				if readErr != nil || parseErr != nil || pending.PendingSourceHash != sourceHash || pending.PendingRemoteHash != sha256hex(desired) {
					t.Errorf("PATCH reached server without durable intent: state=%+v read=%v parse=%v", pending, readErr, parseErr)
				}
				http.Error(w, "rejected", status)
			}))
			t.Cleanup(srv.Close)
			c := client.New(&config.Config{BaseURL: srv.URL}, time.Second, 0)
			c.NoCache = true
			_, err := patchPendingNote(c, n, sourceHash, desired, baseline.NoteVersion)
			if apiStatus(err) != status {
				t.Fatalf("PATCH error = %v, want HTTP %d", err, status)
			}
			if readNote(t, n.path) != before || n.state != baseline {
				t.Fatalf("rejected PATCH left intent or changed baseline: %+v", n.state)
			}
		})
	}
}

func TestVaultPendingIntentClearedAfterRejectedRetry(t *testing.T) {
	baseline := vaultRecoveryBaseline("original")
	n := vaultRecoveryFixture(t, "local edit", baseline)
	before := readNote(t, n.path)
	desired := markdownToNoteHTML(vaultRecoveryCite, "local edit")
	var mu sync.Mutex
	patches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		data, readErr := os.ReadFile(n.path)
		pending, parseErr := parseStateComment(string(data))
		if readErr != nil || parseErr != nil {
			t.Errorf("read pending state: %v, %v", readErr, parseErr)
		}
		if r.Method == http.MethodPatch {
			patches++
			if pending.PendingSourceHash != sha256hex("local edit") || pending.PendingRemoteHash != sha256hex(desired) {
				t.Errorf("PATCH %d has no durable intent: %+v", patches, pending)
			}
			status := http.StatusPreconditionFailed
			if patches > 1 {
				status = http.StatusForbidden
			}
			http.Error(w, "rejected", status)
			return
		}
		if pending != baseline {
			t.Errorf("confirmed 412 left intent before classification: %+v", pending)
		}
		w.Header().Set("Last-Modified-Version", "8")
		_ = json.NewEncoder(w).Encode(map[string]any{"version": 8,
			"data": map[string]any{"itemType": "note", "parentItem": vaultRecoveryParent,
				"note": markdownToNoteHTML(vaultRecoveryCite, "original")},
		})
	}))
	t.Cleanup(srv.Close)
	c := client.New(&config.Config{BaseURL: srv.URL}, time.Second, 0)
	c.NoCache = true
	res := patchWithConflict(c, filepath.Dir(n.path), n, sha256hex("local edit"), desired, &rootFlags{maxChanges: -1})
	if res.Status != "failed" || readNote(t, n.path) != before || n.state != baseline {
		t.Fatalf("rejected retry left intent or changed baseline: result=%+v state=%+v", res, n.state)
	}
	mu.Lock()
	defer mu.Unlock()
	if patches != 2 {
		t.Fatalf("PATCH count = %d, want initial request and retry", patches)
	}
}
