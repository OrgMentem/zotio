// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// export snapshot verify must compare against the lockfile's recorded scope,
// not the whole library.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestExportSnapshotVerifyComparesAgainstRecordedScope(t *testing.T) {
	inScopeA := exportVerifyTestItem("SCOPEA", 1, "In scope A")
	inScopeB := exportVerifyTestItem("SCOPEB", 1, "In scope B")
	libraryOnly := exportVerifyTestItem("LIBONLY", 1, "Elsewhere in the library")

	for _, tt := range []struct {
		name          string
		scope         string
		lockItems     []json.RawMessage
		wantDetection string
		wantReason    bool
		wantSummary   exportVerifySummary
		wantClasses   map[string]string
	}{
		{
			name:          "collection scope fetches only the collection",
			scope:         "collection:COLL1",
			lockItems:     []json.RawMessage{inScopeA, inScopeB},
			wantDetection: "resolved",
			wantSummary:   exportVerifySummary{Unchanged: 2},
			wantClasses:   map[string]string{"SCOPEA": "unchanged", "SCOPEB": "unchanged"},
		},
		{
			name:          "tag scope fetches only tagged items",
			scope:         "tag:to-read",
			lockItems:     []json.RawMessage{inScopeA},
			wantDetection: "resolved",
			wantSummary:   exportVerifySummary{Unchanged: 1},
			wantClasses:   map[string]string{"SCOPEA": "unchanged"},
		},
		{
			name:          "unresolvable scope verifies only recorded keys",
			scope:         "item:LEGACY",
			lockItems:     []json.RawMessage{inScopeA, exportVerifyTestItem("GONE1", 1, "Purged")},
			wantDetection: "skipped",
			wantReason:    true,
			wantSummary:   exportVerifySummary{Removed: 1, Unchanged: 1},
			wantClasses:   map[string]string{"SCOPEA": "unchanged", "GONE1": "removed"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/users/0/collections/COLL1/items":
					_, _ = w.Write([]byte("[" + exportVerifyJoinRaw([]json.RawMessage{inScopeA, inScopeB}) + "]"))
				case "/users/0/items":
					if r.URL.Query().Get("tag") == "to-read" {
						_, _ = w.Write([]byte("[" + exportVerifyJoinRaw([]json.RawMessage{inScopeA}) + "]"))
						return
					}
					_, _ = w.Write([]byte("[" + exportVerifyJoinRaw([]json.RawMessage{inScopeA, inScopeB, libraryOnly}) + "]"))
				case "/users/0/items/SCOPEA":
					_, _ = w.Write(inScopeA)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

			lockPath := writeExportVerifyTestLockfile(t, tt.scope, tt.lockItems)
			cmd := newExportSnapshotVerifyCmd(&rootFlags{asJSON: true})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetArgs([]string{lockPath, "--fail-on-drift"})
			var stdout bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&bytes.Buffer{})
			runErr := cmd.Execute()

			var report exportVerifyReport
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &report); err != nil {
				t.Fatalf("decode report %q: %v (run error %v)", stdout.String(), err, runErr)
			}
			if report.AddedDetection != tt.wantDetection || (report.AddedDetectionReason != "") != tt.wantReason {
				t.Fatalf("added detection = %q reason %q, want %q (reason present: %v)", report.AddedDetection, report.AddedDetectionReason, tt.wantDetection, tt.wantReason)
			}
			if report.Summary != tt.wantSummary {
				t.Fatalf("summary = %+v, want %+v (items %+v)", report.Summary, tt.wantSummary, report.Items)
			}
			assertExportVerifyClasses(t, report, tt.wantClasses)
			wantDrift := tt.wantSummary.Added+tt.wantSummary.Removed+tt.wantSummary.Changed > 0
			if (runErr != nil) != wantDrift {
				t.Fatalf("--fail-on-drift error = %v, want drift failure %v", runErr, wantDrift)
			}
		})
	}
}
