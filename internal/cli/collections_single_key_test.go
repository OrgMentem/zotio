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
)

// collectionsSingleKeyCounter answers the version GET these commands need and
// counts every HTTP request, so a refused argument list can prove that no
// network call was attempted rather than merely proving no mutation was sent.
type collectionsSingleKeyCounter struct {
	server   *httptest.Server
	requests int
}

func newCollectionsSingleKeyCounter(t *testing.T) *collectionsSingleKeyCounter {
	t.Helper()
	ctr := &collectionsSingleKeyCounter{}
	ctr.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctr.requests++
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = w.Write([]byte(`{"key":"K1","version":7,"data":{"key":"K1","name":"before"}}`))
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
	wantJSONPreview := func(action string, checkBody func(*testing.T, map[string]any)) func(*testing.T, string) {
		return func(t *testing.T, out string) {
			t.Helper()
			var decoded map[string]any
			if err := json.Unmarshal([]byte(out), &decoded); err != nil {
				t.Fatalf("decode single-key preview %q: %v", out, err)
			}
			if decoded["action"] != action || decoded["key"] != "K1" {
				t.Errorf("preview envelope = %v, want action %s for key K1", decoded, action)
			}
			if decoded["dry_run"] != true {
				t.Errorf("preview envelope = %v, want dry_run true: the single-key path must stay a preview", decoded)
			}
			if strings.Contains(out, "K2") {
				t.Errorf("preview output %q mentions K2, want only the single requested key", out)
			}
			if checkBody != nil {
				checkBody(t, decoded)
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
			newCmd:  func() *cobra.Command { return newCollectionsUpdateCmd(&rootFlags{asJSON: true}) },
			twoKeys: []string{"K1", "K2", "--name", "Renamed"},
			oneKey:  []string{"K1", "--name", "Renamed"},
			checkPreview: wantJSONPreview("update", func(t *testing.T, decoded map[string]any) {
				t.Helper()
				body, ok := decoded["body"].(map[string]any)
				if !ok || body["name"] != "Renamed" {
					t.Errorf("preview body = %v, want {name: Renamed}", decoded["body"])
				}
			}),
		},
		{
			// Destructive: `collections delete K1 K2` deleted only K1.
			name:    "delete",
			newCmd:  func() *cobra.Command { return newCollectionsDeleteCmd(&rootFlags{asJSON: true}) },
			twoKeys: []string{"K1", "K2"},
			oneKey:  []string{"K1"},
			checkPreview: wantJSONPreview("delete", func(t *testing.T, decoded map[string]any) {
				t.Helper()
				if _, ok := decoded["body"]; ok {
					t.Errorf("delete preview carries body = %v, want no body", decoded["body"])
				}
			}),
		},
		{
			name:    "move",
			newCmd:  func() *cobra.Command { return newCollectionsMoveCmd(&rootFlags{}) },
			twoKeys: []string{"--to", "PARENT", "K1", "K2"},
			oneKey:  []string{"--to", "PARENT", "COLL"},
			checkPreview: func(t *testing.T, out string) {
				t.Helper()
				if want := "Would move collection COLL under parent PARENT\n"; out != want {
					t.Errorf("stdout = %q, want %q", out, want)
				}
				if strings.Contains(out, "K2") {
					t.Errorf("move preview %q mentions K2, want only the single requested key", out)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctr := newCollectionsSingleKeyCounter(t)
			out, err := executeCollectionsSingleKeyCmd(t, ctr, tc.newCmd(), tc.twoKeys...)
			if err == nil {
				t.Fatalf("collections %s accepted two keys (out=%q); it acts on the first and drops the rest without saying so", tc.name, out)
			}
			if !strings.Contains(err.Error(), "accepts at most 1 arg(s)") {
				t.Errorf("error = %q, want the arity bound the neighbouring commands report", err.Error())
			}
			if ctr.requests != 0 {
				t.Errorf("requests = %d, want 0: a refused argument list must not reach the library", ctr.requests)
			}

			// Zero args still renders help rather than erroring, which is why
			// the bound is MaximumNArgs and not ExactArgs.
			helpCtr := newCollectionsSingleKeyCounter(t)
			helpOut, err := executeCollectionsSingleKeyCmd(t, helpCtr, tc.newCmd())
			if err != nil {
				t.Errorf("collections %s with no key = %v, want the help output", tc.name, err)
			}
			if !strings.Contains(helpOut, "Usage:") {
				t.Errorf("collections %s with no key printed %q, want usage help", tc.name, helpOut)
			}
			// The single-key path still previews unchanged and makes no HTTP call.
			singleCtr := newCollectionsSingleKeyCounter(t)
			singleOut, err := executeCollectionsSingleKeyCmd(t, singleCtr, tc.newCmd(), tc.oneKey...)
			if err != nil {
				t.Fatalf("collections %s with one key = %v, want the preview", tc.name, err)
			}
			tc.checkPreview(t, singleOut)
			if singleCtr.requests != 0 {
				t.Errorf("requests = %d, want 0: the preview must not reach the library", singleCtr.requests)
			}
		})
	}
}
