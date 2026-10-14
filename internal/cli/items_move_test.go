// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

type writePlaneTestServer struct {
	server       *httptest.Server
	field        string
	versions     map[string]string
	items        map[string]any
	patchBodies  map[string]map[string]any
	patchHeaders map[string]string
	getCounts    map[string]int
	patchCounts  map[string]int
	// listRequests records the itemKey query of every batched planning read.
	listRequests []string
}

func writePlaneTestNewItemServer[T any](t *testing.T, field string, versions map[string]string, values map[string]T) *writePlaneTestServer {
	t.Helper()
	items := make(map[string]any, len(values))
	for key, value := range values {
		items[key] = value
	}
	ts := &writePlaneTestServer{
		field:        field,
		versions:     versions,
		items:        items,
		patchBodies:  map[string]map[string]any{},
		patchHeaders: map[string]string{},
		getCounts:    map[string]int{},
		patchCounts:  map[string]int{},
	}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Batched planning read: GET /items?itemKey=K1,K2,... answers with the
		// requested items that exist, each carrying its own version.
		if r.Method == http.MethodGet && r.URL.Path == "/users/0/items" && r.URL.Query().Get("itemKey") != "" {
			ts.listRequests = append(ts.listRequests, r.URL.Query().Get("itemKey"))
			rows := make([]string, 0)
			for _, key := range strings.Split(r.URL.Query().Get("itemKey"), ",") {
				value, ok := ts.items[key]
				if !ok {
					continue
				}
				ts.getCounts[key]++
				rows = append(rows, fmt.Sprintf(`{"key":%q,"version":%s,"data":{%q:%s}}`, key, ts.versions[key], ts.field, mustJSON(t, value)))
			}
			_, _ = fmt.Fprintf(w, "[%s]", strings.Join(rows, ","))
			return
		}
		for key, value := range ts.items {
			itemPath := "/users/0/items/" + key
			if r.URL.Path != itemPath {
				continue
			}
			switch r.Method {
			case http.MethodGet:
				ts.getCounts[key]++
				version := ts.versions[key]
				w.Header().Set("Last-Modified-Version", version)
				_, _ = fmt.Fprintf(w, `{"key":%q,"version":%s,"data":{%q:%s}}`, key, version, ts.field, mustJSON(t, value))
			case http.MethodPatch:
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode patch body: %v", err)
				}
				ts.patchBodies[key] = body
				ts.patchHeaders[key] = r.Header.Get("If-Unmodified-Since-Version")
				ts.patchCounts[key]++
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			}
			return
		}
		http.Error(w, "unexpected path", http.StatusNotFound)
	}))
	t.Setenv("ZOTERO_BASE_URL", ts.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Cleanup(ts.server.Close)
	return ts
}

func writePlaneTestRunMutationCmd(t *testing.T, newCmd func(*rootFlags) *cobra.Command, flags *rootFlags, args ...string) (mutation.Envelope, string, error) {
	t.Helper()
	cmd := newCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	var env mutation.Envelope
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode mutation envelope %q: %v", out.String(), decodeErr)
		}
	}
	return env, errOut.String(), err
}

func writePlaneTestMustRunMutationCmd(t *testing.T, label string, newCmd func(*rootFlags) *cobra.Command, flags *rootFlags, args ...string) mutation.Envelope {
	t.Helper()
	env, stderr, err := writePlaneTestRunMutationCmd(t, newCmd, flags, args...)
	if err != nil {
		t.Fatalf("%s %v: %v; stderr=%s", label, args, err, stderr)
	}
	return env
}

type itemMoveTestServer = writePlaneTestServer

func newItemMoveTestServer(t *testing.T, versions map[string]string, collections map[string][]string) *itemMoveTestServer {
	t.Helper()
	return writePlaneTestNewItemServer(t, "collections", versions, collections)
}

func mustRunItemsMoveTestCmd(t *testing.T, srv *itemMoveTestServer, flags *rootFlags, args ...string) mutation.Envelope {
	t.Helper()
	return writePlaneTestMustRunMutationCmd(t, "items move", newItemsMoveCmd, flags, args...)
}

func writePlaneTestPatchBodyCollections(t *testing.T, body map[string]any) []string {
	t.Helper()
	rawCollections, ok := body["collections"].([]any)
	if !ok {
		t.Fatalf("PATCH body = %+v, missing collections", body)
	}
	collections := make([]string, 0, len(rawCollections))
	for _, raw := range rawCollections {
		collection, ok := raw.(string)
		if !ok {
			t.Fatalf("PATCH collection = %#v, want string", raw)
		}
		collections = append(collections, collection)
	}
	return collections
}

