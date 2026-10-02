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
	"zotio/internal/store"
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

func TestRssSavedScopeNativeGroupUsesActiveLibrary(t *testing.T) {
	oldGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(oldGroup) })
	t.Setenv("ZOTERO_GROUP", "99")
	if err := ApplyGroupScopeFromEnv(); err != nil {
		t.Fatalf("apply group scope: %v", err)
	}
	paths := make(chan string, 1)
	routeLocalAPIToHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/groups/99/searches/S1/items":
			paths <- r.URL.Path
			_, _ = w.Write([]byte(`[{"key":"IN1","data":{}}]`))
		case "/api/users/0/searches/S1/items":
			paths <- r.URL.Path
			_, _ = w.Write([]byte(`[{"key":"OUT","data":{}}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	db, err := store.OpenReadOnlyContext(t.Context(), fsSeedScopedFulltextStore(t))
	if err != nil {
		t.Fatalf("open mirror: %v", err)
	}
	defer db.Close()
	keys, err := ResolveFulltextSearchScope(t.Context(), db, "saved-search:S1")
	if err != nil {
		t.Fatalf("resolve native scope: %v", err)
	}
	if len(keys) != 1 || keys[0] != "IN1" {
		t.Fatalf("native group cohort = %v, want [IN1]", keys)
	}
	select {
	case path := <-paths:
		if path != "/api/groups/99/searches/S1/items" {
			t.Fatalf("saved-search path = %q, want the active group library", path)
		}
	default:
		t.Fatal("saved-search scope never reads live membership")
	}
}

func TestRssSavedScopeWithoutMirrorForbiddenRefuses(t *testing.T) {
	routeLocalAPIToHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/users/0/searches/SS1/items" {
			http.Error(w, "Local API is not enabled", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	dbPath := filepath.Join(t.TempDir(), "missing.db")
	out, err := fsRunSearch(t, &rootFlags{asJSON: true, dataSource: "local", timeout: time.Second},
		"calibration", "--fulltext", "--scope", "saved-search:SS1", "--db", dbPath)
	if ExitCode(err) != 9 {
		t.Fatalf("forbidden saved-search exit = %d (%v), want 9; output=%s", ExitCode(err), err, out)
	}
	var envelope preconditionUnmetEnvelope
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("decode refusal %q: %v", out, err)
	}
	if envelope.Kind != "precondition_unmet" || envelope.Precondition != preconditionLiveLocalAPI {
		t.Fatalf("refusal = %+v, want live_local_api", envelope)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("forbidden search creates a mirror: stat error = %v", err)
	}
}
