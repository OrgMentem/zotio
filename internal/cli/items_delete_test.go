// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Zotero requires If-Unmodified-Since-Version on DELETE; items/collections
// delete must fetch the current version and send it (else HTTP 428).

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

func runDeleteCmd(t *testing.T, cmd interface {
	SetOut(io.Writer)
	SetErr(io.Writer)
	SetArgs([]string)
	Execute() error
}, baseURL string, args ...string) error {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", baseURL+"/users/0")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	return cmd.Execute()
}

// deleteWrite is one non-GET request the delete server received.
type deleteWrite struct {
	method, path, version string
}

// deleteVersionServer answers version reads with the given version and records
// every write request, so tests can assert the exact write sequence.
func deleteVersionServer(t *testing.T, version string) (*httptest.Server, *[]deleteWrite) {
	t.Helper()
	writes := new([]deleteWrite)
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Last-Modified-Version", version)
			_, _ = w.Write([]byte(`{"key":"K","version":` + version + `,"data":{}}`))
			return
		}
		mu.Lock()
		*writes = append(*writes, deleteWrite{r.Method, r.URL.Path, r.Header.Get("If-Unmodified-Since-Version")})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	return srv, writes
}

// `items delete` documents a trash operation and `items restore` reverses one, so
// it must PATCH deleted=1. It previously issued a hard DELETE, destroying the
// item and its child attachments with nothing in the trash to restore.
func TestItemsDeleteTrashesRatherThanDestroying(t *testing.T) {
	var methods []string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", "42")
			_, _ = w.Write([]byte(`{"key":"K","version":42,"data":{}}`))
		case http.MethodPatch:
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode patch body: %v", err)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	cmd := newItemsDeleteCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := runDeleteCmd(t, cmd, srv.URL, "K"); err != nil {
		t.Fatalf("items delete: %v", err)
	}
	for _, m := range methods {
		if m == http.MethodDelete {
			t.Fatalf("items delete issued a hard DELETE; methods = %v", methods)
		}
	}
	if got := body["deleted"]; got != float64(1) {
		t.Fatalf("patch body = %#v, want deleted=1", body)
	}
}

// The permanent form is irreversible, so it must not apply without the gate the
// --allow-destructive help text already advertises for "permanent delete".
func TestItemsDeletePermanentRequiresDestructiveGate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			t.Error("permanent delete applied without --allow-destructive")
		}
		w.Header().Set("Last-Modified-Version", "42")
		_, _ = w.Write([]byte(`{"key":"K","version":42,"data":{}}`))
	}))
	defer srv.Close()

	cmd := newItemsDeleteCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	// Not a fatal error path: the engine reports the op as blocked rather than
	// applying it. What matters is that no DELETE reached the server.
	_ = runDeleteCmd(t, cmd, srv.URL, "--permanent", "K")
}

// Hard deletes must issue exactly one DELETE to the target resource with the
// fetched version as precondition, and no other write (e.g. a trashing PATCH).
func TestDestructiveDeletesSendVersionHeader(t *testing.T) {
	for _, tt := range []struct {
		name, version, path string
		newCmd              func(*rootFlags) *cobra.Command
		args                []string
	}{
		{"items delete --permanent", "42", "/users/0/items/K", newItemsDeleteCmd, []string{"--permanent", "K"}},
		{"collections delete", "7", "/users/0/collections/K", newCollectionsDeleteCmd, []string{"K"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, writes := deleteVersionServer(t, tt.version)
			defer srv.Close()
			// Both are destructive gated mutations: applying needs --yes,
			// --allow-destructive, and an unset max-changes cap.
			cmd := tt.newCmd(&rootFlags{asJSON: true, yes: true, allowDestructive: true, maxChanges: -1})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			if err := runDeleteCmd(t, cmd, srv.URL, tt.args...); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			want := []deleteWrite{{http.MethodDelete, tt.path, tt.version}}
			if !reflect.DeepEqual(*writes, want) {
				t.Errorf("writes = %+v, want exactly %+v", *writes, want)
			}
		})
	}
}

