// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// readSingleKeyCounter answers the version/item GET these read commands need
// and counts every HTTP request, so a refused argument list can prove that no
// network call was attempted rather than merely proving no data was returned.
type readSingleKeyCounter struct {
	server   *httptest.Server
	requests int
}

func newReadSingleKeyCounter(t *testing.T) *readSingleKeyCounter {
	t.Helper()
	ctr := &readSingleKeyCounter{}
	ctr.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctr.requests++
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = w.Write([]byte(`{"key":"K1","version":7,"data":{"key":"K1","itemType":"book","title":"T"}}`))
	}))
	t.Cleanup(ctr.server.Close)
	return ctr
}

func executeReadSingleKeyCmd(t *testing.T, ctr *readSingleKeyCounter, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", ctr.server.URL+"/users/0")
	t.Setenv("ZOTERO_API_KEY", "test-key")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// A single-target read must not silently do less than it was asked. Cobra
// defaults a command with no subcommands to ArbitraryArgs, so `items get K1
// K2` read K1 and dropped K2 with no mention of it: the request path is built
// from args[0] alone, and the operator reads one result as two.
func TestSingleKeyReadsRefuseMoreThanOneKey(t *testing.T) {
	for _, tt := range []struct {
		name     string
		new      func(*rootFlags) *cobra.Command
		refuse   []string
		noun     string
		helpNoun string
	}{
		{name: "items get", new: newItemsGetCmd, refuse: []string{"K1", "K2"}, noun: "keys", helpNoun: "key"},
		{name: "collections get", new: newCollectionsGetCmd, refuse: []string{"K1", "K2"}, noun: "keys", helpNoun: "key"},
		{name: "searches get", new: newSearchesGetCmd, refuse: []string{"K1", "K2"}, noun: "keys", helpNoun: "key"},
		{name: "tags get", new: newTagsGetCmd, refuse: []string{"A", "B"}, noun: "names", helpNoun: "name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctr := newReadSingleKeyCounter(t)

			out, err := executeReadSingleKeyCmd(t, ctr, tt.new(&rootFlags{asJSON: true, dataSource: "live", noCache: true}), tt.refuse...)
			if err == nil {
				t.Fatalf("%s accepted two %s (out=%q); it reads the first and drops the rest without saying so", tt.name, tt.noun, out)
			}
			if ctr.requests != 0 {
				t.Errorf("requests = %d, want 0: a refused argument list must not reach the library", ctr.requests)
			}

			// Zero args still renders help rather than erroring, which is why the
			// bound is MaximumNArgs and not ExactArgs.
			helpCtr := newReadSingleKeyCounter(t)
			if _, err := executeReadSingleKeyCmd(t, helpCtr, tt.new(&rootFlags{asJSON: true, dataSource: "live", noCache: true})); err != nil {
				t.Errorf("%s with no %s = %v, want the help output", tt.name, tt.helpNoun, err)
			}
		})
	}
}

// `items open K1 K2` built the deep link from args[0] alone and dropped K2
// with no mention of it. A refusal must come before any desktop launch, so
// this drives the plain-print path (no --launch).
func TestItemsOpenRefusesMoreThanOneKey(t *testing.T) {
	cmd := newItemsOpenCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"K1", "K2"})
	if err := cmd.Execute(); err == nil {
		t.Fatalf("items open accepted two keys (out=%q); it links the first and drops the rest without saying so", out.String())
	}
	if got := out.String(); got != "" {
		t.Errorf("stdout = %q, want nothing: the refused keys must not produce a deep link", got)
	}

	// Zero args still renders help rather than erroring, which is why the
	// bound is MaximumNArgs and not ExactArgs.
	help := newItemsOpenCmd(&rootFlags{})
	help.SilenceErrors, help.SilenceUsage = true, true
	help.SetOut(&bytes.Buffer{})
	help.SetErr(io.Discard)
	if err := help.Execute(); err != nil {
		t.Errorf("items open with no key = %v, want the help output", err)
	}

	// The single-key path still prints the deep link unchanged.
	stdout, _, err := executeItemsOpen("ABC123")
	if err != nil {
		t.Fatalf("items open with one key = %v, want the deep link", err)
	}
	if stdout != "zotero://select/library/items/ABC123\n" {
		t.Errorf("stdout = %q, want the Zotero URI for the one key", stdout)
	}
}
