// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean 9bfn): cover group-library prefix rewriting, the group-aware DB
// path, and the groups list command.

package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteLibraryPrefix(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		groupID string
		want    string
	}{
		{"local user prefix", "http://localhost:23119/api/users/0", "12345", "http://localhost:23119/api/groups/12345"},
		{"web user prefix", "https://api.zotero.org/users/55", "12345", "https://api.zotero.org/groups/12345"},
		{"existing group prefix", "http://localhost:23119/api/groups/1", "2", "http://localhost:23119/api/groups/2"},
		{"no library segment", "http://localhost:23119/api", "7", "http://localhost:23119/api/groups/7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rewriteLibraryPrefix(c.baseURL, c.groupID); got != c.want {
				t.Errorf("rewriteLibraryPrefix(%q, %q) = %q, want %q", c.baseURL, c.groupID, got, c.want)
			}
		})
	}
}

func TestUserIDFromBaseURL(t *testing.T) {
	if id, ok := userIDFromBaseURL("http://localhost:23119/api/users/0"); !ok || id != "0" {
		t.Errorf("userIDFromBaseURL(local) = %q,%v, want 0,true", id, ok)
	}
	if id, ok := userIDFromBaseURL("https://api.zotero.org/users/55"); !ok || id != "55" {
		t.Errorf("userIDFromBaseURL(web) = %q,%v, want 55,true", id, ok)
	}
	if _, ok := userIDFromBaseURL("http://localhost:23119/api/groups/12345"); ok {
		t.Error("userIDFromBaseURL(group URL) = true, want false")
	}
}

func TestDefaultDBPath_GroupAware(t *testing.T) {
	saved := activeGroupID
	defer func() { activeGroupID = saved }()

	activeGroupID = ""
	if got := defaultDBPath("zotero-pp-cli"); !strings.HasSuffix(got, "data.db") || strings.Contains(got, "data-group") {
		t.Errorf("personal defaultDBPath = %q, want .../data.db", got)
	}

	activeGroupID = "12345"
	if got := defaultDBPath("zotero-pp-cli"); !strings.HasSuffix(got, "data-group-12345.db") {
		t.Errorf("group defaultDBPath = %q, want .../data-group-12345.db", got)
	}
}

func TestGroupsList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/0/groups" {
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
		io.WriteString(w, `[{"id":99,"version":1,"data":{"name":"Lab","type":"Private"},"meta":{"numItems":7}}]`)
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	// --json round-trips the group payload.
	jsonFlags := &rootFlags{asJSON: true}
	jsonCmd := newGroupsListCmd(jsonFlags)
	var jsonBuf bytes.Buffer
	jsonCmd.SetOut(&jsonBuf)
	jsonCmd.SetArgs(nil)
	if err := jsonCmd.Execute(); err != nil {
		t.Fatalf("groups list --json: %v", err)
	}
	var groups []map[string]any
	if err := json.Unmarshal(jsonBuf.Bytes(), &groups); err != nil {
		t.Fatalf("decoding json output %q: %v", jsonBuf.String(), err)
	}
	if len(groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(groups))
	}
	if groupFieldString(groups[0], "name") != "Lab" {
		t.Errorf("group name = %q, want Lab", groupFieldString(groups[0], "name"))
	}

	// Table output renders the flattened columns.
	tableFlags := &rootFlags{}
	tableCmd := newGroupsListCmd(tableFlags)
	var tableBuf bytes.Buffer
	tableCmd.SetOut(&tableBuf)
	tableCmd.SetArgs(nil)
	if err := tableCmd.Execute(); err != nil {
		t.Fatalf("groups list: %v", err)
	}
	out := tableBuf.String()
	for _, want := range []string{"99", "Lab", "Private", "7"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q: %s", want, out)
		}
	}
}

// PATCH(glean roadmap-phase7 groups-inspect): cover JSON readiness preflight for accessible and missing groups.
func TestGroupsInspect_JSONReadiness(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/0/groups" {
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
		io.WriteString(w, `[{"id":12345,"data":{"name":"Lab","type":"PrivateGroup","libraryReading":"all","libraryEditing":"members"},"meta":{"numItems":10}}]`)
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	runInspect := func(groupID string) map[string]any {
		t.Helper()
		cmd := newGroupsCmd(&rootFlags{asJSON: true})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"inspect", groupID})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("groups inspect %s --json: %v", groupID, err)
		}
		var report map[string]any
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("decoding inspect output %q: %v", out.String(), err)
		}
		return report
	}

	found := runInspect("12345")
	if found["found"] != true {
		t.Errorf("found = %v, want true", found["found"])
	}
	if found["name"] != "Lab" {
		t.Errorf("name = %v, want Lab", found["name"])
	}
	if found["num_items"] != "10" {
		t.Errorf("num_items = %v, want 10", found["num_items"])
	}
	if found["ready_for_write"] != true {
		t.Errorf("ready_for_write = %v, want true", found["ready_for_write"])
	}

	missing := runInspect("99999")
	if missing["found"] != false {
		t.Errorf("missing found = %v, want false", missing["found"])
	}
}

func TestGroupsList_RejectsGroupBaseURL(t *testing.T) {
	t.Setenv("ZOTERO_BASE_URL", "http://localhost:23119/api/groups/12345")
	cmd := newGroupsListCmd(&rootFlags{asJSON: true})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs(nil)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when listing groups from a group base URL")
	}
}