// 7f6875: items delete and items restore returned before renderMutation on a
// failed write, so --agent/--json callers received no envelope on errors
// (412/428 conflicts, write rejections) while update and move rendered first.
// Both must emit the failed envelope AND propagate the error/exit code.
func TestItemMutationsRenderEnvelopeOnFailedWrite(t *testing.T) {
	for _, tt := range []struct {
		name string
		new  func(*rootFlags) *cobra.Command
		args []string
	}{
		{name: "delete", new: newItemsDeleteCmd, args: []string{"K"}},
		{name: "restore", new: newItemsRestoreCmd, args: []string{"K"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					w.Header().Set("Last-Modified-Version", "42")
					_, _ = w.Write([]byte(`{"key":"K","version":42,"data":{"key":"K","version":42}}`))
				case http.MethodPatch, http.MethodDelete:
					http.Error(w, "stale version", http.StatusPreconditionFailed)
				default:
					http.Error(w, "unexpected", http.StatusMethodNotAllowed)
				}
			}))
			defer srv.Close()

			cmd := tt.new(&rootFlags{asJSON: true, agent: true, yes: true, maxChanges: -1})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			// Fails the test when stdout is not the mutation envelope, which
			// is exactly what the early return produced before the fix.
			env, err := deletesTestRunJSON(t, cmd, srv.URL, tt.args...)
			if err == nil {
				t.Fatalf("%s with 412 = nil error, want failure", tt.name)
			}
			if ExitCode(err) != 5 {
				t.Fatalf("ExitCode(%s error) = %d, want 5; err = %v", tt.name, ExitCode(err), err)
			}
			if env.Result == nil || len(env.Result.Items) != 1 {
				t.Fatalf("result = %+v, want one item", env.Result)
			}
			if got := env.Result.Items[0].Status; got != "conflict" {
				t.Fatalf("result status = %q, want %q (412 must map to conflict, not generic failed)", got, "conflict")
			}
		})
	}
}

func TestDeletesAbortWhenVersionReadFails(t *testing.T) {
	fastRetryBackoff(t)
	for _, tt := range []struct {
		name string
		new  func(*rootFlags) interface {
			SetOut(io.Writer)
			SetErr(io.Writer)
			SetArgs([]string)
			Execute() error
		}
	}{
		{name: "items", new: func(flags *rootFlags) interface {
			SetOut(io.Writer)
			SetErr(io.Writer)
			SetArgs([]string)
			Execute() error
		} {
			return newItemsDeleteCmd(flags)
		}},
		{name: "collections", new: func(flags *rootFlags) interface {
			SetOut(io.Writer)
			SetErr(io.Writer)
			SetArgs([]string)
			Execute() error
		} {
			return newCollectionsDeleteCmd(flags)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			deleteIssued := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					http.Error(w, "version service unavailable", http.StatusServiceUnavailable)
				case http.MethodDelete:
					deleteIssued = true
					w.WriteHeader(http.StatusNoContent)
				default:
					http.Error(w, "unexpected", http.StatusMethodNotAllowed)
				}
			}))
			defer srv.Close()

			cmd := tt.new(&rootFlags{asJSON: true, yes: true, allowDestructive: true, maxChanges: -1})
			err := runDeleteCmd(t, cmd, srv.URL, "K")
			if ExitCode(err) != 5 {
				t.Fatalf("ExitCode(delete error) = %d, want 5; err = %v", ExitCode(err), err)
			}
			if deleteIssued {
				t.Fatal("DELETE issued after failed version read")
			}
		})
	}
}

