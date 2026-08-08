// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Zotero requires If-Unmodified-Since-Version on DELETE; items/collections
// delete must fetch the current version and send it (else HTTP 428).

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"zotio/internal/mutation"
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

func deleteVersionServer(t *testing.T, version string) (*httptest.Server, *string) {
	t.Helper()
	sent := new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Last-Modified-Version", version)
			_, _ = w.Write([]byte(`{"key":"K","version":` + version + `,"data":{}}`))
		case http.MethodDelete, http.MethodPatch:
			*sent = r.Header.Get("If-Unmodified-Since-Version")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	return srv, sent
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

// With the gate, --permanent still does the hard delete and sends the precondition.
func TestItemsDeletePermanentSendsVersionHeader(t *testing.T) {
	srv, sent := deleteVersionServer(t, "42")
	defer srv.Close()
	cmd := newItemsDeleteCmd(&rootFlags{asJSON: true, yes: true, allowDestructive: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := runDeleteCmd(t, cmd, srv.URL, "--permanent", "K"); err != nil {
		t.Fatalf("items delete --permanent: %v", err)
	}
	if *sent != "42" {
		t.Errorf("If-Unmodified-Since-Version = %q, want 42", *sent)
	}
}

func TestCollectionsDeleteSendsVersionHeader(t *testing.T) {
	srv, sent := deleteVersionServer(t, "7")
	defer srv.Close()
	// collections delete is now a destructive gated mutation: applying it needs
	// --allow-destructive as well as --yes, and an unset max-changes cap.
	cmd := newCollectionsDeleteCmd(&rootFlags{asJSON: true, yes: true, allowDestructive: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	if err := runDeleteCmd(t, cmd, srv.URL, "K"); err != nil {
		t.Fatalf("collections delete: %v", err)
	}
	if *sent != "7" {
		t.Errorf("If-Unmodified-Since-Version = %q, want 7", *sent)
	}
}

func TestDeletesAbortWhenVersionReadFails(t *testing.T) {
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

// Preview-first contract (0.11.0): a bare items delete/update/restore renders a
// preview and issues no HTTP request; mutation happens only under --yes.
func TestItemMutationsPreviewByDefault(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	type execCmd interface {
		SetOut(io.Writer)
		SetErr(io.Writer)
		SetArgs([]string)
		Execute() error
	}
	cases := []struct {
		name string
		cmd  execCmd
		args []string
	}{
		{"delete", newItemsDeleteCmd(&rootFlags{asJSON: true}), []string{"K"}},
		{"update", newItemsUpdateCmd(&rootFlags{asJSON: true}), []string{"K", "--title", "x"}},
		{"restore", newItemsRestoreCmd(&rootFlags{asJSON: true}), []string{"K"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requests = 0
			var out bytes.Buffer
			tc.cmd.SetOut(&out)
			tc.cmd.SetErr(io.Discard)
			tc.cmd.SetArgs(tc.args)
			if err := tc.cmd.Execute(); err != nil {
				t.Fatalf("%s preview: %v", tc.name, err)
			}
			if requests != 0 {
				t.Fatalf("%s issued %d HTTP requests in preview mode, want 0", tc.name, requests)
			}
			// items delete now renders the shared mutation envelope like every
			// other mutating command, which marks a preview with mode/preview_reason
			// rather than the bespoke dry_run flag the others still emit.
			if !bytes.Contains(out.Bytes(), []byte(`"dry_run"`)) &&
				!bytes.Contains(out.Bytes(), []byte(`"mode": "preview"`)) {
				t.Fatalf("%s preview output missing preview marker: %s", tc.name, out.String())
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
