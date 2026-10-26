// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/connector"
)

func TestImportApplyConnectorKeepsOnlyUncertainIntentAttachments(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		refused   bool
		uncertain bool
	}{
		{name: "rejected", status: http.StatusBadRequest},
		{name: "refused connection", refused: true},
		{name: "unknown server outcome", status: http.StatusInternalServerError, uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/connector/saveItems" {
					t.Errorf("unexpected request %s", r.URL.Path)
				}
				calls.Add(1)
				http.Error(w, "save rejected", tc.status)
			}))
			t.Cleanup(server.Close)
			if tc.refused {
				server.Close()
			}
			oldConnector, oldPing := connectorForCreate, connectorPing
			connectorForCreate = func(*rootFlags) (*connector.Client, error) {
				return connector.New(server.URL+"/connector", time.Second), nil
			}
			connectorPing = func(context.Context, *connector.Client) error { return nil }
			t.Cleanup(func() { connectorForCreate, connectorPing = oldConnector, oldPing })
			flags := &rootFlags{asJSON: true, yes: true, via: "connector", timeout: time.Second, maxChanges: -1, configPath: testConfigFile(t, "http://127.0.0.1:23119/api/users/0")}
			path := writeUploadFixture(t, "paper.pdf", []byte(uploadFixturePDF))
			manifest := importManifest{SchemaVersion: importManifestSchemaVersion, Entries: []importManifestEntry{{
				Path: path, Action: "create", Status: "resolved", Title: "Paper A", Item: map[string]any{"itemType": "journalArticle", "title": "Paper A"},
			}}}
			journal, err := journalDir()
			if err != nil {
				t.Fatal(err)
			}
			for attempt := range 2 {
				cmd := newImportApplyCmd(flags)
				cmd.SetContext(context.Background())
				ops := importApplyOps(cmd, flags, nil, nil, manifest, "stored", false, "")
				status, detail, err := ops[0].Apply()
				if err == nil {
					t.Fatal("save unexpectedly succeeded")
				}
				wantStatus := "failed"
				if tc.uncertain && attempt == 0 {
					wantStatus = "conflict"
				}
				if status != wantStatus {
					t.Fatalf("status=%s detail=%v error=%v", status, detail, err)
				}
				wantReplayRefusal := tc.uncertain && attempt == 1
				if strings.Contains(err.Error(), "refusing automatic replay") != wantReplayRefusal {
					t.Fatalf("attempt=%d error=%v", attempt, err)
				}
				if !tc.uncertain && detail != nil {
					t.Fatalf("confirmed rejection has committed evidence: %v", detail)
				}
				paths, err := filepath.Glob(filepath.Join(journal+".import-pending", "*.json"))
				if err != nil {
					t.Fatal(err)
				}
				wantRecords := 0
				if tc.uncertain {
					wantRecords = 1
				}
				if len(paths) != wantRecords {
					t.Fatalf("intent records=%v, want %d", paths, wantRecords)
				}
			}
			wantCalls := int32(2)
			if tc.refused {
				wantCalls = 0
			} else if tc.uncertain {
				wantCalls = 1
			}
			if calls.Load() != wantCalls {
				t.Fatalf("save requests=%d, want %d", calls.Load(), wantCalls)
			}
		})
	}
}