// N4-2: a 404 on the version GET is not "already deleted". A trashed item still
// GETs fine with data.deleted=1, so 404 means the key never existed, was
// permanently destroyed, or — the case that broke this — was created moments ago
// and has not yet propagated to the write plane. Reporting success in that
// window is a false success: the delete never happened and the item resurfaces
// live. Both items delete and collections delete must fail honestly instead,
// exactly like items tags add / items move already do on the identical 404.
func TestDeletesFailHonestlyOnMissingVersionRead(t *testing.T) {
	for _, tt := range []struct {
		name string
		new  func(*rootFlags) interface {
			SetOut(io.Writer)
			SetErr(io.Writer)
			SetArgs([]string)
			Execute() error
		}
	}{
		{name: "items", new: func(flags *rootFlags) interface {
			SetOut(io.Writer)
			SetErr(io.Writer)
			SetArgs([]string)
			Execute() error
		} {
			return newItemsDeleteCmd(flags)
		}},
		{name: "collections", new: func(flags *rootFlags) interface {
			SetOut(io.Writer)
			SetErr(io.Writer)
			SetArgs([]string)
			Execute() error
		} {
			return newCollectionsDeleteCmd(flags)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			deleteIssued := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					http.Error(w, "missing", http.StatusNotFound)
				case http.MethodDelete:
					deleteIssued = true
					w.WriteHeader(http.StatusNoContent)
				default:
					http.Error(w, "unexpected", http.StatusMethodNotAllowed)
				}
			}))
			defer srv.Close()

			cmd := tt.new(&rootFlags{asJSON: true, yes: true, allowDestructive: true, maxChanges: -1})
			err := runDeleteCmd(t, cmd, srv.URL, "K")
			// The report observed rc=3 for the identical 404 on items tags add /
			// items move; delete must land on the same exit code family.
			if ExitCode(err) != 3 {
				t.Fatalf("ExitCode(delete on 404) = %d, want 3 — the false-success bug N4-2 caught; err = %v", ExitCode(err), err)
			}
			if deleteIssued {
				t.Fatal("DELETE issued for a key the version read already 404'd on")
			}
		})
	}
}

// Preview-first contract: a bare items delete/update/restore renders a preview
// and mutation happens only under --yes. The preview (default or --dry-run)
// reads the target through the write plane, so it plans the item's real
// version, and it never writes.
func TestItemMutationsPreviewReadsTheTargetButNeverWrites(t *testing.T) {
	var gets, writes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		gets++
		w.Header().Set("Last-Modified-Version", "42")
		_, _ = w.Write([]byte(`{"key":"ITEMAAA2","version":42,"data":{}}`))
	}))
	defer srv.Close()

	type execCmd interface {
		SetOut(io.Writer)
		SetErr(io.Writer)
		SetArgs([]string)
		Execute() error
	}
	cases := []struct {
		name string
		new  func(flags *rootFlags) execCmd
		args []string
	}{
		{"delete", func(f *rootFlags) execCmd { return newItemsDeleteCmd(f) }, []string{"ITEMAAA2"}},
		{"update", func(f *rootFlags) execCmd { return newItemsUpdateCmd(f) }, []string{"ITEMAAA2", "--title", "x"}},
		{"restore", func(f *rootFlags) execCmd { return newItemsRestoreCmd(f) }, []string{"ITEMAAA2"}},
	}
	for _, tc := range cases {
		for _, dryRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dryRun=%t", tc.name, dryRun), func(t *testing.T) {
				gets, writes = 0, 0
				t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
				cmd := tc.new(&rootFlags{asJSON: true, dryRun: dryRun, maxChanges: -1})
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(io.Discard)
				cmd.SetArgs(tc.args)
				if err := cmd.Execute(); err != nil {
					t.Fatalf("%s preview: %v", tc.name, err)
				}
				if gets == 0 || writes != 0 {
					t.Fatalf("%s preview made %d GET and %d write requests, want reads only", tc.name, gets, writes)
				}
				var env mutation.Envelope
				if err := json.Unmarshal(out.Bytes(), &env); err != nil {
					t.Fatalf("decode envelope: %v\noutput: %s", err, out.String())
				}
				if !env.OK || env.Mode != "preview" || env.Result != nil {
					t.Fatalf("%s envelope = %+v, want an ok preview with no result", tc.name, env)
				}
				if len(env.Plan.Operations) != 1 || env.Plan.Operations[0].ExpectedVersion != 42 {
					t.Fatalf("%s plan = %+v, want expected_version 42 read from the item", tc.name, env.Plan.Operations)
				}
			})
		}
	}
}