func TestItemsMoveToAddsCollection(t *testing.T) {
	srv := writePlaneTestNewItemServer(t, "collections", map[string]string{"KEYAAAA2": "42"}, map[string][]string{
		"KEYAAAA2": {"SRCAAAA2"},
	})

	env := writePlaneTestMustRunMutationCmd(t, "items move", newItemsMoveCmd, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--to", "TARGET22", "KEYAAAA2")
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want one applied item", env)
	}
	if srv.patchCounts["KEYAAAA2"] != 1 {
		t.Fatalf("PATCH count = %d, want 1", srv.patchCounts["KEYAAAA2"])
	}
	if srv.patchHeaders["KEYAAAA2"] != "42" {
		t.Errorf("If-Unmodified-Since-Version = %q, want 42", srv.patchHeaders["KEYAAAA2"])
	}
	collections := writePlaneTestPatchBodyCollections(t, srv.patchBodies["KEYAAAA2"])
	if !stringSliceContains(collections, "SRCAAAA2") || !stringSliceContains(collections, "TARGET22") {
		t.Errorf("PATCH collections = %v, want SRCAAAA2 and TARGET22", collections)
	}
}

func TestItemsMoveAlreadyInTargetIsNoOp(t *testing.T) {
	srv := writePlaneTestNewItemServer(t, "collections", map[string]string{"KEYAAAA2": "42"}, map[string][]string{
		"KEYAAAA2": {"TARGET22"},
	})

	env := writePlaneTestMustRunMutationCmd(t, "items move", newItemsMoveCmd, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--to", "TARGET22", "KEYAAAA2")
	if !env.OK || env.Result == nil || env.Result.Summary.NoOp != 1 || env.Result.Items[0].Status != "no_op" {
		t.Fatalf("env = %+v, want no_op", env)
	}
	if srv.patchCounts["KEYAAAA2"] != 0 {
		t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["KEYAAAA2"])
	}
	// A bare {"status":"no_op"} is indistinguishable from a missing item or
	// collection, so the reason must name which no-op this was.
	reason, ok := env.Result.Items[0].Reason.(map[string]any)
	if !ok {
		t.Fatalf("reason = %#v, want a structured reason", env.Result.Items[0].Reason)
	}
	if reason["code"] != "already_member" {
		t.Errorf("reason code = %v, want already_member", reason["code"])
	}
	if reason["to"] != "TARGET22" {
		t.Errorf("reason target = %v, want TARGET22", reason["to"])
	}
	if _, ok := reason["message"].(string); !ok {
		t.Errorf("reason %#v carries no human message", reason)
	}
}

func TestItemsMoveFromRemovesCollection(t *testing.T) {
	srv := writePlaneTestNewItemServer(t, "collections", map[string]string{"KEYAAAA2": "42"}, map[string][]string{
		"KEYAAAA2": {"SRCAAAA2", "KEEP2222"},
	})

	env := writePlaneTestMustRunMutationCmd(t, "items move", newItemsMoveCmd, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--from", "SRCAAAA2", "KEYAAAA2")
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want applied remove", env)
	}
	collections := writePlaneTestPatchBodyCollections(t, srv.patchBodies["KEYAAAA2"])
	if stringSliceContains(collections, "SRCAAAA2") || !stringSliceContains(collections, "KEEP2222") {
		t.Errorf("PATCH collections = %v, want SRCAAAA2 removed and KEEP2222 preserved", collections)
	}
}

func TestItemsMoveFromToMovesCollection(t *testing.T) {
	srv := writePlaneTestNewItemServer(t, "collections", map[string]string{"KEYAAAA2": "42"}, map[string][]string{
		"KEYAAAA2": {"SRCAAAA2", "KEEP2222"},
	})

	env := writePlaneTestMustRunMutationCmd(t, "items move", newItemsMoveCmd, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--from", "SRCAAAA2", "--to", "TARGET22", "KEYAAAA2")
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want applied move", env)
	}
	if len(env.Plan.Operations) != 1 || len(env.Plan.Operations[0].Changes) != 2 {
		t.Fatalf("changes = %+v, want remove and add", env.Plan.Operations)
	}
	collections := writePlaneTestPatchBodyCollections(t, srv.patchBodies["KEYAAAA2"])
	if stringSliceContains(collections, "SRCAAAA2") || !stringSliceContains(collections, "TARGET22") || !stringSliceContains(collections, "KEEP2222") {
		t.Errorf("PATCH collections = %v, want SRCAAAA2 removed, TARGET22 added, KEEP2222 preserved", collections)
	}
}

