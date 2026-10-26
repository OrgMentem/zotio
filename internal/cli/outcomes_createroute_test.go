// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"zotio/internal/connector"
	"zotio/internal/mutation"
)

func setupCreateRouteJournal(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	previous := mutationJournalRecorder
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = previous })
}

func assertCreateRouteUnknown(t *testing.T, env mutation.Envelope) {
	t.Helper()
	if env.Result == nil || len(env.Result.Items) != 1 || env.Result.Items[0].Status != "conflict" {
		t.Fatalf("expected one conflict: %+v", env)
	}
	reason, _ := env.Result.Items[0].Reason.(map[string]any)
	if journal, _ := env.Journal.(map[string]any); reason["committed"] != true || reason["outcome"] != "unknown" || journal["run_id"] == nil {
		t.Fatalf("missing committed unknown journal evidence: reason=%v journal=%v", reason, env.Journal)
	}
}

func TestSingleCreateWriteTokenStableOnRetry(t *testing.T) {
	for _, generic := range []bool{false, true} {
		t.Run(fmt.Sprintf("generic=%v", generic), func(t *testing.T) {
			setupCreateRouteJournal(t)
			fastRetryBackoff(t)
			var mu sync.Mutex
			var tokens []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				tokens = append(tokens, r.Header.Get("Zotero-Write-Token"))
				first := len(tokens) == 1
				mu.Unlock()
				if first {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				http.Error(w, "token already used", http.StatusPreconditionFailed)
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			flags := &rootFlags{yes: true, asJSON: true, maxChanges: -1}
			var env mutation.Envelope
			var err error
			if generic {
				file := writeImportGenericFixture(t, "{\"itemType\":\"book\",\"title\":\"Example\"}\n")
				_, raw, _, runErr := runImportGeneric(t, flags, "items", "--input", file)
				err = runErr
				if decodeErr := json.Unmarshal([]byte(raw), &env); decodeErr != nil {
					t.Fatal(decodeErr)
				}
			} else {
				c, clientErr := flags.newClient()
				if clientErr != nil {
					t.Fatal(clientErr)
				}
				env, err = runMutation(context.Background(), flags, "items.new", []mutation.Op{{
					ID: "create", Kind: "item_create", Changes: []mutation.Change{{Field: "title", Add: "Example"}},
					Apply: func() (string, any, error) {
						res, createErr := routeCreateItemVia(context.Background(), flags, "web", c, map[string]any{"title": "Example", "itemType": "book"}, "", false)
						return singleItemCreateApplyResult(res, createErr, "")
					},
				}})
			}
			if err == nil {
				t.Fatal("expected unresolved create error")
			}
			assertCreateRouteUnknown(t, env)
			mu.Lock()
			defer mu.Unlock()
			if len(tokens) < 2 || tokens[0] == "" {
				t.Fatalf("missing write token retry: %v", tokens)
			}
			for _, token := range tokens {
				if token != tokens[0] {
					t.Fatalf("write token changed on retry: %v", tokens)
				}
			}
		})
	}
}

func TestConnectorMissingRecoveryIsJournaledUnknown(t *testing.T) {
	for _, tc := range []struct {
		name      string
		title     string
		truncated bool
	}{
		{name: "absent", title: "Not visible"},
		{name: "untitled"},
		{name: "committed-truncated", title: "Not visible", truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupCreateRouteJournal(t)
			oldWindow := connectorCreateRecoveryWindow
			connectorCreateRecoveryWindow = 0
			t.Cleanup(func() { connectorCreateRecoveryWindow = oldWindow })
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/connector/saveItems" {
					if tc.truncated {
						w.Header().Set("Content-Length", "100")
						w.WriteHeader(http.StatusCreated)
						_, _ = w.Write([]byte("{"))
					} else {
						http.Error(w, "save failed", http.StatusInternalServerError)
					}
					return
				}
				_, _ = w.Write([]byte("[]"))
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			oldFactory := connectorForCreate
			connectorForCreate = func(*rootFlags) (*connector.Client, error) {
				return connector.New(srv.URL+"/connector", time.Second), nil
			}
			t.Cleanup(func() { connectorForCreate = oldFactory })
			flags := &rootFlags{yes: true, maxChanges: -1, via: "connector"}
			env, err := runMutation(context.Background(), flags, "items.new", []mutation.Op{{
				ID: "create", Kind: "item_create", Changes: []mutation.Change{{Field: "itemType", Add: "book"}},
				Apply: func() (string, any, error) {
					res, createErr := routeCreateItemVia(context.Background(), flags, "connector", nil, map[string]any{"title": tc.title, "itemType": "book"}, "", false)
					return singleItemCreateApplyResult(res, createErr, "")
				},
			}})
			if err == nil {
				t.Fatal("expected unknown outcome")
			}
			assertCreateRouteUnknown(t, env)
			reason := env.Result.Items[0].Reason.(map[string]any)
			if reason["session"] == "" || reason["connector_key"] == "" || !strings.Contains(reason["message"].(string), "before retrying") {
				t.Fatalf("missing connector evidence or guidance: %v", reason)
			}
		})
	}
}

