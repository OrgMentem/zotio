// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// The Zotero local API rejects writes with distinctive bodies; classifyAPIError
// must turn those into read-only guidance, while leaving genuine auth errors alone.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
)

func TestClassifyAPIErrorLocalWriteRejection(t *testing.T) {
	for _, msg := range []string{
		"POST /items returned HTTP 400: Endpoint does not support method",
		"PATCH /items/ABCD returned HTTP 501: Method not implemented",
	} {
		got := classifyAPIError(fmt.Errorf("%s", msg), &rootFlags{}).Error()
		if !strings.Contains(got, "read-only") || !strings.Contains(got, "ZOTERO_BASE_URL") {
			t.Errorf("%q -> expected read-only guidance, got: %s", msg, got)
		}
	}
}

func TestClassifyAPIErrorAuthNotMisclassified(t *testing.T) {
	// A genuine auth 400 (no local-API rejection strings) must not be relabeled.
	got := classifyAPIError(fmt.Errorf("POST /items returned HTTP 400: invalid key"), &rootFlags{}).Error()
	if strings.Contains(got, "read-only") {
		t.Errorf("auth 400 misclassified as a local read-only rejection: %s", got)
	}
}

func TestClassifyAPIErrorVersionConflict(t *testing.T) {
	got := classifyAPIError(fmt.Errorf("PATCH /items/A returned HTTP 412: Precondition Failed"), &rootFlags{}).Error()
	if !strings.Contains(got, "version conflict") || !strings.Contains(got, "sync") {
		t.Errorf("412 -> expected version-conflict/sync hint, got: %s", got)
	}
}
func TestGuardedPatchReconcilesLostCommittedResponse(t *testing.T) {
	for _, tc := range []struct {
		name       string
		landed     bool
		wantStatus string
	}{
		{name: "requested mutation landed", landed: true, wantStatus: "applied"},
		{name: "another mutation won", landed: false, wantStatus: "conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const path = "/items/ITEM0001"
			desired := map[string]any{
				"title":       "Requested",
				"collections": []string{"COLL0002", "COLL0001"},
				"tags": []map[string]any{
					{"tag": "beta", "type": 0},
					{"tag": "alpha"},
				},
			}
			version := 1
			current := map[string]any{
				"title": "Before", "collections": []string{"COLL0000"},
				"tags": []map[string]any{{"tag": "old"}},
			}
			var gets, patches int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					gets++
					_ = json.NewEncoder(w).Encode(map[string]any{
						"key": "ITEM0001", "version": version, "data": current,
					})
				case http.MethodPatch:
					patches++
					if got := r.Header.Get("If-Unmodified-Since-Version"); got != "1" {
						t.Errorf("patch precondition = %q, want 1", got)
					}
					if patches == 1 {
						version = 2
						if tc.landed {
							current = map[string]any{
								"title":       "Requested",
								"collections": []string{"COLL0001", "COLL0002"},
								"tags": []map[string]any{
									{"tag": "alpha", "type": 0},
									{"tag": "beta", "type": 0},
								},
							}
						} else {
							current = map[string]any{
								"title":       "Another actor",
								"collections": []string{"COLL0001"},
								"tags":        []map[string]any{{"tag": "other"}},
							}
						}
						hijacker, ok := w.(http.Hijacker)
						if !ok {
							t.Error("test server cannot drop the committed response")
							return
						}
						conn, _, err := hijacker.Hijack()
						if err != nil {
							t.Errorf("hijack response: %v", err)
							return
						}
						_ = conn.Close()
						return
					}
					http.Error(w, "stale version", http.StatusPreconditionFailed)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			c := client.New(&config.Config{BaseURL: srv.URL}, time.Second, 0)
			c.NoCache = true

			status, detail, err := patchWithWritePlaneVersion(context.Background(), c, path, desired)
			if status != tc.wantStatus {
				t.Fatalf("status=%q detail=%v err=%v, want %q", status, detail, err, tc.wantStatus)
			}
			if tc.landed {
				if err != nil {
					t.Fatalf("reconciled applied mutation returned error: %v", err)
				}
				evidence, ok := detail.(map[string]any)
				if !ok || evidence["reconciled"] != true {
					t.Fatalf("detail = %#v, want reconciled evidence", detail)
				}
			} else if err == nil {
				t.Fatal("non-matching current object returned no conflict error")
			}
			if gets != 2 || patches != 1 {
				t.Fatalf("gets=%d patches=%d, want precondition read, lost PATCH, and reconciliation read", gets, patches)
			}
		})
	}
}
