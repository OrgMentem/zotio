// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func runMalformedBodyCommand(t *testing.T, source, body string, args ...string) (string, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/api/users/0")

	flags := &rootFlags{asJSON: true, dataSource: source, noCache: true, timeout: 10 * time.Second}
	return executeMalformedBodyCommand(t, flags, args...)
}

func executeMalformedBodyCommand(t *testing.T, flags *rootFlags, args ...string) (string, error) {
	t.Helper()
	var cmd *cobra.Command
	switch args[0] {
	case "items list":
		cmd = newItemsListCmd(flags)
	case "items get":
		cmd = newItemsGetCmd(flags)
	case "collections items":
		cmd = newCollectionsItemsCmd(flags)
	case "collections export":
		cmd = newCollectionsExportCmd(flags)
	case "items trash":
		cmd = newItemsTrashCmd(flags)
	case "groups list":
		cmd = newGroupsListCmd(flags)
	}
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args[1:])
	err := cmd.Execute()
	return out.String(), err
}

func TestMalformedJSONBodyFailsLiveRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"items list default", []string{"items list"}},
		{"items get", []string{"items get", "A"}},
		{"collections items", []string{"collections items", "COL"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runMalformedBodyCommand(t, "live", `[{"key":"A"`, tc.args...)
			if err == nil || ExitCode(err) != 5 {
				t.Fatalf("error = %v, exit = %d, want API exit 5", err, ExitCode(err))
			}
			if got := err.Error(); got != "response body is not valid JSON" {
				t.Fatalf("error = %q, want a fixed error without the response body", got)
			}
			if out != "" {
				t.Fatalf("output = %q, want no results envelope", out)
			}
		})
	}
}

func TestMalformedJSONBodyPreservesTextFormat(t *testing.T) {
	text := "@article{a, title={x}}"
	out, err := runMalformedBodyCommand(t, "live", text, "items list", "--format", "bibtex")
	if err != nil {
		t.Fatalf("items list bibtex error: %v", err)
	}
	var envelope struct {
		Results []string `json:"results"`
		Meta    struct {
			Source string `json:"source"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("decode envelope %q: %v", out, err)
	}
	if len(envelope.Results) != 1 || envelope.Results[0] != text || envelope.Meta.Source != "live" {
		t.Fatalf("envelope = %+v, want one text result from live", envelope)
	}
}

func TestMalformedJSONBodyDoesNotServeMirror(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	out, err := runMalformedBodyCommand(t, "auto", `[{"key":"A"`, "items list")
	if err == nil || ExitCode(err) != 5 || err.Error() != "response body is not valid JSON" {
		t.Fatalf("auto read error = %v, exit = %d, want malformed JSON API error", err, ExitCode(err))
	}
	if out != "" {
		t.Fatalf("auto read output = %q, want no mirror rows or results envelope", out)
	}
}

func TestMalformedJSONBodyFailsCollectionExport(t *testing.T) {
	out, err := runMalformedBodyCommand(t, "live", `[{"key":"A"`, "collections export", "COL", "--format", "json", "--flat")
	if err == nil || ExitCode(err) != 5 {
		t.Fatalf("collection export error = %v, exit = %d, want API exit 5", err, ExitCode(err))
	}
	if !strings.Contains(err.Error(), "response body is not valid JSON") || strings.Contains(err.Error(), `[{"key":"A"`) {
		t.Fatalf("collection export error = %q, want safe malformed JSON error", err)
	}
	if out != "" {
		t.Fatalf("collection export output = %q, want no partial export", out)
	}
}

func TestMalformedJSONBodyCacheEnabledRecovers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			_, _ = w.Write([]byte(`[{"key":"BROKEN"`))
			return
		}
		_, _ = w.Write([]byte(`[{"key":"RECOVERED","data":{"key":"RECOVERED","itemType":"book"}}]`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/api/users/0")

	flags := &rootFlags{asJSON: true, dataSource: "live", timeout: 10 * time.Second}
	out, err := executeMalformedBodyCommand(t, flags, "items list")
	if out != "" || err == nil || ExitCode(err) != 5 || err.Error() != "response body is not valid JSON" {
		t.Fatalf("first read: output = %q, error = %v (exit %d)", out, err, ExitCode(err))
	}
	out, err = executeMalformedBodyCommand(t, flags, "items list")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	var envelope struct {
		Results []struct {
			Key string `json:"key"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("decode recovered output %q: %v", out, err)
	}
	if len(envelope.Results) != 1 || envelope.Results[0].Key != "RECOVERED" || requests != 2 {
		t.Fatalf("recovered results = %+v, server requests = %d; want RECOVERED from second request", envelope.Results, requests)
	}
}

func TestMalformedJSONBodyFailsTrashFirstPageWithoutMirrorFallback(t *testing.T) {
	flags, _ := seedLocalTrashDB(t, localTrashFixture[:1], true)
	flags.dataSource = "auto"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"key":"BROKEN"`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/api/users/0")
	out, err := executeMalformedBodyCommand(t, flags, "items trash")
	if out != "" || err == nil || ExitCode(err) != 5 || err.Error() != "response body is not valid JSON" {
		t.Fatalf("trash first page: output = %q, error = %v (exit %d); want API error without mirror rows", out, err, ExitCode(err))
	}
}

func TestMalformedJSONBodyFailsTrashLaterPage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	firstPage := make([]json.RawMessage, 100)
	for i := range firstPage {
		firstPage[i] = json.RawMessage(fmt.Sprintf(`{"key":"ITEM%d"}`, i))
	}
	validPage, err := json.Marshal(firstPage)
	if err != nil {
		t.Fatal(err)
	}
	var starts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		starts = append(starts, r.URL.Query().Get("start"))
		if r.URL.Query().Get("start") == "0" {
			_, _ = w.Write(validPage)
			return
		}
		_, _ = w.Write([]byte(`[{"key":"BROKEN"`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/api/users/0")
	out, err := executeMalformedBodyCommand(t, &rootFlags{asJSON: true, dataSource: "live", noCache: true, timeout: 10 * time.Second}, "items trash")
	if out != "" || err == nil || ExitCode(err) != 5 || err.Error() != "response body is not valid JSON" {
		t.Fatalf("trash later page: output = %q, error = %v (exit %d); want no truncated list", out, err, ExitCode(err))
	}
	if len(starts) != 2 || starts[0] != "0" || starts[1] != "100" {
		t.Fatalf("requested starts = %v, want pages 0 and 100", starts)
	}
}

func TestMalformedJSONBodyFailsGroupsList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":99`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	out, err := executeMalformedBodyCommand(t, &rootFlags{asJSON: true, noCache: true, timeout: 10 * time.Second}, "groups list")
	if out != "" || err == nil || ExitCode(err) != 5 || err.Error() != "response body is not valid JSON" {
		t.Fatalf("groups list: output = %q, error = %v (exit %d)", out, err, ExitCode(err))
	}
}

func TestMalformedJSONBodyFailsFanoutEnumeration(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`[{"id":99`))
	}))
	defer srv.Close()
	isolateFanoutEnv(t, srv.URL+"/users/0")

	out, _, err := runFanoutCmd(t, "collections", "list", "--group", "all", "--json", "--data-source", "live")
	if out != "" || err == nil || ExitCode(err) != 5 || err.Error() != "response body is not valid JSON" {
		t.Fatalf("fan-out: output = %q, error = %v (exit %d); want enumeration failure", out, err, ExitCode(err))
	}
	if len(paths) != 1 || paths[0] != "/users/0/groups" {
		t.Fatalf("server paths = %v, want only the failed group enumeration", paths)
	}
}
