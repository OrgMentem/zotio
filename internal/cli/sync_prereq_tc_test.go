// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Dependent per-item-type schema sync must refuse to fan out when its cached
// /itemTypes prerequisite is empty, oversized, unprobeable, or drifted from
// the live list.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"zotio/internal/store"
)

func TestSyncDependentSchemaRefusesUnusableItemTypePrerequisite(t *testing.T) {
	oversized := make([]string, maxDependentSchemaItemTypes+1)
	for i := range oversized {
		oversized[i] = fmt.Sprintf("type-%03d", i)
	}

	for _, tt := range []struct {
		name string
		// cached item types seeded into the schema resource.
		cached []string
		// liveStatus is the /itemTypes response status; liveTypes its body.
		liveStatus int
		liveTypes  []string
		// wantPrecondition: exit 9 precondition_unmet whose detail has wantDetail.
		wantPrecondition bool
		wantDetail       string
		// wantItemTypeProbe: whether the live /itemTypes probe may run at all.
		wantItemTypeProbe bool
	}{
		{
			name:             "empty cached item-type list",
			liveStatus:       http.StatusOK,
			liveTypes:        []string{"book"},
			wantPrecondition: true,
			wantDetail:       "the cached item-type list is empty",
		},
		{
			name:              "cached list drifted from live list",
			cached:            []string{"book"},
			liveStatus:        http.StatusOK,
			liveTypes:         []string{"book", "thesis"},
			wantPrecondition:  true,
			wantDetail:        "(cached 1, live 2)",
			wantItemTypeProbe: true,
		},
		{
			name:       "cardinality above the safety ceiling",
			cached:     oversized,
			liveStatus: http.StatusOK,
			liveTypes:  oversized,
			wantDetail: fmt.Sprintf("count %d exceeds the safety ceiling of %d", maxDependentSchemaItemTypes+1, maxDependentSchemaItemTypes),
		},
		{
			name:              "live item-type probe fails",
			cached:            []string{"book"},
			liveStatus:        http.StatusInternalServerError,
			wantItemTypeProbe: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			syncTestWithHumanFriendly(t, false)
			fastRetryBackoff(t)

			var mu sync.Mutex
			probeHits, deepHits := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/itemTypes":
					mu.Lock()
					probeHits++
					mu.Unlock()
					if tt.liveStatus != http.StatusOK {
						http.Error(w, "schema unavailable", tt.liveStatus)
						return
					}
					w.Header().Set("Zotero-Schema-Version", "100")
					rows := make([]map[string]string, 0, len(tt.liveTypes))
					for _, itemType := range tt.liveTypes {
						rows = append(rows, map[string]string{"itemType": itemType})
					}
					_ = json.NewEncoder(w).Encode(rows)
				case "/itemTypeFields":
					mu.Lock()
					deepHits++
					mu.Unlock()
					fmt.Fprint(w, `[{"field":"title"}]`)
				default:
					http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
				}
			}))
			defer server.Close()

			dbPath := filepath.Join(t.TempDir(), "sync.db")
			db, err := store.OpenWithContext(context.Background(), dbPath)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			if len(tt.cached) > 0 {
				rows := make([]json.RawMessage, len(tt.cached))
				for i, itemType := range tt.cached {
					rows[i] = json.RawMessage(fmt.Sprintf(`{"itemType":%q}`, itemType))
				}
				if _, err := db.UpsertKeyed("schema", tt.cached, rows); err != nil {
					db.Close()
					t.Fatalf("seed item types: %v", err)
				}
			}
			if err := db.SaveZoteroSchemaVersionContext(context.Background(), "schema", "100"); err != nil {
				db.Close()
				t.Fatalf("seed schema version: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close seed store: %v", err)
			}

			cmd := newSyncCmd(&rootFlags{
				asJSON:     true,
				configPath: testConfigFile(t, server.URL+"/users/0"),
				timeout:    5 * time.Second,
			})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetArgs([]string{"--resources", "schema-item-type-fields", "--db", dbPath})
			err = cmd.Execute()
			if err == nil {
				t.Fatalf("dependent schema sync succeeded on an unusable prerequisite; output=%s", out.String())
			}

			if tt.wantPrecondition {
				if ExitCode(err) != 9 {
					t.Fatalf("error = %v (exit %d), want exit 9; output=%s", err, ExitCode(err), out.String())
				}
				var env preconditionUnmetEnvelope
				if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
					t.Fatalf("decode precondition envelope %q: %v", out.String(), decodeErr)
				}
				if env.Kind != "precondition_unmet" || env.Precondition != preconditionSyncedStore {
					t.Fatalf("envelope = %+v, want precondition_unmet/%s", env, preconditionSyncedStore)
				}
				if !strings.Contains(env.Detail, tt.wantDetail) {
					t.Fatalf("precondition detail = %q, want %q", env.Detail, tt.wantDetail)
				}
			} else {
				if ExitCode(err) == 9 {
					t.Fatalf("error = %v exited 9, want a sync failure rather than a stale-store precondition", err)
				}
				if tt.wantDetail != "" && !strings.Contains(out.String()+err.Error(), tt.wantDetail) {
					t.Fatalf("output=%s error=%v, want %q", out.String(), err, tt.wantDetail)
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if deepHits != 0 {
				t.Fatalf("refused prerequisite made %d per-item-type request(s), want 0", deepHits)
			}
			if !tt.wantItemTypeProbe && probeHits != 0 {
				t.Fatalf("refusal decided from the cache still probed /itemTypes %d time(s)", probeHits)
			}
		})
	}
}
