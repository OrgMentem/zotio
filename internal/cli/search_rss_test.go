// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zotio/internal/client"
)

func TestRssSearchSavedScopeWithoutMirrorRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	for _, tc := range []struct {
		name      string
		available bool
	}{
		{name: "desktop unavailable"},
		{name: "desktop available", available: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.available {
				stubSavedSearchLocalAPI(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/users/0/searches/SS1/items" {
						http.NotFound(w, r)
						return
					}
					_, _ = w.Write([]byte(`[{"key":"PARENT","data":{}}]`))
				})
			} else {
				old := savedSearchLiveClient
				savedSearchLiveClient = func(context.Context, *rootFlags) (*client.Client, string, error) {
					return nil, "Zotero desktop local API is not reachable", nil
				}
				t.Cleanup(func() { savedSearchLiveClient = old })
			}

			dbPath := filepath.Join(t.TempDir(), "missing.db")
			out, err := fsRunSearch(t, &rootFlags{asJSON: true, dataSource: "local", timeout: time.Second},
				"calibration", "--fulltext", "--scope", "saved-search:SS1", "--db", dbPath)
			if tc.available {
				if err != nil {
					t.Fatalf("search with reachable desktop: %v; output=%s", err, out)
				}
				var got struct {
					Results []json.RawMessage `json:"results"`
				}
				if err := json.Unmarshal([]byte(out), &got); err != nil {
					t.Fatalf("decode empty search %q: %v", out, err)
				}
				if got.Results == nil || len(got.Results) != 0 {
					t.Fatalf("missing mirror results = %s, want an empty results array", out)
				}
			} else {
				if ExitCode(err) != 9 {
					t.Fatalf("search exit = %d (%v), want precondition exit 9; output=%s", ExitCode(err), err, out)
				}
				var envelope preconditionUnmetEnvelope
				if err := json.Unmarshal([]byte(out), &envelope); err != nil {
					t.Fatalf("decode refusal %q: %v", out, err)
				}
				if envelope.Kind != "precondition_unmet" || envelope.Precondition != preconditionLiveLocalAPI {
					t.Fatalf("refusal = %+v, want precondition_unmet for %s", envelope, preconditionLiveLocalAPI)
				}
			}
			if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
				t.Fatalf("search creates a mirror: stat error = %v", err)
			}
		})
	}
}
