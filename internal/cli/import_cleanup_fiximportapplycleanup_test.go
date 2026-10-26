// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/mutation"
)

type importCleanupFailWriter struct{}

func (importCleanupFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestImportApplyIntentCleanupAfterOutput(t *testing.T) {
	for _, scenario := range []string{"success", "output_failure", "refresh_cancellation", "missing_run_id"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			previous := mutationJournalRecorder
			mutationJournalRecorder = recordMutationJournal
			if scenario == "missing_run_id" {
				mutationJournalRecorder = func(env *mutation.Envelope) error {
					env.Journal = map[string]any{}
					return nil
				}
			}
			t.Cleanup(func() { mutationJournalRecorder = previous })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pdf := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))
			hash, err := fileMD5(pdf)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:23119")
			if err != nil {
				t.Skipf("port 23119 is unavailable: %v", err)
			}
			var refreshed atomic.Bool
			var attachmentURL atomic.Value
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/connector/ping":
					w.WriteHeader(http.StatusOK)
				case "/connector/saveItems":
					w.WriteHeader(http.StatusCreated)
				case "/connector/saveAttachment":
					var metadata struct{ URL string }
					if err := json.Unmarshal([]byte(r.Header.Get("X-Metadata")), &metadata); err != nil {
						t.Error(err)
					}
					attachmentURL.Store(metadata.URL)
					w.WriteHeader(http.StatusCreated)
				case "/api/users/0/items/top":
					_ = json.NewEncoder(w).Encode([]any{map[string]any{
						"key": "PARENTAA", "title": "Paper A", "itemType": "journalArticle", "dateAdded": time.Now().UTC().Format(time.RFC3339),
					}})
				case "/api/users/0/items/PARENTAA/children":
					_ = json.NewEncoder(w).Encode([]any{map[string]any{"key": "ATTACHAA", "data": map[string]any{
						"itemType": "attachment", "md5": hash, "url": attachmentURL.Load(),
					}}})
				case "/api/users/0/items":
					refreshed.Store(true)
					if scenario == "refresh_cancellation" {
						cancel()
					}
					w.Header().Set("Last-Modified-Version", "1")
					_, _ = io.WriteString(w, "[]")
				default:
					http.NotFound(w, r)
				}
			}))
			server.Listener = listener
			server.Start()
			t.Cleanup(server.Close)
			manifest := writeImportApplyTestManifest(t, importManifest{SchemaVersion: importManifestSchemaVersion, Entries: []importManifestEntry{{
				Path: pdf, Action: "create", Status: "resolved", Title: "Paper A", Item: map[string]any{"itemType": "journalArticle", "title": "Paper A"},
			}}})
			flags := &rootFlags{asJSON: true, yes: true, via: "connector", timeout: time.Second, maxChanges: -1, configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0")}
			cmd := newImportApplyCmd(flags)
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			if scenario == "output_failure" {
				cmd.SetOut(importCleanupFailWriter{})
			}
			cmd.SetArgs([]string{"--attach-mode", "stored", manifest})
			err = cmd.ExecuteContext(ctx)
			if scenario == "output_failure" {
				if !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("output error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("import failed: %v", err)
			}
			if !refreshed.Load() {
				t.Fatal("import did not reach the post-journal refresh")
			}
			if scenario != "missing_run_id" {
				entries, err := mutation.ListEntries(helpersTestJournalDir(t))
				if err != nil || len(entries) != 1 {
					t.Fatalf("journal entries = %v, error = %v", entries, err)
				}
			}
			journal, err := journalDir()
			if err != nil {
				t.Fatal(err)
			}
			paths, err := filepath.Glob(filepath.Join(journal+".import-pending", "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if scenario == "success" {
				want = 0
			}
			if len(paths) != want {
				t.Fatalf("intent records = %v, want %d", paths, want)
			}
		})
	}
}