// A preview of a missing item fails like the apply does: exit 3, never a plan
// with expected_version 0.
func TestItemMutationPreviewOfMissingItemExitsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not found", http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	for name, newCmd := range map[string]func(*rootFlags) *cobra.Command{
		"delete":  newItemsDeleteCmd,
		"update":  newItemsUpdateCmd,
		"restore": newItemsRestoreCmd,
	} {
		t.Run(name, func(t *testing.T) {
			args := []string{"MISSING2"}
			if name == "update" {
				args = append(args, "--title", "x")
			}
			cmd := newCmd(&rootFlags{asJSON: true, maxChanges: -1})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(args)
			if err := cmd.Execute(); ExitCode(err) != 3 {
				t.Fatalf("ExitCode(%s preview of a missing item) = %d, want 3; err = %v", name, ExitCode(err), err)
			}
		})
	}
}

// deletesTestRunJSON executes a delete command and decodes the standard
// mutation envelope, failing the test if the shape isn't that envelope (e.g.
// the legacy {"status":"noop","reason":...} bypass writeNoop used to produce).
func deletesTestRunJSON(t *testing.T, cmd interface {
	SetOut(io.Writer)
	SetErr(io.Writer)
	SetArgs([]string)
	Execute() error
}, baseURL string, args ...string) (mutation.Envelope, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", baseURL+"/users/0")
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	err := cmd.Execute()
	var env mutation.Envelope
	if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
		t.Fatalf("decode envelope: %v\noutput: %s", decodeErr, out.String())
	}
	return env, err
}

// Trashing an item that is already trashed (data.deleted=1 on the write
// plane's own copy) must no-op rather than send a redundant PATCH: real
// version churn and journal noise for zero effect, the same class as W-6
// (rename-to-itself). --permanent stays exempt — destroying an already-trashed
// item is what that flag is for, not a no-op.
func TestItemsDeleteAlreadyTrashedIsNoOp(t *testing.T) {
	var patched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", "9")
			_, _ = w.Write([]byte(`{"key":"K","version":9,"data":{"deleted":1}}`))
		case http.MethodPatch:
			patched = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	cmd := newItemsDeleteCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	env, err := deletesTestRunJSON(t, cmd, srv.URL, "K")
	if err != nil {
		t.Fatalf("items delete on an already-trashed item: %v", err)
	}
	if patched {
		t.Fatal("PATCHed an item that was already trashed")
	}
	if env.Result == nil || len(env.Result.Items) != 1 {
		t.Fatalf("result = %+v, want one item", env.Result)
	}
	item := env.Result.Items[0]
	if item.Status != "no_op" {
		t.Fatalf("status = %q, want no_op", item.Status)
	}
	reason, ok := item.Reason.(map[string]any)
	if !ok || reason["code"] != "already_deleted" {
		t.Fatalf("reason = %#v, want code already_deleted", item.Reason)
	}
}

