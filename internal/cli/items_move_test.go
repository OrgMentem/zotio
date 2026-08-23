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

	"zotio/internal/client"
	"zotio/internal/config"
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
	srv := writePlaneTestNewItemServer(t, "collections", map[string]string{"K1": "42"}, map[string][]string{
		"K1": {"SOURCE"},
	})

	env := writePlaneTestMustRunMutationCmd(t, "items move", newItemsMoveCmd, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--to", "TARGET", "K1")
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want one applied item", env)
	}
	if srv.patchCounts["K1"] != 1 {
		t.Fatalf("PATCH count = %d, want 1", srv.patchCounts["K1"])
	}
	if srv.patchHeaders["K1"] != "42" {
		t.Errorf("If-Unmodified-Since-Version = %q, want 42", srv.patchHeaders["K1"])
	}
	collections := writePlaneTestPatchBodyCollections(t, srv.patchBodies["K1"])
	if !stringSliceContains(collections, "SOURCE") || !stringSliceContains(collections, "TARGET") {
		t.Errorf("PATCH collections = %v, want SOURCE and TARGET", collections)
	}
}

func TestItemsMoveAlreadyInTargetIsNoOp(t *testing.T) {
	srv := writePlaneTestNewItemServer(t, "collections", map[string]string{"K1": "42"}, map[string][]string{
		"K1": {"TARGET"},
	})

	env := writePlaneTestMustRunMutationCmd(t, "items move", newItemsMoveCmd, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--to", "TARGET", "K1")
	if !env.OK || env.Result == nil || env.Result.Summary.NoOp != 1 || env.Result.Items[0].Status != "no_op" {
		t.Fatalf("env = %+v, want no_op", env)
	}
	if srv.patchCounts["K1"] != 0 {
		t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["K1"])
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
	if reason["to"] != "TARGET" {
		t.Errorf("reason target = %v, want TARGET", reason["to"])
	}
	if _, ok := reason["message"].(string); !ok {
		t.Errorf("reason %#v carries no human message", reason)
	}
}

func TestItemsMoveFromRemovesCollection(t *testing.T) {
	srv := writePlaneTestNewItemServer(t, "collections", map[string]string{"K1": "42"}, map[string][]string{
		"K1": {"SOURCE", "KEEP"},
	})

	env := writePlaneTestMustRunMutationCmd(t, "items move", newItemsMoveCmd, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--from", "SOURCE", "K1")
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want applied remove", env)
	}
	collections := writePlaneTestPatchBodyCollections(t, srv.patchBodies["K1"])
	if stringSliceContains(collections, "SOURCE") || !stringSliceContains(collections, "KEEP") {
		t.Errorf("PATCH collections = %v, want SOURCE removed and KEEP preserved", collections)
	}
}

func TestItemsMoveFromToMovesCollection(t *testing.T) {
	srv := writePlaneTestNewItemServer(t, "collections", map[string]string{"K1": "42"}, map[string][]string{
		"K1": {"SOURCE", "KEEP"},
	})

	env := writePlaneTestMustRunMutationCmd(t, "items move", newItemsMoveCmd, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--from", "SOURCE", "--to", "TARGET", "K1")
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want applied move", env)
	}
	if len(env.Plan.Operations) != 1 || len(env.Plan.Operations[0].Changes) != 2 {
		t.Fatalf("changes = %+v, want remove and add", env.Plan.Operations)
	}
	collections := writePlaneTestPatchBodyCollections(t, srv.patchBodies["K1"])
	if stringSliceContains(collections, "SOURCE") || !stringSliceContains(collections, "TARGET") || !stringSliceContains(collections, "KEEP") {
		t.Errorf("PATCH collections = %v, want SOURCE removed, TARGET added, KEEP preserved", collections)
	}
}

func TestItemsMoveRequiresToOrFrom(t *testing.T) {
	srv := writePlaneTestNewItemServer(t, "collections", map[string]string{"K1": "42"}, map[string][]string{
		"K1": {"SOURCE"},
	})

	_, _, err := writePlaneTestRunMutationCmd(t, newItemsMoveCmd, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "K1")
	if err == nil || !strings.Contains(err.Error(), "--to or --from") {
		t.Fatalf("err = %v, want --to/--from usage error", err)
	}
	if srv.patchCounts["K1"] != 0 {
		t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["K1"])
	}
}

