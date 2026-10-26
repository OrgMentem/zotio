// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/connector"
	"zotio/internal/mutation"
	"zotio/internal/store"
)

func importfileRunCommand(t *testing.T, ctx context.Context, cmd *cobra.Command, args ...string) (mutation.Envelope, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(args)
	err := cmd.Execute()
	var env mutation.Envelope
	if out.Len() != 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode output %s: %v", out.String(), decodeErr)
		}
	}
	return env, err
}

func importfileJournal(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_DATA_DIR", t.TempDir())
	old := mutationJournalRecorder
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = old })
}

func TestImportFileWebCreatedKeysJournalUndoAndMirror(t *testing.T) {
	importfileJournal(t)
	seedWriteThroughItem(t, "ZZZZ2345", `{"key":"ZZZZ2345","version":1,"data":{"key":"ZZZZ2345","itemType":"book","title":"Existing"}}`)
	seed, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.SaveSyncState("items", "", 1); err != nil {
		_ = seed.Close()
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	oldMirror := mirrorWriteThrough
	mirrorWriteThrough = applyMirrorWriteThrough
	t.Cleanup(func() { mirrorWriteThrough = oldMirror })
	const alphabet = "23456789ABCDEFGHIJKLMNPQRSTUVWXYZ"
	keys := make([]string, 51)
	var content strings.Builder
	for i := range keys {
		keys[i] = "ABCD23" + string(alphabet[i/len(alphabet)]) + string(alphabet[i%len(alphabet)])
		fmt.Fprintf(&content, "@article{r%d, title={Record %d}}\n", i, i)
	}
	var offset int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var items []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&items); err != nil {
			t.Errorf("request: %v", err)
		}
		successful := make(map[string]any)
		for i := range items {
			successful[strconv.Itoa(i)] = map[string]any{"key": keys[offset+i]}
		}
		offset += len(items)
		_ = json.NewEncoder(w).Encode(map[string]any{"successful": successful, "failed": map[string]any{}})
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
	env, err := importfileRunCommand(t, context.Background(), newImportFileCmd(flags), writeImportFixture(t, content.String()))
	if err != nil || env.Result == nil || env.Result.Summary.Applied != len(keys) {
		t.Fatalf("result=%+v err=%v", env.Result, err)
	}
	for i, item := range env.Result.Items {
		if item.Key != keys[i] {
			t.Fatalf("record %d key=%q want %q", i, item.Key, keys[i])
		}
	}
	entries, err := mutation.ListEntries(helpersTestJournalDir(t))
	if err != nil || len(entries) != 1 {
		t.Fatalf("journal=%+v err=%v", entries, err)
	}
	inverse, refused := mutation.InverseOps(entries[0])
	if len(inverse) != len(keys) || len(refused) != 0 {
		t.Fatalf("undo operations=%d refusals=%+v", len(inverse), refused)
	}
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, key := range keys {
		rows, err := (localQueryStore{db}).QueryRaw("SELECT id FROM resources WHERE resource_type='items' AND id=?", key)
		if err != nil || len(rows) != 1 {
			t.Fatalf("mirrored key %s rows=%v err=%v", key, rows, err)
		}
	}
}

