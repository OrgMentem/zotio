// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// path, and the groups list command.

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
	oldAllowPrivateOutbound := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllowPrivateOutbound) })

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
	if found["num_items"] != float64(10) {
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

// groups inspect enumerates through the same shared helper as groups list, so
// it must refuse a group-scoped base URL the same way — with its own purpose
// in the message, not the other command's.
func TestGroupsRejectsGroupBaseURL(t *testing.T) {
	tests := []struct {
		name      string
		newCmd    func(*rootFlags) *cobra.Command
		args      []string
		purpose   string
		wantError string
	}{
		{
			name:      "list",
			newCmd:    newGroupsListCmd,
			args:      nil,
			purpose:   "to list groups",
			wantError: "expected error when listing groups from a group base URL",
		},
		{
			name:      "inspect",
			newCmd:    newGroupsInspectCmd,
			args:      []string{"12345"},
			purpose:   "to inspect groups",
			wantError: "expected error when inspecting groups from a group base URL",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ZOTERO_BASE_URL", "http://localhost:23119/api/groups/12345")
			cmd := tc.newCmd(&rootFlags{asJSON: true})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetArgs(tc.args)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			err := cmd.Execute()
			if err == nil {
				t.Fatal(tc.wantError)
			}
			if code := ExitCode(err); code != 2 {
				t.Fatalf("exit code = %d, want 2 (usage); err=%v", code, err)
			}
			if !strings.Contains(err.Error(), tc.purpose) {
				t.Errorf("error = %q, want the refusal to name the caller's purpose", err.Error())
			}
		})
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

// serveGroupPages answers /users/1/groups from a fixed group list, honouring
// limit/start and sending Total-Results, so a listing that stops at the first
// page is visible. headerTotal overrides the advertised total when >= 0.
func serveGroupPages(t *testing.T, groups []string, headerTotal int, pageCap int, requests *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/groups") {
			http.NotFound(w, r)
			return
		}
		*requests = append(*requests, r.URL.RawQuery)
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		limit := pageCap
		if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l < limit {
			limit = l
		}
		if start > len(groups) {
			start = len(groups)
		}
		end := start + limit
		if end > len(groups) {
			end = len(groups)
		}
		total := len(groups)
		if headerTotal >= 0 {
			total = headerTotal
		}
		w.Header().Set("Total-Results", strconv.Itoa(total))
		_, _ = io.WriteString(w, "["+strings.Join(groups[start:end], ",")+"]")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func manyGroups(n int) []string {
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf(`{"id":%d,"data":{"name":"G%d","type":"Private"},"meta":{"numItems":1}}`, 1000+i, i))
	}
	return out
}

// A Web API account in more than one page of groups must be enumerated to
// completion: --group all and groups list drop every group past the first page
// otherwise, and report no failure.
func TestFetchAccessibleGroups_WalksEveryPage(t *testing.T) {
	var requests []string
	srv := serveGroupPages(t, manyGroups(130), -1, 100, &requests)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/1")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	libs, err := resolveFanoutLibraries(&rootFlags{})
	if err != nil {
		t.Fatalf("resolveFanoutLibraries: %v", err)
	}
	if len(libs) != 131 {
		t.Fatalf("libraries = %d, want 131 (personal + 130 groups); requests=%v", len(libs), requests)
	}
	if last := libs[len(libs)-1]; last.ID != "1130" {
		t.Errorf("last library = %+v, want group 1130", last)
	}
}

// A page that falls short of the advertised Total-Results must fail the run:
// fanning out over a partial group set looks like a smaller account.
func TestFetchAccessibleGroups_RefusesShortListing(t *testing.T) {
	var requests []string
	srv := serveGroupPages(t, manyGroups(30), 45, 100, &requests)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/1")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	_, err := resolveFanoutLibraries(&rootFlags{})
	if err == nil {
		t.Fatal("resolveFanoutLibraries succeeded over a listing shorter than Total-Results")
	}
	if !strings.Contains(err.Error(), "Total-Results") {
		t.Errorf("error = %v, want it to name Total-Results", err)
	}
}

// A server that ignores start would repeat page one forever.
func TestFetchAccessibleGroups_RefusesIgnoredStart(t *testing.T) {
	groups := manyGroups(100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "["+strings.Join(groups, ",")+"]")
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/1")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	if _, err := fetchAccessibleGroups(&rootFlags{}, "list groups"); err == nil {
		t.Fatal("fetchAccessibleGroups accepted a listing that ignores start")
	}
}

func TestGroupsInspect_NonNumericIDAndQuietExit(t *testing.T) {
	oldAllow := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllow) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/groups"):
			_, _ = io.WriteString(w, `[{"id":12345,"data":{"name":"Lab","type":"PrivateGroup"},"meta":{"numItems":10}}]`)
		case r.URL.Path == "/keys/current":
			_, _ = io.WriteString(w, `{"userID":0,"access":{"groups":{"12345":{"library":true,"write":false}}}}`)
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

	run := func(flags *rootFlags, id string) (string, error) {
		cmd := newGroupsCmd(flags)
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"inspect", id})
		err := cmd.Execute()
		return out.String(), err
	}
	exitCode := func(err error) int {
		var ce *cliError
		if errors.As(err, &ce) {
			return ce.code
		}
		return -1
	}

	if _, err := run(&rootFlags{}, "my-lab"); exitCode(err) != 2 || !strings.Contains(err.Error(), "zotio groups list") {
		t.Errorf("non-numeric ID err = %v, want usage error naming `zotio groups list`", err)
	}
	out, err := run(&rootFlags{}, "12345")
	if err != nil || !strings.Contains(out, "write=false") || !strings.Contains(out, "note:") {
		t.Errorf("human output = %q, err = %v; want write=false and a note line", out, err)
	}
	if out, err := run(&rootFlags{quiet: true}, "99999"); exitCode(err) != 3 || out != "" {
		t.Errorf("quiet not found: out=%q err=%v, want exit 3", out, err)
	}
	if out, err := run(&rootFlags{quiet: true}, "12345"); exitCode(err) != 9 || out != "" {
		t.Errorf("quiet not writable: out=%q err=%v, want exit 9", out, err)
	}
}