func TestItemsMoveBulkKeysFrom(t *testing.T) {
	srv := writePlaneTestNewItemServer(t, "collections", map[string]string{"K1": "42", "K2": "43"}, map[string][]string{
		"K1": {"ONE"},
		"K2": {"TWO"},
	})
	keysPath := filepath.Join(t.TempDir(), "keys.txt")
	if err := os.WriteFile(keysPath, []byte("K1\nK2\n"), 0o600); err != nil {
		t.Fatalf("write keys file: %v", err)
	}

	env := writePlaneTestMustRunMutationCmd(t, "items move", newItemsMoveCmd, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--to", "BULK", "--keys-from", keysPath)
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 2 || len(env.Result.Items) != 2 {
		t.Fatalf("env = %+v, want two applied items", env)
	}
	for _, key := range []string{"K1", "K2"} {
		if srv.patchCounts[key] != 1 {
			t.Fatalf("%s PATCH count = %d, want 1", key, srv.patchCounts[key])
		}
		if !stringSliceContains(writePlaneTestPatchBodyCollections(t, srv.patchBodies[key]), "BULK") {
			t.Errorf("%s PATCH body = %+v, want BULK collection", key, srv.patchBodies[key])
		}
	}
}

func TestItemsMovePreviewWritesNothing(t *testing.T) {
	srv := writePlaneTestNewItemServer(t, "collections", map[string]string{"K1": "42"}, map[string][]string{
		"K1": {"SOURCE"},
	})

	env := writePlaneTestMustRunMutationCmd(t, "items move", newItemsMoveCmd, &rootFlags{asJSON: true, maxChanges: -1}, "--to", "TARGET", "K1")
	if !env.OK || env.Mode != "preview" || env.Result != nil || env.Plan.Summary.Planned != 1 {
		t.Fatalf("env = %+v, want preview plan with one change", env)
	}
	if srv.patchCounts["K1"] != 0 {
		t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["K1"])
	}
}

func TestItemsMoveDryRunAvoidsVersionFetch(t *testing.T) {
	srv := writePlaneTestNewItemServer(t, "collections", map[string]string{"K1": "42"}, map[string][]string{
		"K1": {"SOURCE"},
	})

	env := writePlaneTestMustRunMutationCmd(t, "items move", newItemsMoveCmd, &rootFlags{asJSON: true, dryRun: true, maxChanges: -1}, "--to", "TARGET", "K1")
	if !env.OK || env.Mode != "preview" || env.PreviewReason != "dry_run" || env.Result != nil || env.Plan.Summary.Planned != 1 {
		t.Fatalf("env = %+v, want dry-run preview with one planned change", env)
	}
	if srv.getCounts["K1"] != 0 || srv.patchCounts["K1"] != 0 {
		t.Fatalf("requests: GET=%d PATCH=%d, want none", srv.getCounts["K1"], srv.patchCounts["K1"])
	}
}

func TestPatchItemCollectionsFailsClosedWithoutVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("PATCH must not be dispatched when version is 0; got %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected PATCH", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	c := client.New(&config.Config{BaseURL: srv.URL + "/users/0"}, 0, 0)
	c.NoCache = true
	status, reason, err := patchItemCollections(c, "/users/0/items/K1", 0, []string{"TARGET"})
	if err == nil {
		t.Fatalf("patchItemCollections with version 0: err = nil, want error")
	}
	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	msg, _ := reason.(string)
	if !strings.Contains(strings.ToLower(msg), "write-plane version") && !strings.Contains(strings.ToLower(msg), "if-unmodified-since-version") {
		t.Fatalf("reason = %q, want missing write-plane precondition", msg)
	}
}

func TestApplyItemCollectionMoveFailsClosedOnZeroVersion(t *testing.T) {
	srv := writePlaneTestNewItemServer(t, "collections", map[string]string{"K1": "0"}, map[string][]string{
		"K1": {"SOURCE"},
	})
	env, _, _ := writePlaneTestRunMutationCmd(t, newItemsMoveCmd, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "--to", "TARGET", "K1")
	if env.Result == nil || len(env.Result.Items) != 1 {
		t.Fatalf("env = %+v, want one result", env)
	}
	if env.Result.Items[0].Status != "failed" {
		t.Fatalf("status = %q, want failed (zero version must fail closed)", env.Result.Items[0].Status)
	}
	if srv.patchCounts["K1"] != 0 {
		t.Fatalf("PATCH count = %d, want 0 (no request when version is 0)", srv.patchCounts["K1"])
	}
}
