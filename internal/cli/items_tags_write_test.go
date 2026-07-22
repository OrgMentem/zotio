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

	"zotio/internal/mutation"
)

type itemTagTestServer struct {
	server       *httptest.Server
	versions     map[string]string
	tags         map[string][]map[string]any
	patchBodies  map[string]map[string]any
	patchHeaders map[string]string
	getCounts    map[string]int
	patchCounts  map[string]int
}

func newItemTagTestServer(t *testing.T, versions map[string]string, tags map[string][]map[string]any) *itemTagTestServer {
	t.Helper()
	ts := &itemTagTestServer{
		versions:     versions,
		tags:         tags,
		patchBodies:  map[string]map[string]any{},
		patchHeaders: map[string]string{},
		getCounts:    map[string]int{},
		patchCounts:  map[string]int{},
	}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for key := range ts.tags {
			itemPath := "/users/0/items/" + key
			if r.URL.Path != itemPath {
				continue
			}
			switch r.Method {
			case http.MethodGet:
				ts.getCounts[key]++
				version := ts.versions[key]
				w.Header().Set("Last-Modified-Version", version)
				_, _ = fmt.Fprintf(w, `{"key":%q,"version":%s,"data":{"tags":%s}}`, key, version, mustJSON(t, ts.tags[key]))
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
	t.Cleanup(ts.server.Close)
	return ts
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(data)
}

func runItemsTagsTestCmd(t *testing.T, srv *itemTagTestServer, flags *rootFlags, args ...string) (mutation.Envelope, string) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newItemsTagsCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items tags %v: %v; stderr=%s", args, err, errOut.String())
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode mutation envelope %q: %v", out.String(), err)
	}
	return env, errOut.String()
}

func TestItemsTagsAddNewTagApplies(t *testing.T) {
	srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{
		"K1": {{"tag": "existing", "type": float64(0)}},
	})

	env, _ := runItemsTagsTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "add", "--tag", "fresh", "K1")
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want one applied item", env)
	}
	if srv.patchCounts["K1"] != 1 {
		t.Fatalf("PATCH count = %d, want 1", srv.patchCounts["K1"])
	}
	if srv.patchHeaders["K1"] != "42" {
		t.Errorf("If-Unmodified-Since-Version = %q, want 42", srv.patchHeaders["K1"])
	}
	if !patchBodyHasTag(srv.patchBodies["K1"], "fresh") {
		t.Errorf("PATCH body = %+v, want fresh tag", srv.patchBodies["K1"])
	}
}

func TestItemsTagsAddAutomaticTagType(t *testing.T) {
	t.Run("automatic new tag", func(t *testing.T) {
		srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{
			"K1": {{"tag": "existing", "type": float64(0)}},
		})

		env, _ := runItemsTagsTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "add", "--automatic", "--tag", "x", "K1")
		if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 {
			t.Fatalf("env = %+v, want one applied item", env)
		}
		if got := patchBodyTag(srv.patchBodies["K1"], "x"); got["type"] != float64(1) {
			t.Errorf("automatic tag = %+v, want type 1", got)
		}
	})

	t.Run("manual new tag", func(t *testing.T) {
		srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{
			"K1": {{"tag": "existing", "type": float64(0)}},
		})

		env, _ := runItemsTagsTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "add", "--tag", "x", "K1")
		if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 {
			t.Fatalf("env = %+v, want one applied item", env)
		}
		if got := patchBodyTag(srv.patchBodies["K1"], "x"); len(got) != 1 || got["tag"] != "x" {
			t.Errorf("manual tag = %+v, want {tag:x}", got)
		}
	})

	t.Run("existing manual tag is unchanged", func(t *testing.T) {
		srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{
			"K1": {{"tag": "x", "type": float64(0)}},
		})

		env, _ := runItemsTagsTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "add", "--automatic", "--tag", "x", "K1")
		if !env.OK || env.Result == nil || env.Result.Summary.NoOp != 1 || env.Result.Items[0].Status != "no_op" {
			t.Fatalf("env = %+v, want no_op", env)
		}
		if srv.patchCounts["K1"] != 0 {
			t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["K1"])
		}
	})
}

func TestItemsTagsAddAlreadyPresentIsNoOp(t *testing.T) {
	srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{
		"K1": {{"tag": "fresh", "type": float64(0)}},
	})

	env, _ := runItemsTagsTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "add", "--tag", "fresh", "K1")
	if !env.OK || env.Result == nil || env.Result.Summary.NoOp != 1 || env.Result.Items[0].Status != "no_op" {
		t.Fatalf("env = %+v, want no_op", env)
	}
	if srv.patchCounts["K1"] != 0 {
		t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["K1"])
	}
}

func TestItemsTagsRemoveAbsentIsNoOp(t *testing.T) {
	srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{
		"K1": {{"tag": "existing", "type": float64(0)}},
	})

	env, _ := runItemsTagsTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "remove", "--tag", "missing", "K1")
	if !env.OK || env.Result == nil || env.Result.Summary.NoOp != 1 || env.Result.Items[0].Status != "no_op" {
		t.Fatalf("env = %+v, want no_op", env)
	}
	if srv.patchCounts["K1"] != 0 {
		t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["K1"])
	}
}