// --permanent must still destroy an already-trashed item: that is what the
// flag is for, not a case the no-op above should swallow.
func TestItemsDeletePermanentStillDestroysAnAlreadyTrashedItem(t *testing.T) {
	var deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", "9")
			_, _ = w.Write([]byte(`{"key":"K","version":9,"data":{"deleted":1}}`))
		case http.MethodDelete:
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	cmd := newItemsDeleteCmd(&rootFlags{asJSON: true, yes: true, allowDestructive: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := runDeleteCmd(t, cmd, srv.URL, "--permanent", "K"); err != nil {
		t.Fatalf("items delete --permanent on a trashed item: %v", err)
	}
	if !deleted {
		t.Fatal("--permanent no-op'd on an already-trashed item instead of destroying it")
	}
}

// versionGet404Handler answers the write-plane version GET for key with a
// 404 while rejecting anything else. The old fixtures 404'd every non-DELETE
// request, so a wrong version-GET path or key still reached the same
// classifier and the marker checks passed anyway. The /users/0 prefix comes
// from the test harness base URL (srv.URL + "/users/0"), so it is pinned too.
func versionGet404Handler(t *testing.T, key string, deleteIssued *bool) http.HandlerFunc {
	t.Helper()
	wantPath := "/users/0/items/" + key
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			*deleteIssued = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != wantPath {
			t.Errorf("version read = %s %s, want GET %s", r.Method, r.URL.Path, wantPath)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		http.Error(w, "Item does not exist", http.StatusNotFound)
	}
}

// ADR-0007: a write-plane 404 on a permanent delete is decided by the real
// store. A deletion marker proves this installation already applied the
// delete, so the 404 is an idempotent no-op (not the failure used for an
// unknown key). The inverse window — a live read-plane row with no marker —
// must remain a failure and must preserve that mirror evidence. Neither path
// may issue the DELETE.
func TestItemsDeletePermanent404HonorsRealStoreDeletionMarkers(t *testing.T) {
	for _, tt := range []struct {
		name       string
		key        string
		seed       string
		reap       bool
		wantMarker bool
		check      func(t *testing.T, db *store.Store, out []byte, err error)
	}{
		{
			name:       "deletion marker is an idempotent no-op",
			key:        "PURGED404",
			seed:       `{"key":"PURGED404","version":1,"data":{"key":"PURGED404","itemType":"book","title":"Gone"}}`,
			reap:       true,
			wantMarker: true,
			check: func(t *testing.T, _ *store.Store, out []byte, err error) {
				if err != nil {
					t.Fatalf("repeat permanent delete with a real deletion marker: %v", err)
				}
				var env mutation.Envelope
				if decodeErr := json.Unmarshal(out, &env); decodeErr != nil {
					t.Fatalf("decode envelope: %v\noutput: %s", decodeErr, out)
				}
				if env.Result == nil || len(env.Result.Items) != 1 || env.Result.Items[0].Status != "no_op" {
					t.Fatalf("result = %+v, want one no_op", env.Result)
				}
				reason, ok := env.Result.Items[0].Reason.(map[string]any)
				if !ok || reason["code"] != "already_deleted" {
					t.Fatalf("reason = %#v, want code already_deleted", env.Result.Items[0].Reason)
				}
			},
		},
		{
			name: "mirrored-only lag stays a failure",
			key:  "MIRROR404",
			seed: `{"key":"MIRROR404","data":{"key":"MIRROR404","itemType":"book","title":"Waiting for Zotero sync"}}`,
			check: func(t *testing.T, db *store.Store, _ []byte, err error) {
				if err == nil {
					t.Fatal("permanent delete reported success for a mirrored item absent from the write plane")
				}
				if ExitCode(err) != 3 || !strings.Contains(err.Error(), "mirrored on this machine") || !strings.Contains(err.Error(), "nothing was deleted") {
					t.Fatalf("error = %v (exit %d), want mirrored-only propagation failure", err, ExitCode(err))
				}
				if _, getErr := db.Get("items", "MIRROR404"); getErr != nil {
					t.Fatalf("mirrored-only row was removed after the failed delete: %v", getErr)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ZOTERO_DATA_DIR", t.TempDir())
			t.Setenv("ZOTERO_HOME", "")
			t.Setenv("XDG_DATA_HOME", "")
			t.Setenv("ZOTIO_DEMO", "0")
			savedGroup := activeGroupIDLocked()
			setActiveGroupID("")
			t.Cleanup(func() { setActiveGroupID(savedGroup) })

			db, err := store.OpenWithContext(t.Context(), helpersTestDefaultDBPath(t, "zotio"))
			if err != nil {
				t.Fatalf("open real store: %v", err)
			}
			defer db.Close()
			if _, err := db.UpsertKeyed("items", []string{tt.key}, []json.RawMessage{json.RawMessage(tt.seed)}); err != nil {
				t.Fatalf("seed mirrored item: %v", err)
			}
			if tt.reap {
				if err := db.ReapMirroredItem(tt.key); err != nil {
					t.Fatalf("seed permanent-delete marker: %v", err)
				}
			}

			previousMirror := mirrorWriteThrough
			mirrorWriteThrough = applyMirrorWriteThrough
			t.Cleanup(func() { mirrorWriteThrough = previousMirror })

			deleteIssued := false
			srv := httptest.NewServer(versionGet404Handler(t, tt.key, &deleteIssued))
			defer srv.Close()

			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			cmd := newItemsDeleteCmd(&rootFlags{asJSON: true, yes: true, allowDestructive: true, maxChanges: -1})
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"--permanent", tt.key})
			runErr := cmd.Execute()

			if deleteIssued {
				t.Fatal("issued DELETE after the write-plane version read reported the item absent")
			}
			tt.check(t, db, out.Bytes(), runErr)
			for _, resource := range []string{"items", "items-trash"} {
				marked, markerErr := db.PendingDeletion(resource, tt.key)
				if markerErr != nil {
					t.Fatalf("read %s deletion marker: %v", resource, markerErr)
				}
				if marked != tt.wantMarker {
					t.Fatalf("%s deletion marker = %v, want %v (an idempotent no-op must keep it; a failed delete must not write one)", resource, marked, tt.wantMarker)
				}
			}
		})
	}
}

// --ignore-missing must resolve as a structured no_op through the standard
// mutation envelope, on BOTH commands and BOTH failure points (the version
// read and the write-call race), not the legacy bespoke
// {"status":"noop","reason":...} shape writeNoop produced.
func TestDeleteIgnoreMissingIsAStructuredNoOp(t *testing.T) {
	for _, tt := range []struct {
		name             string
		allowDestructive bool
		new              func(*rootFlags) interface {
			SetOut(io.Writer)
			SetErr(io.Writer)
			SetArgs([]string)
			Execute() error
		}
	}{
		{name: "items", new: func(flags *rootFlags) interface {
			SetOut(io.Writer)
			SetErr(io.Writer)
			SetArgs([]string)
			Execute() error
		} {
			return newItemsDeleteCmd(flags)
		}},
		{name: "collections", allowDestructive: true, new: func(flags *rootFlags) interface {
			SetOut(io.Writer)
			SetErr(io.Writer)
			SetArgs([]string)
			Execute() error
		} {
			return newCollectionsDeleteCmd(flags)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.NotFound(w, r)
			}))
			defer srv.Close()

			cmd := tt.new(&rootFlags{asJSON: true, yes: true, ignoreMissing: true, allowDestructive: tt.allowDestructive, maxChanges: -1})
			env, err := deletesTestRunJSON(t, cmd, srv.URL, "DOESNOTEXIST")
			if err != nil {
				t.Fatalf("%s delete --ignore-missing on a 404: %v", tt.name, err)
			}
			if env.Result == nil || len(env.Result.Items) != 1 {
				t.Fatalf("%s: result = %+v, want one item (the legacy bypass never populated .result at all)", tt.name, env.Result)
			}
			item := env.Result.Items[0]
			if item.Status != "no_op" {
				t.Fatalf("%s: status = %q, want no_op", tt.name, item.Status)
			}
			reason, ok := item.Reason.(map[string]any)
			if !ok || reason["code"] != "already_deleted" {
				t.Fatalf("%s: reason = %#v, want code already_deleted", tt.name, item.Reason)
			}
		})
	}
}

// A destructive command must not silently do less than it was asked. Cobra
// defaults a command with no subcommands to ArbitraryArgs, so
// `items delete K1 K2 --permanent` purged K1 and dropped K2 with no mention of
// it: the request path, the op id and the whole envelope are built from args[0]
// alone. The operator sees one applied delete and reads it as two.
func TestItemsDeleteRefusesMoreThanOneKey(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Last-Modified-Version", "3")
		_, _ = w.Write([]byte(`{"key":"K1","version":3,"data":{}}`))
	}))
	defer srv.Close()

	cmd := newItemsDeleteCmd(&rootFlags{asJSON: true, yes: true, allowDestructive: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := runDeleteCmd(t, cmd, srv.URL, "--permanent", "K1", "K2"); err == nil {
		t.Fatal("items delete accepted two keys; it acts on the first and drops the rest without saying so")
	}
	if requests != 0 {
		t.Errorf("requests = %d, want 0: a refused argument list must not delete anything", requests)
	}
}
