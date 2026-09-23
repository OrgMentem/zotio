// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	flags := &rootFlags{asJSON: true, dataSource: source, noCache: true, timeout: time.Second}
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
		{"items list json", []string{"items list", "--format", "json"}},
		{"items list csljson", []string{"items list", "--format", "csljson"}},
		{"items list versions", []string{"items list", "--format", "versions"}},
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