func TestItemsTagsRemovePresentApplies(t *testing.T) {
	srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{
		"K1": {{"tag": "keep", "type": float64(0)}, {"tag": "drop", "type": float64(0)}},
	})

	env, _ := runItemsTagsTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "remove", "--tag", "drop", "K1")
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Items[0].Status != "applied" {
		t.Fatalf("env = %+v, want applied remove", env)
	}
	if srv.patchHeaders["K1"] != "42" {
		t.Errorf("If-Unmodified-Since-Version = %q, want 42", srv.patchHeaders["K1"])
	}
	if patchBodyHasTag(srv.patchBodies["K1"], "drop") || !patchBodyHasTag(srv.patchBodies["K1"], "keep") {
		t.Errorf("PATCH body = %+v, want drop removed and keep preserved", srv.patchBodies["K1"])
	}
}

func TestItemsTagsPreviewWritesNothing(t *testing.T) {
	srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{
		"K1": {{"tag": "existing", "type": float64(0)}},
	})

	env, _ := runItemsTagsTestCmd(t, srv, &rootFlags{asJSON: true, maxChanges: -1}, "add", "--tag", "fresh", "K1")
	if !env.OK || env.Mode != "preview" || env.Result != nil || env.Plan.Summary.Planned != 1 {
		t.Fatalf("env = %+v, want preview plan with one change", env)
	}
	if srv.patchCounts["K1"] != 0 {
		t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["K1"])
	}
}

func TestItemsTagsDryRunAvoidsVersionFetch(t *testing.T) {
	srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{
		"K1": {{"tag": "existing", "type": float64(0)}},
	})

	env, _ := runItemsTagsTestCmd(t, srv, &rootFlags{asJSON: true, dryRun: true, maxChanges: -1}, "add", "--tag", "fresh", "K1")
	if !env.OK || env.Mode != "preview" || env.PreviewReason != "dry_run" || env.Result != nil || env.Plan.Summary.Planned != 1 {
		t.Fatalf("env = %+v, want dry-run preview with one planned change", env)
	}
	if srv.getCounts["K1"] != 0 || srv.patchCounts["K1"] != 0 {
		t.Fatalf("requests: GET=%d PATCH=%d, want none", srv.getCounts["K1"], srv.patchCounts["K1"])
	}
}

func TestItemsTagsRemoveDryRunAvoidsVersionFetch(t *testing.T) {
	srv := newItemTagTestServer(t, map[string]string{"K1": "42"}, map[string][]map[string]any{
		"K1": {{"tag": "existing", "type": float64(0)}},
	})

	env, _ := runItemsTagsTestCmd(t, srv, &rootFlags{asJSON: true, dryRun: true, maxChanges: -1}, "remove", "--tag", "existing", "K1")
	if !env.OK || env.Mode != "preview" || env.PreviewReason != "dry_run" || env.Result != nil || env.Plan.Summary.Planned != 1 {
		t.Fatalf("env = %+v, want dry-run preview with one planned change", env)
	}
	if srv.getCounts["K1"] != 0 || srv.patchCounts["K1"] != 0 {
		t.Fatalf("requests: GET=%d PATCH=%d, want none", srv.getCounts["K1"], srv.patchCounts["K1"])
	}
}

func TestItemsTagsBulkAddKeysFrom(t *testing.T) {
	srv := newItemTagTestServer(t, map[string]string{"K1": "42", "K2": "43"}, map[string][]map[string]any{
		"K1": {{"tag": "one", "type": float64(0)}},
		"K2": {{"tag": "two", "type": float64(0)}},
	})
	keysPath := filepath.Join(t.TempDir(), "keys.txt")
	if err := os.WriteFile(keysPath, []byte("K1\nK2\n"), 0o600); err != nil {
		t.Fatalf("write keys file: %v", err)
	}

	env, _ := runItemsTagsTestCmd(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "add", "--tag", "bulk", "--keys-from", keysPath)
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 2 || len(env.Result.Items) != 2 {
		t.Fatalf("env = %+v, want two applied items", env)
	}
	for _, key := range []string{"K1", "K2"} {
		if srv.patchCounts[key] != 1 {
			t.Fatalf("%s PATCH count = %d, want 1", key, srv.patchCounts[key])
		}
		if !patchBodyHasTag(srv.patchBodies[key], "bulk") {
			t.Errorf("%s PATCH body = %+v, want bulk tag", key, srv.patchBodies[key])
		}
	}
}

func TestItemsTagsBareReadDeprecatedAlias(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/users/0/items/K1/tags" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`[{"tag":"existing","type":0}]`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	cmd := newItemsTagsCmd(&rootFlags{asJSON: true, dataSource: "live"})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"K1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items tags legacy read: %v; stderr=%s", err, errOut.String())
	}
	if !strings.Contains(errOut.String(), `note: "items tags <key>" is deprecated; use "items tags list <key>"`) {
		t.Fatalf("stderr = %q, want deprecation note", errOut.String())
	}
	if !strings.Contains(out.String(), "existing") {
		t.Fatalf("stdout = %q, want existing tag", out.String())
	}
}

func patchBodyHasTag(body map[string]any, tagName string) bool {
	rawTags, _ := body["tags"].([]any)
	for _, raw := range rawTags {
		tagObj, _ := raw.(map[string]any)
		if currentTag, ok := tagObj["tag"].(string); ok && currentTag == tagName {
			return true
		}
	}
	return false
}

func patchBodyTag(body map[string]any, tagName string) map[string]any {
	rawTags, _ := body["tags"].([]any)
	for _, raw := range rawTags {
		tagObj, _ := raw.(map[string]any)
		if tagObj["tag"] == tagName {
			return tagObj
		}
	}
	return nil
}
