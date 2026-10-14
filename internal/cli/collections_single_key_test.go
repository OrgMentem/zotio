// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
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

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

// collectionsSingleKeyCounter answers the version GET these commands need and
// counts every HTTP request, so a refused argument list can prove that no
// network call was attempted rather than merely proving no mutation was sent.
type collectionsSingleKeyCounter struct {
	server   *httptest.Server
	requests int
	writes   int
}

func newCollectionsSingleKeyCounter(t *testing.T) *collectionsSingleKeyCounter {
	t.Helper()
	ctr := &collectionsSingleKeyCounter{}
	ctr.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctr.requests++
		if r.Method != http.MethodGet {
			ctr.writes++
		}
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = w.Write([]byte(`{"key":"ABCD2345","version":7,"data":{"key":"ABCD2345","name":"before"}}`))
	}))
	t.Cleanup(ctr.server.Close)
	return ctr
}

func executeCollectionsSingleKeyCmd(t *testing.T, ctr *collectionsSingleKeyCounter, cmd *cobra.Command, args ...string) (string, error) {
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

// A single-target command must not silently do less than it was asked. Cobra
// defaults a command with no subcommands to ArbitraryArgs, so each of these
// acted on K1 and dropped K2 with no mention of it: the request path, the op
// id and the whole envelope are built from args[0] alone. The operator sees
// one applied change and reads it as two.
func TestCollectionsSingleKeyCommandsRefuseMoreThanOneKey(t *testing.T) {
	wantPreview := func(operation string, checkChange func(*testing.T, any)) func(*testing.T, string) {
		return func(t *testing.T, out string) {
			t.Helper()
			var env mutation.Envelope
			if err := json.Unmarshal([]byte(out), &env); err != nil {
				t.Fatalf("decode single-key preview %q: %v", out, err)
			}
			if env.Operation != operation || !env.OK || env.Mode != "preview" || env.Result != nil {
				t.Errorf("preview envelope = %+v, want an ok %s preview with no result: the single-key path must stay a preview", env, operation)
			}
			if len(env.Plan.Operations) != 1 || env.Plan.Operations[0].Key != "ABCD2345" {
				t.Errorf("plan = %+v, want exactly one operation on ABCD2345", env.Plan)
			}
			if strings.Contains(out, "EFGH6789") {
				t.Errorf("preview output %q mentions EFGH6789, want only the single requested key", out)
			}
			if checkChange != nil && len(env.Plan.Operations) == 1 && len(env.Plan.Operations[0].Changes) == 1 {
				checkChange(t, env.Plan.Operations[0].Changes[0].Add)
			}
		}
	}
	cases := []struct {
		name            string
		newCmd          func() *cobra.Command
		twoKeys, oneKey []string
		checkPreview    func(t *testing.T, out string)
	}{
		{
			name:    "update",
			newCmd:  func() *cobra.Command { return newCollectionsUpdateCmd(&rootFlags{asJSON: true, maxChanges: -1}) },
			twoKeys: []string{"ABCD2345", "EFGH6789", "--name", "Renamed"},
			oneKey:  []string{"ABCD2345", "--name", "Renamed"},
			checkPreview: wantPreview("collections.update", func(t *testing.T, add any) {
				t.Helper()
				body, ok := add.(map[string]any)
				if !ok || body["name"] != "Renamed" {
					t.Errorf("planned body = %v, want {name: Renamed}", add)
				}
			}),
		},
		{
			// Destructive: `collections delete K1 K2` deleted only K1.
			name: "delete",
			newCmd: func() *cobra.Command {
				return newCollectionsDeleteCmd(&rootFlags{asJSON: true, maxChanges: -1, allowDestructive: true})
			},
			twoKeys:      []string{"ABCD2345", "EFGH6789"},
			oneKey:       []string{"ABCD2345"},
			checkPreview: wantPreview("collections.delete", nil),
		},
		{
			name:    "move",
			newCmd:  func() *cobra.Command { return newCollectionsMoveCmd(&rootFlags{asJSON: true, maxChanges: -1}) },
			twoKeys: []string{"--to", "PRNTAB23", "ABCD2345", "EFGH6789"},
			oneKey:  []string{"--to", "PRNTAB23", "ABCD2345"},
			checkPreview: wantPreview("collections.move", func(t *testing.T, add any) {
				t.Helper()
				body, ok := add.(map[string]any)
				if !ok || body["parentCollection"] != "PRNTAB23" {
					t.Errorf("planned body = %v, want parentCollection PRNTAB23", add)
				}
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctr := newCollectionsSingleKeyCounter(t)
			out, err := executeCollectionsSingleKeyCmd(t, ctr, tc.newCmd(), tc.twoKeys...)
			if err == nil {
				t.Fatalf("collections %s accepted two keys (out=%q); it acts on the first and drops the rest without saying so", tc.name, out)
			}
			if !strings.Contains(err.Error(), "accepts 1 arg(s)") {
				t.Errorf("error = %q, want the arity bound the neighbouring commands report", err.Error())
			}
			if ctr.requests != 0 {
				t.Errorf("requests = %d, want 0: a refused argument list must not reach the library", ctr.requests)
			}

			// The single-key path still previews unchanged.
			singleCtr := newCollectionsSingleKeyCounter(t)
			singleOut, err := executeCollectionsSingleKeyCmd(t, singleCtr, tc.newCmd(), tc.oneKey...)
			if err != nil {
				t.Fatalf("collections %s with one key = %v, want the preview", tc.name, err)
			}
			tc.checkPreview(t, singleOut)
			// The preview runs the same reads as apply, but never writes.
			if singleCtr.requests == 0 || singleCtr.writes != 0 {
				t.Errorf("requests = %d, writes = %d, want reads only from the preview", singleCtr.requests, singleCtr.writes)
			}
		})
	}
}
