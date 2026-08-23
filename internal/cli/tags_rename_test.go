// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"zotio/internal/client"
	"zotio/internal/config"
	"zotio/internal/mutation"
)

type tagRenamePageRequest struct {
	Start int
	Limit int
}

type tagRenameCommandTestServer struct {
	server       *httptest.Server
	keys         []string
	versions     map[string]int
	patchBodies  map[string]map[string]any
	patchHeaders map[string]string
	patchCounts  map[string]int
}

func newTagRenameCommandTestServer(t *testing.T, keys []string) *tagRenameCommandTestServer {
	t.Helper()
	ts := &tagRenameCommandTestServer{
		keys:         keys,
		versions:     map[string]int{},
		patchBodies:  map[string]map[string]any{},
		patchHeaders: map[string]string{},
		patchCounts:  map[string]int{},
	}
	for i, key := range keys {
		ts.versions[key] = 10 + i
	}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/0/items":
			if got := r.URL.Query().Get("tag"); got != "foo" {
				t.Errorf("tag query = %q, want foo", got)
				http.Error(w, "bad tag", http.StatusBadRequest)
				return
			}
			limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
			if err != nil {
				t.Errorf("limit query = %q: %v", r.URL.Query().Get("limit"), err)
				http.Error(w, "bad limit", http.StatusBadRequest)
				return
			}
			start, err := strconv.Atoi(r.URL.Query().Get("start"))
			if err != nil {
				t.Errorf("start query = %q: %v", r.URL.Query().Get("start"), err)
				http.Error(w, "bad start", http.StatusBadRequest)
				return
			}
			end := start + limit
			if start > len(ts.keys) {
				start = len(ts.keys)
			}
			if end > len(ts.keys) {
				end = len(ts.keys)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("["))
			for i, key := range ts.keys[start:end] {
				if i > 0 {
					_, _ = w.Write([]byte(","))
				}
				_, _ = fmt.Fprintf(w, `{"key":%q,"version":%d,"data":{"key":%q,"tags":[{"tag":"foo","type":0},{"tag":"keep","type":1}]}}`, key, ts.versions[key], key)
			}
			_, _ = w.Write([]byte("]"))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/users/0/items/"):
			// Apply re-reads the item from the write plane to resolve the
			// precondition version.
			key := strings.TrimPrefix(r.URL.Path, "/users/0/items/")
			version, ok := ts.versions[key]
			if !ok {
				http.Error(w, "unknown item", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"key":%q,"version":%d,"data":{"key":%q,"tags":[{"tag":"foo","type":0},{"tag":"keep","type":1}]}}`, key, version, key)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/users/0/items/"):
			key := strings.TrimPrefix(r.URL.Path, "/users/0/items/")
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode patch body: %v", err)
			}
			ts.patchBodies[key] = body
			ts.patchHeaders[key] = r.Header.Get("If-Unmodified-Since-Version")
			ts.patchCounts[key]++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Setenv("ZOTERO_BASE_URL", ts.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Cleanup(ts.server.Close)
	return ts
}

func totalTagRenamePatchCount(srv *tagRenameCommandTestServer) int {
	total := 0
	for _, count := range srv.patchCounts {
		total += count
	}
	return total
}

func TestListTagRenameUpdatesWalksMultiplePages(t *testing.T) {
	items := []string{
		`{"key":"K0","version":10,"data":{"key":"K0","tags":[{"tag":"old","type":0}]}}`,
		`{"key":"K1","version":11,"data":{"key":"K1","tags":[{"tag":"old","type":0}]}}`,
		`{"key":"K2","version":12,"data":{"key":"K2","tags":[{"tag":"old","type":0}]}}`,
		`{"key":"K3","version":13,"data":{"key":"K3","tags":[{"tag":"old","type":0}]}}`,
	}
	var requests []tagRenamePageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/users/0/items" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("tag"); got != "old" {
			t.Errorf("tag query = %q, want old", got)
			http.Error(w, "bad tag", http.StatusBadRequest)
			return
		}
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil {
			t.Errorf("limit query = %q: %v", r.URL.Query().Get("limit"), err)
			http.Error(w, "bad limit", http.StatusBadRequest)
			return
		}
		start, err := strconv.Atoi(r.URL.Query().Get("start"))
		if err != nil {
			t.Errorf("start query = %q: %v", r.URL.Query().Get("start"), err)
			http.Error(w, "bad start", http.StatusBadRequest)
			return
		}
		requests = append(requests, tagRenamePageRequest{Start: start, Limit: limit})
		end := start + limit
		if start > len(items) {
			start = len(items)
		}
		if end > len(items) {
			end = len(items)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("["))
		for i, item := range items[start:end] {
			if i > 0 {
				_, _ = w.Write([]byte(","))
			}
			_, _ = w.Write([]byte(item))
		}
		_, _ = w.Write([]byte("]"))
	}))
	defer srv.Close()

	c := client.New(&config.Config{BaseURL: srv.URL + "/users/0"}, 5*time.Second, 0)
	c.NoCache = true
	updates, matched, err := listTagRenameUpdates(c, "old", "new", 2)
	if err != nil {
		t.Fatalf("listTagRenameUpdates: %v", err)
	}
	if matched != len(updates) {
		t.Errorf("matched = %d, want %d: every paged item carries the tag here", matched, len(updates))
	}

	wantRequests := []tagRenamePageRequest{{Start: 0, Limit: 2}, {Start: 2, Limit: 2}, {Start: 4, Limit: 2}}
	if len(requests) != len(wantRequests) {
		t.Fatalf("requests = %+v, want %+v", requests, wantRequests)
	}
	for i := range wantRequests {
		if requests[i] != wantRequests[i] {
			t.Fatalf("request %d = %+v, want %+v", i, requests[i], wantRequests[i])
		}
	}
	if len(updates) != len(items) {
		t.Fatalf("updates = %d, want %d", len(updates), len(items))
	}
	for i, update := range updates {
		if wantKey := "K" + strconv.Itoa(i); update.key != wantKey {
			t.Fatalf("update %d key = %q, want %q", i, update.key, wantKey)
		}
		if mutationExpectedVersion(update.version) == 0 {
			t.Fatalf("update %d recorded no read-plane version: %#v", i, update.version)
		}
	}
}

func TestTagsRenamePreviewsWithoutPatching(t *testing.T) {
	for _, tc := range []struct {
		name       string
		flags      rootFlags
		wantReason string
	}{
		{name: "default", flags: rootFlags{maxChanges: -1}, wantReason: "default"},
		{name: "dry-run", flags: rootFlags{dryRun: true, maxChanges: -1}, wantReason: "dry_run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTagRenameCommandTestServer(t, []string{"K1", "K2"})

			env, stderr, err := writePlaneTestRunMutationCmd(t, newTagsRenameCmd, &tc.flags, "--from", "foo", "--to", "bar")
			if err != nil {
				t.Fatalf("tags rename preview: %v; stderr=%s", err, stderr)
			}
			if !env.OK || env.Mode != "preview" || env.PreviewReason != tc.wantReason || env.Result != nil || env.Plan.Summary.Planned != 2 {
				t.Fatalf("env = %+v, want preview plan with two changes", env)
			}
			if total := totalTagRenamePatchCount(srv); total != 0 {
				t.Fatalf("PATCH count = %d, want 0", total)
			}
		})
	}
}

func TestTagsRenameYesAppliesPatches(t *testing.T) {
	srv := newTagRenameCommandTestServer(t, []string{"K1", "K2"})

	env, stderr, err := writePlaneTestRunMutationCmd(t, newTagsRenameCmd, &rootFlags{yes: true, maxChanges: -1}, "--from", "foo", "--to", "bar")
	if err != nil {
		t.Fatalf("tags rename apply: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.Applied != 2 {
		t.Fatalf("env = %+v, want apply result with two applied items", env)
	}
	for _, key := range []string{"K1", "K2"} {
		if srv.patchCounts[key] != 1 {
			t.Fatalf("%s PATCH count = %d, want 1", key, srv.patchCounts[key])
		}
		if got := srv.patchHeaders[key]; got != strconv.Itoa(srv.versions[key]) {
			t.Errorf("%s If-Unmodified-Since-Version = %q, want %d", key, got, srv.versions[key])
		}
		if !patchBodyHasTag(srv.patchBodies[key], "bar") {
			t.Errorf("%s PATCH body = %+v, want renamed tag bar", key, srv.patchBodies[key])
		}
		if patchBodyHasTag(srv.patchBodies[key], "foo") {
			t.Errorf("%s PATCH body = %+v, still contains old tag foo", key, srv.patchBodies[key])
		}
	}
}

func TestTagsRenameMaxChangesRefusesBeforePatching(t *testing.T) {
	srv := newTagRenameCommandTestServer(t, []string{"K1", "K2"})

	env, _, err := writePlaneTestRunMutationCmd(t, newTagsRenameCmd, &rootFlags{yes: true, maxChanges: 1}, "--from", "foo", "--to", "bar")
	if err == nil {
		t.Fatal("tags rename apply succeeded, want max_changes_exceeded error")
	}
	if env.OK || env.Error == nil || env.Error.Code != "max_changes_exceeded" {
		t.Fatalf("env = %+v, want max_changes_exceeded", env)
	}
	if total := totalTagRenamePatchCount(srv); total != 0 {
		t.Fatalf("PATCH count = %d, want 0", total)
	}
}

func TestRenamedItemTagsDeduplicatesByExactName(t *testing.T) {
	type tagSpec struct {
		tag    string
		typeID int
	}
	for _, tc := range []struct {
		name   string
		oldTag string
		newTag string
		input  []tagSpec
		want   []tagSpec
	}{
		{
			name:   "renames without changing count",
			oldTag: "foo",
			newTag: "baz",
			input:  []tagSpec{{"foo", 0}, {"bar", 1}},
			want:   []tagSpec{{"baz", 0}, {"bar", 1}},
		},
		{
			name:   "renamed tag collapses into existing exact name",
			oldTag: "foo",
			newTag: "Foo",
			input:  []tagSpec{{"foo", 0}, {"Foo", 1}},
			want:   []tagSpec{{"Foo", 0}},
		},
		{
			name:   "preexisting duplicate keeps first tag",
			oldTag: "foo",
			newTag: "bar",
			input:  []tagSpec{{"a", 0}, {"b", 1}, {"a", 2}},
			want:   []tagSpec{{"a", 0}, {"b", 1}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tags := make([]any, 0, len(tc.input))
			for _, tag := range tc.input {
				tags = append(tags, map[string]any{"tag": tag.tag, "type": tag.typeID})
			}
			item := map[string]any{"data": map[string]any{"tags": tags}}

			got, _, _, err := renamedItemTags(item, tc.oldTag, tc.newTag)
			if err != nil {
				t.Fatalf("renamedItemTags: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("tag count = %d, want %d", len(got), len(tc.want))
			}
			for i, want := range tc.want {
				tagObj, ok := got[i].(map[string]any)
				if !ok {
					t.Fatalf("tag %d = %#v, want tag object", i, got[i])
				}
				if actual := tagObj["tag"]; actual != want.tag {
					t.Errorf("tag %d name = %q, want %q", i, actual, want.tag)
				}
				if actual := tagObj["type"]; actual != want.typeID {
					t.Errorf("tag %d type = %v, want %d", i, actual, want.typeID)
				}
			}
		})
	}
}

// TestBuildTagRenameOpsEmitsInvertibleTagsChange guards the mirror-corruption
// and undo-refusal regression: buildTagRenameOps used to emit the singular
// field name "tag", which bypassed applyChangeToItemData's tags-membership
// branch (corrupting the local mirror with a bogus "tag" key) and was refused
// by mutation.InvertChange (breaking the documented `journal undo` promise
// for tag renames). It must emit plural "tags" with the alias in Remove and
// the canonical name in Add, carrying the per-item tag type through so a
// rename never flips manual <-> automatic.
func TestBuildTagRenameOpsEmitsInvertibleTagsChange(t *testing.T) {
	updates := []tagRenameUpdate{
		{key: "K1", version: 5, tagType: 0},
		{key: "K2", version: 7, tagType: 1},
	}
	ops := buildTagRenameOps(updates, "foo", "bar", func(tagRenameUpdate) (string, any, error) {
		return "applied", nil, nil
	})
	if len(ops) != 2 {
		t.Fatalf("ops = %d, want 2", len(ops))
	}
	for i, op := range ops {
		if len(op.Changes) != 1 {
			t.Fatalf("op %d changes = %+v, want exactly one", i, op.Changes)
		}
		change := op.Changes[0]
		if change.Field != "tags" {
			t.Errorf("op %d field = %q, want %q", i, change.Field, "tags")
		}
		if change.Remove != "foo" || change.Add != "bar" {
			t.Errorf("op %d change = %+v, want Remove=foo Add=bar", i, change)
		}
		if change.TagType != updates[i].tagType {
			t.Errorf("op %d TagType = %d, want %d", i, change.TagType, updates[i].tagType)
		}

		inv, ok := mutation.InvertChange(change)
		if !ok {
			t.Fatalf("op %d change %+v not reversible, want journal undo to reverse tag renames", i, change)
		}
		if inv.Field != "tags" || inv.Remove != "bar" || inv.Add != "foo" || inv.TagType != change.TagType {
			t.Errorf("op %d inverse = %+v, want reverse rename Remove=bar Add=foo", i, inv)
		}
	}
}

// TestTagRenameChangeReplaysOntoMirroredItemTags proves the write-through
// mirror replays a tag rename correctly: the stale alias disappears, the
// canonical name appears in its place, unrelated tags are untouched, and the
// manual(0)/automatic(1) type carried on the Change is preserved rather than
// silently flipped to manual. Modeled on the direct applyChangeToItemData
// calls in write_through_test.go.
func TestTagRenameChangeReplaysOntoMirroredItemTags(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tagType int
	}{
		{name: "manual alias", tagType: 0},
		{name: "automatic alias", tagType: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string]any{
				"tags": []any{
					map[string]any{"tag": "old", "type": tc.tagType},
					map[string]any{"tag": "untouched", "type": 0},
				},
			}
			change := mutation.Change{Field: "tags", Remove: "old", Add: "new", TagType: tc.tagType}
			if !applyChangeToItemData(data, change) {
				t.Fatal("tag rename change refused by write-through replay")
			}

			tags, _ := data["tags"].([]any)
			var sawOld, sawUntouched bool
			var newType any
			newFound := false
			for _, raw := range tags {
				tag, ok := raw.(map[string]any)
				if !ok {
					t.Fatalf("tag entry = %#v, want object", raw)
				}
				switch tag["tag"] {
				case "old":
					sawOld = true
				case "new":
					newFound = true
					newType = tag["type"]
				case "untouched":
					sawUntouched = true
				}
			}
			if sawOld {
				t.Errorf("mirrored tags = %+v, stale alias %q still present", tags, "old")
			}
			if !newFound {
				t.Fatalf("mirrored tags = %+v, renamed tag %q missing", tags, "new")
			}
			if !sawUntouched {
				t.Errorf("mirrored tags = %+v, unrelated tag dropped", tags)
			}
			var wantType any
			if tc.tagType != 0 {
				wantType = tc.tagType
			}
			if newType != wantType {
				t.Errorf("renamed tag type = %v, want %v (rename must not flip manual/automatic)", newType, wantType)
			}
		})
	}
}