func TestItemsMoveRequiresToOrFrom(t *testing.T) {
	srv := writePlaneTestNewItemServer(t, "collections", map[string]string{"KEYAAAA2": "42"}, map[string][]string{
		"KEYAAAA2": {"SRCAAAA2"},
	})

	_, _, err := writePlaneTestRunMutationCmd(t, newItemsMoveCmd, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "KEYAAAA2")
	if err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "--to or --from") {
		t.Fatalf("err = %v (exit %d), want exit 2 --to/--from usage error", err, ExitCode(err))
	}
	if srv.patchCounts["KEYAAAA2"] != 0 {
		t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["KEYAAAA2"])
	}
}

func TestItemsMoveBulkKeysFrom(t *testing.T) {
	srv := writePlaneTestNewItemServer(t, "collections", map[string]string{"KEYAAAA2": "42", "KEYAAAA3": "43"}, map[string][]string{
		"KEYAAAA2": {"ONE"},
		"KEYAAAA3": {"TWO"},
	})
	keysPath := filepath.Join(t.TempDir(), "keys.txt")
	if err := os.WriteFile(keysPath, []byte("KEYAAAA2\nKEYAAAA3\n"), 0o600); err != nil {
		t.Fatalf("write keys file: %v", err)
	}

	env := writePlaneTestMustRunMutationCmd(t, "items move", newItemsMoveCmd, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--to", "BULK2222", "--keys-from", keysPath)
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 2 || len(env.Result.Items) != 2 {
		t.Fatalf("env = %+v, want two applied items", env)
	}
	for _, key := range []string{"KEYAAAA2", "KEYAAAA3"} {
		if srv.patchCounts[key] != 1 {
			t.Fatalf("%s PATCH count = %d, want 1", key, srv.patchCounts[key])
		}
		if !stringSliceContains(writePlaneTestPatchBodyCollections(t, srv.patchBodies[key]), "BULK2222") {
			t.Errorf("%s PATCH body = %+v, want BULK2222 collection", key, srv.patchBodies[key])
		}
	}
}

// Preview and apply run the same reads: the default preview fetches each item
// from the write plane, plans with its real version, and writes nothing.
func TestItemsMoveDefaultPreviewReadsButNeverWrites(t *testing.T) {
	srv := newItemMoveTestServer(t, map[string]string{"KEYAAAA2": "42"}, map[string][]string{"KEYAAAA2": {"SRCAAAA2"}})

	env := mustRunItemsMoveTestCmd(t, srv, &rootFlags{asJSON: true, maxChanges: -1}, "--to", "TARGET22", "KEYAAAA2")
	if !env.OK || env.Mode != "preview" {
		t.Fatalf("env = %+v, want ok preview", env)
	}
	if srv.getCounts["KEYAAAA2"] != 1 || len(srv.patchCounts) != 0 {
		t.Fatalf("GETs = %v PATCHes = %v, want one GET and no PATCH", srv.getCounts, srv.patchCounts)
	}
	if len(env.Plan.Operations) != 1 || env.Plan.Operations[0].ExpectedVersion != 42 {
		t.Fatalf("plan = %+v, want expected_version 42 read from the item", env.Plan.Operations)
	}
}

// A preview of a missing item fails like the apply: exit 3, not a plan with
// expected_version 0.
func TestItemsMovePreviewOfMissingItemExitsNotFound(t *testing.T) {
	newItemMoveTestServer(t, map[string]string{"KEYAAAA2": "42"}, map[string][]string{"KEYAAAA2": {"SRCAAAA2"}})

	for _, flags := range []*rootFlags{
		{asJSON: true, maxChanges: -1},
		{asJSON: true, dryRun: true, maxChanges: -1},
	} {
		_, _, err := writePlaneTestRunMutationCmd(t, newItemsMoveCmd, flags, "--to", "TARGET22", "MISSING2")
		if ExitCode(err) != 3 {
			t.Fatalf("ExitCode(preview move of a missing item, dryRun=%v) = %d, want 3; err = %v", flags.dryRun, ExitCode(err), err)
		}
	}
}
