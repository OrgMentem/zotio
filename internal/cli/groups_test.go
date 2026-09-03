// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
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
	saved := activeGroupIDLocked()
	defer func() { setActiveGroupID(saved) }()

	setActiveGroupID("")
	if got := helpersTestDefaultDBPath(t, "zotio"); !strings.HasSuffix(got, "data.db") || strings.Contains(got, "data-group") {
		t.Errorf("personal defaultDBPath = %q, want .../data.db", got)
	}

	setActiveGroupID("12345")
	if got := helpersTestDefaultDBPath(t, "zotio"); !strings.HasSuffix(got, "data-group-12345.db") {
		t.Errorf("group defaultDBPath = %q, want .../data-group-12345.db", got)
	}
}

func TestDefaultDBPathUsesDataDirOverride(t *testing.T) {
	saved := activeGroupIDLocked()
	defer func() { setActiveGroupID(saved) }()

	dataDir := t.TempDir()
	t.Setenv("ZOTERO_DATA_DIR", dataDir)
	t.Setenv("ZOTERO_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")

	setActiveGroupID("")
	if got, want := helpersTestDefaultDBPath(t, "zotio"), filepath.Join(dataDir, "data.db"); got != want {
		t.Fatalf("personal defaultDBPath = %q, want %q", got, want)
	}

	setActiveGroupID("12345")
	if got, want := helpersTestDefaultDBPath(t, "zotio"), filepath.Join(dataDir, "data-group-12345.db"); got != want {
		t.Fatalf("group defaultDBPath = %q, want %q", got, want)
	}
}

func TestGroupsList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/0/groups" {
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `[{"id":99,"version":1,"data":{"name":"Lab","type":"Private"},"meta":{"numItems":7}}]`)
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
	env := decodeResultsArrayEnvelope(t, jsonBuf.Bytes())
	if len(env.Results) != 1 {
		t.Fatalf("got %d groups, want 1", len(env.Results))
	}
	if groupFieldString(env.Results[0], "name") != "Lab" {
		t.Errorf("group name = %q, want Lab", groupFieldString(env.Results[0], "name"))
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

func TestGroupsInspect_JSONReadiness(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = oldAllowPrivateOutbound })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/groups"):
			_, _ = io.WriteString(w, `[{"id":12345,"data":{"name":"Lab","type":"PrivateGroup","libraryReading":"all","libraryEditing":"members"},"meta":{"numItems":10}}]`)
		case r.URL.Path == "/keys/current":
			_, _ = io.WriteString(w, `{"userID":0,"access":{"groups":{"12345":{"library":true,"write":true}}}}`)
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer srv.Close()
	oldBase := zoteroWebAPIBase
	zoteroWebAPIBase = srv.URL
	defer func() { zoteroWebAPIBase = oldBase }()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_API_KEY", "testkey")
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
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when listing groups from a group base URL")
	}
	if code := ExitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2 (usage); err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "to list groups") {
		t.Errorf("error = %q, want the refusal to name the caller's purpose", err.Error())
	}
}

// groups inspect enumerates through the same shared helper as groups list, so
// it must refuse a group-scoped base URL the same way — with its own purpose
// in the message, not the other command's.
func TestGroupsInspect_RejectsGroupBaseURL(t *testing.T) {
	t.Setenv("ZOTERO_BASE_URL", "http://localhost:23119/api/groups/12345")
	cmd := newGroupsInspectCmd(&rootFlags{asJSON: true})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"12345"})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when inspecting groups from a group base URL")
	}
	if code := ExitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2 (usage); err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "to inspect groups") {
		t.Errorf("error = %q, want the refusal to name the caller's purpose", err.Error())
	}
}

// fetchAccessibleGroups is the one enumeration both groups commands and the
// --group all fan-out share; it must hit the personal-library prefix exactly
// once per call, whatever the caller.
func TestFetchAccessibleGroups_UsesThePersonalLibraryPrefix(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = io.WriteString(w, `[{"id":99,"data":{"name":"Lab","type":"Private"},"meta":{"numItems":7}}]`)
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	data, err := fetchAccessibleGroups(&rootFlags{}, "list groups")
	if err != nil {
		t.Fatalf("fetchAccessibleGroups: %v", err)
	}
	var groups []map[string]any
	if err := json.Unmarshal(data, &groups); err != nil {
		t.Fatalf("decoding %s: %v", data, err)
	}
	if len(groups) != 1 || groupFieldString(groups[0], "id") != "99" {
		t.Fatalf("groups = %s, want the single group 99", data)
	}
	if len(paths) != 1 || paths[0] != "/users/0/groups" {
		t.Fatalf("requests = %v, want one /users/0/groups", paths)
	}
}
