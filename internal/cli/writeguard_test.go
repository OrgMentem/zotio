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
	"strconv"
	"strings"
	"sync/atomic"
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

// writeGuardDropOnceServer answers reads with serveGet and drops the response
// to the first wantMethod request without answering it. That is the shape of a
// write the server committed whose response never reached the client: the
// transport fails, so the caller cannot tell the write apart from one that
// never landed, and must read the object back to find out.
func writeGuardDropOnceServer(t *testing.T, wantMethod string, serveGet func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	var writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			serveGet(w)
		case wantMethod:
			if n := writes.Add(1); n > 1 {
				t.Errorf("%s dispatched %d times; an ambiguous write must never be replayed", wantMethod, n)
				http.Error(w, "unexpected replay", http.StatusPreconditionFailed)
				return
			}
			if got := r.Header.Get("If-Unmodified-Since-Version"); got != "7" {
				t.Errorf("%s precondition = %q, want 7", wantMethod, got)
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
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeGuardObject writes one Zotero object with its version in the header the
// client reads the observed version from.
func writeGuardObject(w http.ResponseWriter, key string, version int, data map[string]any) {
	w.Header().Set("Last-Modified-Version", strconv.Itoa(version))
	_ = json.NewEncoder(w).Encode(map[string]any{"key": key, "version": version, "data": data})
}

// putWithVersionGuard reconciles a lost PUT response by comparing the current
// object against the fields it asked for. collections update and collections
// move route every rename and reparent through it, so an inverted comparison
// would report someone else's collection tree as the one the caller asked for.
func TestGuardedPutReconcilesLostCommittedResponse(t *testing.T) {
	const path = "/collections/COLL0001"
	desired := map[string]any{"name": "Requested", "parentCollection": "PARENT01"}

	for _, tc := range []struct {
		name       string
		serveGet   func(w http.ResponseWriter)
		wantStatus string
		wantCode   int
		wantData   bool
		wantErr    bool
	}{
		{
			name: "requested write landed",
			serveGet: func(w http.ResponseWriter) {
				writeGuardObject(w, "COLL0001", 8, map[string]any{"name": "Requested", "parentCollection": "PARENT01"})
			},
			wantStatus: "applied",
			wantCode:   http.StatusOK,
			wantData:   true,
		},
		{
			name: "another write won",
			serveGet: func(w http.ResponseWriter) {
				writeGuardObject(w, "COLL0001", 8, map[string]any{"name": "Another actor", "parentCollection": "PARENT01"})
			},
			wantStatus: "conflict",
			wantErr:    true,
		},
		{
			name:       "read-back fails",
			serveGet:   func(w http.ResponseWriter) { http.Error(w, "unavailable", http.StatusServiceUnavailable) },
			wantStatus: "failed",
			wantErr:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastRetryBackoff(t)
			srv := writeGuardDropOnceServer(t, http.MethodPut, tc.serveGet)
			c := client.New(&config.Config{BaseURL: srv.URL}, time.Second, 0)
			c.NoCache = true

			data, statusCode, status, detail, err := putWithVersionGuard(c, path, desired, 7)
			if status != tc.wantStatus {
				t.Fatalf("status=%q detail=%v err=%v, want %q", status, detail, err, tc.wantStatus)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error: %v", err, tc.wantErr)
			}
			if statusCode != tc.wantCode {
				t.Errorf("statusCode = %d, want %d", statusCode, tc.wantCode)
			}
			if (data != nil) != tc.wantData {
				t.Errorf("data = %s, want present: %v", data, tc.wantData)
			}
			if tc.wantStatus == "applied" {
				evidence, ok := detail.(map[string]any)
				if !ok || evidence["reconciled"] != true || evidence["observed_version"] != 8 {
					t.Fatalf("detail = %#v, want reconciled evidence naming the observed version", detail)
				}
			}
			if tc.wantStatus == "failed" && !strings.Contains(err.Error(), "reconciliation failed") {
				t.Errorf("error = %q, want it to name the failed reconciliation", err)
			}
		})
	}
}

// deleteWithVersionGuard reconciles a lost DELETE response by the target's
// ABSENCE, the opposite evidence to the PUT guard's field comparison. Only a
// 404 read-back proves the delete landed: a surviving object means the delete
// is still unproven, and reporting it as applied would tell items delete
// --permanent and collections delete that a purge succeeded when it did not.
func TestGuardedDeleteReconcilesLostCommittedResponse(t *testing.T) {
	const path = "/items/ITEM0001"

	for _, tc := range []struct {
		name        string
		serveGet    func(w http.ResponseWriter)
		wantStatus  string
		wantCode    int
		wantDeleted bool
		wantErr     bool
	}{
		{
			name:        "target is gone",
			serveGet:    func(w http.ResponseWriter) { http.Error(w, "Not found", http.StatusNotFound) },
			wantStatus:  "applied",
			wantCode:    http.StatusNoContent,
			wantDeleted: true,
		},
		{
			name: "target survived",
			serveGet: func(w http.ResponseWriter) {
				writeGuardObject(w, "ITEM0001", 8, map[string]any{"title": "Still here"})
			},
			wantStatus: "failed",
			wantErr:    true,
		},
		{
			name:       "read-back fails",
			serveGet:   func(w http.ResponseWriter) { http.Error(w, "unavailable", http.StatusServiceUnavailable) },
			wantStatus: "failed",
			wantErr:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fastRetryBackoff(t)
			srv := writeGuardDropOnceServer(t, http.MethodDelete, tc.serveGet)
			c := client.New(&config.Config{BaseURL: srv.URL}, time.Second, 0)
			c.NoCache = true

			_, statusCode, status, detail, err := deleteWithVersionGuard(c, path, 7)
			if status != tc.wantStatus {
				t.Fatalf("status=%q detail=%v err=%v, want %q", status, detail, err, tc.wantStatus)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error: %v", err, tc.wantErr)
			}
			if statusCode != tc.wantCode {
				t.Errorf("statusCode = %d, want %d", statusCode, tc.wantCode)
			}
			if tc.wantDeleted {
				evidence, ok := detail.(map[string]any)
				if !ok || evidence["reconciled"] != true || evidence["deleted"] != true {
					t.Fatalf("detail = %#v, want reconciled deletion evidence", detail)
				}
			}
		})
	}
}
