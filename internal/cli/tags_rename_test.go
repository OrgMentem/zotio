// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
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
	t.Cleanup(ts.server.Close)
	return ts
}

func runTagsRenameTestCmd(t *testing.T, srv *tagRenameCommandTestServer, flags *rootFlags, args ...string) (mutation.Envelope, string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newTagsRenameCmd(flags)
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
			t.Fatalf("tag query = %q, want old", got)
		}
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil {
			t.Fatalf("limit query = %q: %v", r.URL.Query().Get("limit"), err)
		}
		start, err := strconv.Atoi(r.URL.Query().Get("start"))
		if err != nil {
			t.Fatalf("start query = %q: %v", r.URL.Query().Get("start"), err)
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
	updates, err := listTagRenameUpdates(c, "old", "new", 2)
	if err != nil {
		t.Fatalf("listTagRenameUpdates: %v", err)
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
		raw, err := json.Marshal(update.tags)
		if err != nil {
			t.Fatalf("marshal tags: %v", err)
		}
		if string(raw) != `[{"tag":"new","type":0}]` {
			t.Fatalf("update %d tags = %s, want renamed tag only", i, raw)
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

			env, stderr, err := runTagsRenameTestCmd(t, srv, &tc.flags, "--from", "foo", "--to", "bar")
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

	env, stderr, err := runTagsRenameTestCmd(t, srv, &rootFlags{yes: true, maxChanges: -1}, "--from", "foo", "--to", "bar")
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

	env, _, err := runTagsRenameTestCmd(t, srv, &rootFlags{yes: true, maxChanges: 1}, "--from", "foo", "--to", "bar")
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