func TestImportFileAndCollectionsCancelPostedBatch(t *testing.T) {
	for _, kind := range []string{"file", "collections"} {
		t.Run(kind, func(t *testing.T) {
			importfileJournal(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			oldOutcome := mutationOutcomeRecorder
			t.Cleanup(func() { mutationOutcomeRecorder = oldOutcome })
			// Cancel after the first outcome: the complete batch response has
			// arrived, but the engine has not collected its second record yet.
			mutationOutcomeRecorder = func(_ string, _ string, _ string, _ mutation.Op, _ mutation.ResultItem) error {
				cancel()
				return nil
			}
			var posts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				posts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"successful":{"0":{"key":"ABCD2345"},"1":{"key":"BCDE2345"}},"failed":{}}`)
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
			var cmd *cobra.Command
			var args []string
			if kind == "file" {
				cmd = newImportFileCmd(flags)
				args = []string{writeImportFixture(t, "@article{a,title={One}}\n@article{b,title={Two}}")}
			} else {
				cmd = newCollectionsCreateCmd(flags)
				cmd.SetIn(strings.NewReader(`[{"name":"One"},{"name":"Two"}]`))
				args = []string{"--stdin"}
			}
			env, err := importfileRunCommand(t, ctx, cmd, args...)
			if err != nil || posts.Load() != 1 || env.Result == nil || env.Result.Summary.Applied != 2 {
				t.Fatalf("posts=%d result=%+v err=%v", posts.Load(), env.Result, err)
			}
			for _, item := range env.Result.Items {
				if item.Status != "applied" || item.Key == "" {
					t.Fatalf("posted item lost its outcome: %+v", item)
				}
			}
			entries, err := mutation.ListEntries(helpersTestJournalDir(t))
			if err != nil || len(entries) != 1 || len(entries[0].Ops) != 2 {
				t.Fatalf("journal=%+v err=%v", entries, err)
			}
		})
	}
}

func TestImportFileConnectorGuardSurvivesFilingCancellation(t *testing.T) {
	importfileJournal(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	content := []byte("@article{a,title={One}}\n@article{b,title={Two}}")
	pendingPath, _, err := importFilePendingPath(content, "bibtex")
	if err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connector/ping":
			w.WriteHeader(http.StatusOK)
		case "/connector/import":
			posts.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `[{"key":"ABCD2345"},{"key":"BCDE2345"}]`)
		case "/connector/updateSession":
			if _, err := os.Stat(pendingPath); err != nil {
				t.Errorf("guard disappeared before filing: %v", err)
			}
			_, _ = io.Copy(io.Discard, r.Body)
			cancel()
			http.Error(w, "filing interrupted", http.StatusInternalServerError)
		default:
			w.Header().Set("Last-Modified-Version", "1")
			_, _ = io.WriteString(w, `[]`)
		}
	}))
	defer srv.Close()
	redirectConnectorDialsToFixture(t, srv)
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:23119/api/users/0")
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1, via: "connector", connectorTarget: "C1", timeout: time.Second,
		configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0")}
	path := writeImportFixture(t, string(content))
	env, _ := importfileRunCommand(t, ctx, newImportFileCmd(flags), path)
	if env.Result == nil || env.Result.Summary.Applied != 2 {
		t.Fatalf("canceled committed batch=%+v", env.Result)
	}
	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatalf("canceled run lost guard: %v", err)
	}
	entries, err := mutation.ListEntries(helpersTestJournalDir(t))
	if err != nil || len(entries) != 1 || len(entries[0].Ops) != 2 {
		t.Fatalf("journal=%+v err=%v", entries, err)
	}
	_, err = importfileRunCommand(t, context.Background(), newImportFileCmd(flags), path)
	if err == nil || posts.Load() != 1 {
		t.Fatalf("rerun err=%v import count=%d", err, posts.Load())
	}
}

func TestImportFileConnectorGuardRequiresDurableJournal(t *testing.T) {
	for _, journalFails := range []bool{false, true} {
		t.Run(fmt.Sprint(journalFails), func(t *testing.T) {
			importfileJournal(t)
			content := []byte("@article{a,title={One}}")
			pendingPath, _, err := importFilePendingPath(content, "bibtex")
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/connector/ping":
					w.WriteHeader(http.StatusOK)
				case "/connector/import":
					w.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(w, `[{"key":"ABCD2345"}]`)
				default:
					w.Header().Set("Last-Modified-Version", "1")
					_, _ = io.WriteString(w, `[]`)
				}
			}))
			defer srv.Close()
			redirectConnectorDialsToFixture(t, srv)
			t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:23119/api/users/0")
			mutationJournalRecorder = func(env *mutation.Envelope) error {
				if _, err := os.Stat(pendingPath); err != nil {
					t.Errorf("guard absent before journal publication: %v", err)
				}
				if journalFails {
					return errors.New("journal disk full")
				}
				return recordMutationJournal(env)
			}
			flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1, via: "connector", timeout: time.Second,
				configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0")}
			env, err := importfileRunCommand(t, context.Background(), newImportFileCmd(flags), writeImportFixture(t, string(content)))
			if err != nil || env.Result == nil || env.Result.Summary.Applied != 1 {
				t.Fatalf("result=%+v err=%v", env.Result, err)
			}
			_, statErr := os.Stat(pendingPath)
			if journalFails && statErr != nil {
				t.Fatalf("failed journal lost guard: %v", statErr)
			}
			if !journalFails && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("completed journal retained guard: %v", statErr)
			}
		})
	}
}

func TestImportFileUnknownExtensionNamesInput(t *testing.T) {
	for _, name := range []string{"notes.txt", "notes"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(path, []byte("@article{a,title={One}}"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := importfileRunCommand(t, context.Background(), newImportFileCmd(&rootFlags{maxChanges: -1}), path)
			extension := filepath.Ext(path)
			if extension == "" {
				extension = "no extension"
			}
			if err == nil || !strings.Contains(err.Error(), extension) || !strings.Contains(err.Error(), "--format bibtex, ris, or csljson") || !strings.Contains(err.Error(), name) {
				t.Fatalf("error=%v", err)
			}
			_, err = importfileRunCommand(t, context.Background(), newImportFileCmd(&rootFlags{maxChanges: -1}), "--format", "wrong", path)
			if err == nil || !strings.Contains(err.Error(), `unknown format "wrong"`) {
				t.Fatalf("explicit format error=%v", err)
			}
		})
	}
}

func TestImportPDFUnknownSaveJournalsAndRefusesReplay(t *testing.T) {
	for _, failure := range []string{"500", "disconnect"} {
		t.Run(failure, func(t *testing.T) {
			importfileJournal(t)
			var saves atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/connector/saveStandaloneAttachment" {
					_, _ = io.WriteString(w, `[]`)
					return
				}
				saves.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if failure == "500" {
					http.Error(w, "saved then failed", http.StatusInternalServerError)
					return
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/api/users/0")
			flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1, configPath: testConfigFile(t, srv.URL+"/api/users/0")}
			path := writeImportPDFFixture(t, t.TempDir(), "unknown.pdf")
			conn := connector.New(srv.URL+"/connector", time.Second)
			run := func() (mutation.Envelope, error) {
				op := importPDFOp(testApplyCmd(), flags, conn, path, filepath.Base(path), 1, "")
				return runMutation(context.Background(), flags, "import.pdf", []mutation.Op{op})
			}
			env, err := run()
			if err == nil || env.Result == nil || env.Result.Summary.Conflicts != 1 || env.Result.Summary.Failed != 0 {
				t.Fatalf("result=%+v err=%v", env.Result, err)
			}
			detail, _ := env.Result.Items[0].Reason.(map[string]any)
			if detail["committed"] != true || detail["session"] == nil || detail["imported_after"] == nil {
				t.Fatalf("missing reconciliation evidence: %+v", detail)
			}
			entries, err := mutation.ListEntries(helpersTestJournalDir(t))
			if err != nil || len(entries) != 1 || len(entries[0].Ops) != 1 {
				t.Fatalf("journal=%+v err=%v", entries, err)
			}
			_, err = run()
			if err == nil || saves.Load() != 1 {
				t.Fatalf("replay err=%v save count=%d", err, saves.Load())
			}
		})
	}
}
