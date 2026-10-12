// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/mutation"
)

type collectionMoveTestServer struct {
	server       *httptest.Server
	getCount     int
	putCount     int
	putHeader    string
	putBody      map[string]any
	requestPaths []string
}

func newCollectionMoveTestServer(t *testing.T) *collectionMoveTestServer {
	t.Helper()
	ts := &collectionMoveTestServer{}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.requestPaths = append(ts.requestPaths, r.Method+" "+r.URL.Path)
		if r.URL.Path != "/users/0/collections/COLL" {
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			ts.getCount++
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Last-Modified-Version", "77")
			_, _ = w.Write([]byte(`{"key":"COLL","version":77,"data":{"key":"COLL","name":"Child","parentCollection":false}}`))
		case http.MethodPut:
			ts.putCount++
			ts.putHeader = r.Header.Get("If-Unmodified-Since-Version")
			if err := json.NewDecoder(r.Body).Decode(&ts.putBody); err != nil {
				t.Errorf("decode put body: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func runCollectionsMoveTestCmd(t *testing.T, srv *collectionMoveTestServer, flags *rootFlags, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	t.Setenv("ZOTERO_API_KEY", "testkey")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newCollectionsMoveCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// TestCollectionsMovePreviewWritesNothing pins one contract across every
// preview mode: emit the shared preview envelope (nothing under --quiet) and
// issue not one HTTP request.
func TestCollectionsMovePreviewWritesNothing(t *testing.T) {
	tests := []struct {
		name      string
		flags     *rootFlags
		wantEmpty bool
	}{
		{name: "default_preview", flags: &rootFlags{}},
		{name: "dry_run", flags: &rootFlags{dryRun: true}},
		{name: "agent_preview", flags: &rootFlags{asJSON: true, agent: true}},
		{name: "quiet_preview", flags: &rootFlags{quiet: true}, wantEmpty: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newCollectionMoveTestServer(t)

			out, stderr, err := runCollectionsMoveTestCmd(t, srv, tt.flags, "--to", "PARENT", "COLL")
			if err != nil {
				t.Fatalf("collections move preview: %v; stderr=%s", err, stderr)
			}
			if srv.getCount != 0 || srv.putCount != 0 || len(srv.requestPaths) != 0 {
				t.Fatalf("requests = %v (GET=%d PUT=%d), want no HTTP calls", srv.requestPaths, srv.getCount, srv.putCount)
			}
			if tt.wantEmpty {
				if out != "" {
					t.Fatalf("stdout = %q, want empty", out)
				}
				return
			}
			if !strings.Contains(out, "COLL") || !strings.Contains(out, "PARENT") {
				t.Fatalf("preview %q does not show the planned move", out)
			}
			if tt.flags.asJSON {
				var env struct {
					Action  string         `json:"action"`
					Key     string         `json:"key"`
					Body    map[string]any `json:"body"`
					DryRun  bool           `json:"dry_run"`
					Success bool           `json:"success"`
				}
				if err := json.Unmarshal([]byte(out), &env); err != nil {
					t.Fatalf("preview is not valid JSON: %v\n%s", err, out)
				}
				if env.Action != "move" || env.Key != "COLL" || !env.DryRun || env.Success || env.Body["parentCollection"] != "PARENT" {
					t.Fatalf("envelope = %+v, want planned move of COLL under PARENT", env)
				}
			}
		})
	}
}

func TestCollectionsMoveApplyGetsThenPutsWithVersionPrecondition(t *testing.T) {
	srv := newCollectionMoveTestServer(t)

	_, stderr, err := runCollectionsMoveTestCmd(t, srv, &rootFlags{yes: true, maxChanges: -1}, "--to", "PARENT", "COLL")
	if err != nil {
		t.Fatalf("collections move apply: %v; stderr=%s", err, stderr)
	}
	if srv.getCount != 1 || srv.putCount != 1 {
		t.Fatalf("GET=%d PUT=%d requests=%v, want one GET then one PUT", srv.getCount, srv.putCount, srv.requestPaths)
	}
	if got, want := strings.Join(srv.requestPaths, ","), "GET /users/0/collections/COLL,PUT /users/0/collections/COLL"; got != want {
		t.Fatalf("request order = %q, want %q", got, want)
	}
	if srv.putHeader != "77" {
		t.Fatalf("If-Unmodified-Since-Version = %q, want 77", srv.putHeader)
	}
	if srv.putBody["parentCollection"] != "PARENT" {
		t.Fatalf("PUT body = %+v, want parentCollection PARENT", srv.putBody)
	}
}

// TestCollectionsMoveIsJournaled guards the fix for the reviewer-flagged gap:
// the PUT used to bypass runMutation entirely, so an applied move never
// reached the journal. Route it through runMutation and the run must appear
// as a journaled "collections.move" entry with one applied change.
func TestCollectionsMoveIsJournaled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	srv := newCollectionMoveTestServer(t)

	_, stderr, err := runCollectionsMoveTestCmd(t, srv, &rootFlags{yes: true, maxChanges: -1}, "--to", "PARENT", "COLL")
	if err != nil {
		t.Fatalf("collections move apply: %v; stderr=%s", err, stderr)
	}

	entries, err := mutation.ListEntries(helpersTestJournalDir(t))
	if err != nil {
		t.Fatalf("list journal entries: %v", err)
	}
	if len(entries) != 1 || entries[0].Operation != "collections.move" || entries[0].Summary.Applied != 1 {
		t.Fatalf("recorded entries = %+v, want one applied collections.move run", entries)
	}
}

// TestCollectionsMoveHonorsMaxChanges guards the same gap from the gate side:
// the direct PutWithHeaders call never consulted CheckGates, so --max-changes
// had no effect on this command. Routing through runMutation must make a cap
// of zero refuse the write before any PUT is issued.
func TestCollectionsMoveHonorsMaxChanges(t *testing.T) {
	srv := newCollectionMoveTestServer(t)

	_, stderr, err := runCollectionsMoveTestCmd(t, srv, &rootFlags{yes: true, maxChanges: 0}, "--to", "PARENT", "COLL")
	if err == nil {
		t.Fatalf("collections move at cap 0 = nil error, want max-changes refusal; stderr=%s", stderr)
	}
	if srv.putCount != 0 {
		t.Fatalf("putCount = %d, want 0 (max-changes must block before the PUT)", srv.putCount)
	}
}

// TestCollectionsMoveSendsVersionHeader confirms the version-read precondition
// survives the runMutation rewrite: the read must stay outside the gated
// Apply closure, still land the If-Unmodified-Since-Version header on the
// PUT, and a failing version read must abort before any PUT is attempted.
func TestCollectionsMoveSendsVersionHeader(t *testing.T) {
	srv := newCollectionMoveTestServer(t)

	_, stderr, err := runCollectionsMoveTestCmd(t, srv, &rootFlags{yes: true, maxChanges: -1}, "--to", "PARENT", "COLL")
	if err != nil {
		t.Fatalf("collections move apply: %v; stderr=%s", err, stderr)
	}
	if srv.putHeader != "77" {
		t.Fatalf("If-Unmodified-Since-Version = %q, want 77", srv.putHeader)
	}

	var putAttempted bool
	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			putAttempted = true
		}
		http.Error(w, "version lookup failed", http.StatusBadRequest)
	}))
	t.Cleanup(failServer.Close)
	t.Setenv("ZOTERO_BASE_URL", failServer.URL+"/users/0")
	t.Setenv("ZOTERO_API_KEY", "testkey")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	cmd := newCollectionsMoveCmd(&rootFlags{yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--to", "PARENT", "COLL"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("collections move with failing version GET = nil error, want failure")
	}
	if putAttempted {
		t.Fatal("PUT was attempted despite a failed version GET")
	}
}
