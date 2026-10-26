// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/connector"
	"zotio/internal/mutation"
)

type importfileBrokenOutput struct{}

func (importfileBrokenOutput) Write([]byte) (int, error) {
	return 0, errors.New("output closed")
}

func TestImportPDFGuardSurvivesUntilJournalAndRendering(t *testing.T) {
	for _, operation := range []string{"import.pdf", "import.apply"} {
		for _, failure := range []string{"none", "recognition", "recognition_cancel", "filing", "journal", "output", "cancel_after_journal", "unresolved"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				importfileJournal(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				path := writeImportPDFFixture(t, t.TempDir(), "complete.pdf")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				pendingPath, _, err := importFilePendingPath(data, "pdf")
				if err != nil {
					t.Fatal(err)
				}
				var saves atomic.Int32
				var recognition atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/connector/saveStandaloneAttachment":
						saves.Add(1)
						w.WriteHeader(http.StatusCreated)
						_, _ = io.WriteString(w, `{"canRecognize":true}`)
					case "/connector/getRecognizedItem":
						recognition.Add(1)
						if _, err := os.Stat(pendingPath); err != nil {
							t.Errorf("guard absent during recognition: %v", err)
						}
						if failure == "recognition_cancel" {
							cancel()
						}
						if failure == "recognition" || failure == "recognition_cancel" {
							http.Error(w, "recognition interrupted", http.StatusInternalServerError)
							return
						}
						_, _ = io.WriteString(w, `{"title":"Completed Paper","itemType":"journalArticle"}`)
					case "/connector/updateSession":
						if _, err := os.Stat(pendingPath); err != nil {
							t.Errorf("guard absent during filing: %v", err)
						}
						if failure == "filing" {
							http.Error(w, "filing interrupted", http.StatusInternalServerError)
							return
						}
						w.WriteHeader(http.StatusOK)
					case "/api/users/0/items/top":
						if _, err := os.Stat(pendingPath); err != nil {
							t.Errorf("guard absent during key resolution: %v", err)
						}
						if failure == "unresolved" {
							_, _ = io.WriteString(w, `[]`)
							return
						}
						_ = json.NewEncoder(w).Encode([]map[string]any{{"key": "ABCD2345", "data": map[string]any{
							"key": "ABCD2345", "title": "Completed Paper", "itemType": "journalArticle", "dateAdded": time.Now().UTC().Format(time.RFC3339),
						}}})
					case "/api/users/0/items/ABCD2345/children":
						_, _ = io.WriteString(w, `[{"key":"BCDE2345","data":{"key":"BCDE2345","itemType":"attachment","contentType":"application/pdf"}}]`)
					default:
						http.NotFound(w, r)
					}
				}))
				defer srv.Close()
				t.Setenv("ZOTERO_BASE_URL", srv.URL+"/api/users/0")
				flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1, connectorTarget: "C1", configPath: testConfigFile(t, srv.URL+"/api/users/0")}
				conn := connector.New(srv.URL+"/connector", time.Second)
				cmd := testApplyCmd()
				cmd.SetContext(ctx)
				cmd.SetOut(&bytes.Buffer{})
				cmd.SetErr(io.Discard)
				if failure == "output" {
					cmd.SetOut(importfileBrokenOutput{})
				}
				mutationJournalRecorder = func(env *mutation.Envelope) error {
					if _, err := os.Stat(pendingPath); err != nil {
						t.Errorf("guard absent before journal publication: %v", err)
					}
					if failure == "journal" {
						return errors.New("journal disk full")
					}
					return recordMutationJournal(env)
				}
				op := importPDFOp(cmd, flags, conn, path, filepath.Base(path), 1, "CDEF2345")
				env, runErr := runMutation(ctx, flags, operation, []mutation.Op{op})
				if saves.Load() != 1 || recognition.Load() != 1 {
					t.Fatalf("saves=%d recognition=%d", saves.Load(), recognition.Load())
				}
				if _, err := os.Stat(pendingPath); err != nil {
					t.Fatalf("guard disappeared before rendering: %v", err)
				}
				if failure == "cancel_after_journal" {
					cancel()
				}
				renderErr := renderImportPDFMutationResult(cmd, flags, env, runErr)
				if failure == "none" {
					if renderErr != nil || env.Result == nil || env.Result.Summary.Applied != 1 {
						t.Fatalf("successful import: result=%+v err=%v", env.Result, renderErr)
					}
					if _, err := os.Stat(pendingPath); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("completed import retained guard: %v", err)
					}
					return
				}
				if _, err := os.Stat(pendingPath); err != nil {
					t.Fatalf("%s lost guard: %v", failure, err)
				}
				if failure == "output" && renderErr == nil {
					t.Fatal("output failure did not propagate")
				}
				// The same shared operation protects standalone imports and
				// manifest recognize entries before another save can start.
				retry := importPDFOp(testApplyCmd(), flags, conn, path, filepath.Base(path), 1, "")
				status, _, retryErr := retry.Apply()
				if retryErr == nil || status != "failed" || saves.Load() != 1 {
					t.Fatalf("replay status=%s err=%v saves=%d", status, retryErr, saves.Load())
				}
			})
		}
	}
}