func TestGenericImportStandardEnvelopeAndResourceValidation(t *testing.T) {
	setupCreateRouteJournal(t)
	_, requests := recordingImportServer(t)
	file := writeImportGenericFixture(t, "{\"title\":\"Example\"}\n")
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
	_, raw, _, err := runImportGeneric(t, flags, "items", "--input", file)
	if err != nil {
		t.Fatal(err)
	}
	var env mutation.Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatal(err)
	}
	if journal, _ := env.Journal.(map[string]any); !env.OK || env.Plan.Summary.Planned != 1 || journal["run_id"] == nil {
		t.Fatalf("missing standard apply envelope: %s", raw)
	}
	flags.maxChanges = 0
	_, raw, _, err = runImportGeneric(t, flags, "items", "--input", file)
	if err == nil || !strings.Contains(raw, "\"schema_version\"") || !strings.Contains(raw, "\"plan\"") {
		t.Fatalf("gate lost standard envelope: %s (%v)", raw, err)
	}
	_, _, _, err = runImportGeneric(t, flags, "bogus", "--input", file)
	if err == nil || !strings.Contains(err.Error(), "valid resources: items, collections, searches") || *requests != 1 {
		t.Fatalf("invalid resource reached API: requests=%d err=%v", *requests, err)
	}
}

func TestConnectorCreateInvalidatesLibraryListings(t *testing.T) {
	setupCreateRouteJournal(t)
	var mu sync.Mutex
	created := false
	reads := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/connector/saveItems" {
			created = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("{}"))
			return
		}
		reads[r.URL.Path]++
		if created {
			_, _ = w.Write([]byte(`[{"key":"ABCD2345"},{"key":"BCDE2345"}]`))
		} else {
			_, _ = w.Write([]byte(`[{"key":"ABCD2345"}]`))
		}
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	flags := &rootFlags{via: "connector", maxChanges: -1}
	c, err := flags.newClient()
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"/items", "/items/top", "/collections/ABCD2345/items", "/tags"}
	for _, path := range paths {
		for range 2 {
			if _, err := c.Get(path, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	mu.Lock()
	for _, path := range paths {
		if reads["/users/0"+path] != 1 {
			t.Errorf("cache fixture did not cache %s: %v", path, reads)
		}
	}
	mu.Unlock()
	oldFactory := connectorForCreate
	connectorForCreate = func(*rootFlags) (*connector.Client, error) {
		return connector.New(srv.URL+"/connector", time.Second), nil
	}
	t.Cleanup(func() { connectorForCreate = oldFactory })
	if _, err := routeCreateItemVia(context.Background(), flags, "connector", c, map[string]any{"itemType": "book"}, "", false); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		data, err := c.Get(path, nil)
		if err != nil || !strings.Contains(string(data), "BCDE2345") {
			t.Fatalf("stale listing after connector create: %s = %s (%v)", path, data, err)
		}
	}
}
