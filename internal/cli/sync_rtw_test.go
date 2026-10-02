// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRtwRepairFailuresKeepChangeOnlyWorkflowIncomplete(t *testing.T) {
	for _, failure := range []string{"member repair", "key listing"} {
		for _, mode := range []string{"watch", "summary", "plain sync"} {
			t.Run(failure+"/"+mode, func(t *testing.T) {
				syncTestWithHumanFriendly(t, false)
				fastRetryBackoff(t)
				lib := &woLibrary{
					version: 10,
					items: map[string]woObject{
						"ITEM0001": rtwMember("ITEM0001"),
						"ITEM0002": rtwMember("ITEM0002"),
					},
					collections: map[string]woObject{"COLL0002": woCollection("COLL0002", 9, "Erased")},
				}
				var failing atomic.Bool
				var failedRequests atomic.Int64
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if failing.Load() {
						if failure == "key listing" && r.URL.Path == "/users/0/collections" && r.URL.Query().Get("format") == "keys" {
							failedRequests.Add(1)
							http.Error(w, "listing failure", http.StatusInternalServerError)
							return
						}
						if r.URL.Path == "/users/0/items/ITEM0001" {
							_, _ = fmt.Fprint(w, `{"key":"ITEM0001","version":5,"data":{"key":"ITEM0001","version":5,"itemType":"book","collections":[]}}`)
							return
						}
						if r.URL.Path == "/users/0/items" && r.URL.Query().Get("itemKey") != "" {
							failedRequests.Add(1)
							http.Error(w, "repair failure", http.StatusInternalServerError)
							return
						}
					}
					lib.ServeHTTP(w, r)
				}))
				defer srv.Close()
				t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
				t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
				t.Setenv("ZOTERO_DATA_DIR", t.TempDir())
				t.Setenv("ZOTERO_HOME", "")
				t.Setenv("ZOTIO_DEMO", "")
				if stderr, err := woRunWatch(t); err != nil {
					t.Fatalf("seed mirror: %v; %s", err, stderr)
				}
				lib.mu.Lock()
				lib.version = 11
				delete(lib.collections, "COLL0002")
				lib.items["ITEM0003"] = woItem("ITEM0003", 11, "New", false)
				lib.mu.Unlock()
				failing.Store(true)
				if mode == "watch" {
					spec := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{{Args: []string{"version"}}}})
					stderr, err := woRunWatch(t, "--workflow", spec, "--workflow-on-change")
					if err != nil {
						t.Fatalf("watch exit = %d: %v", ExitCode(err), err)
					}
					if !strings.Contains(stderr, "workflow skipped: sync incomplete") || strings.Contains(stderr, "workflow preview") {
						t.Fatalf("workflow must skip incomplete mirror: %s", stderr)
					}
				} else {
					var summary *syncChangeSummary
					if mode == "summary" {
						summary = &syncChangeSummary{}
					}
					cmd := newSyncCmdWithSummary(&rootFlags{timeout: 5 * time.Second}, summary)
					cmd.SetOut(&bytes.Buffer{})
					cmd.SetErr(&bytes.Buffer{})
					cmd.SetArgs([]string{"--resources", "items,collections", "--concurrency", "1"})
					if err := cmd.Execute(); err != nil {
						t.Fatalf("sync exit = %d, want 0: %v", ExitCode(err), err)
					}
					if summary != nil && (summary.Complete || !summary.changed()) {
						t.Fatalf("summary = %+v, want changed but incomplete", summary)
					}
				}
				if failedRequests.Load() == 0 {
					t.Fatal("scenario did not reach the failed reconciliation request")
				}
			})
		}
	}
}

func rtwMember(key string) woObject {
	return woObject{version: 5, body: fmt.Sprintf(`{"key":%q,"version":5,"data":{"key":%q,"version":5,"itemType":"book","collections":["COLL0002"]}}`, key, key)}
}
