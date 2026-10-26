// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

func TestCreateUnusableSuccessResponsePreservesUnknownOutcome(t *testing.T) {
	for _, route := range []string{"single", "import_file", "items_create", "collections_create"} {
		for _, tc := range []struct {
			name     string
			body     string
			rejected bool
		}{
			{name: "malformed_json", body: `{"successful":`},
			{name: "non_envelope", body: `{"error":"proxy error"}`},
			{name: "missing_result", body: `{"successful":{},"failed":{}}`},
			{name: "missing_key", body: `{"successful":{"0":{}}}`},
			{name: "malformed_rejection", body: `{"failed":{"0":{"code":"bad"}}}`},
			{name: "explicit_rejection", body: `{"failed":{"0":{"code":400,"message":"invalid field"}}}`, rejected: true},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				setupCreateRouteJournal(t)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tc.body))
				}))
				defer srv.Close()
				t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
				flags := &rootFlags{yes: true, asJSON: true, maxChanges: -1, via: "web"}
				var env mutation.Envelope
				var err error
				switch route {
				case "single":
					c, clientErr := flags.newClient()
					if clientErr != nil {
						t.Fatal(clientErr)
					}
					env, err = runMutation(context.Background(), flags, "items.new", []mutation.Op{{
						ID: "create", Kind: "item_create", Changes: []mutation.Change{{Field: "title", Add: "Example"}},
						Apply: func() (string, any, error) {
							res, createErr := routeCreateItemVia(context.Background(), flags, "web", c, map[string]any{"title": "Example", "itemType": "book"}, "", false)
							if res.Via != "web" {
								t.Errorf("route information lost: %+v", res)
							}
							return singleItemCreateApplyResult(res, createErr, "")
						},
					}})
				case "import_file":
					file := writeImportFixture(t, "@article{example,\n title = {Example}\n}\n")
					_, raw, runErr := runImportFile(t, flags, file)
					err = runErr
					if decodeErr := json.Unmarshal([]byte(raw), &env); decodeErr != nil {
						t.Fatal(decodeErr)
					}
				default:
					var cmd *cobra.Command
					if route == "items_create" {
						cmd = newItemsCreateCmd(flags)
						cmd.SetArgs([]string{"--items", `[{"title":"Example","itemType":"book"}]`})
					} else {
						cmd = newCollectionsCreateCmd(flags)
						cmd.SetArgs([]string{"--name", "Example"})
					}
					cmd.SilenceErrors, cmd.SilenceUsage = true, true
					var out bytes.Buffer
					cmd.SetOut(&out)
					cmd.SetErr(io.Discard)
					err = cmd.Execute()
					if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
						t.Fatalf("decode %s: %v", out.String(), decodeErr)
					}
				}
				if err == nil {
					t.Fatalf("unusable create response reported success: %+v", env)
				}
				if !tc.rejected {
					assertCreateRouteUnknown(t, env)
					reason, _ := env.Result.Items[0].Reason.(map[string]any)
					if reason["via"] != "web" {
						t.Fatalf("missing route evidence: %v", reason)
					}
				} else if journal, _ := env.Journal.(map[string]any); env.Result == nil || env.Result.Summary.Failed != 1 || journal["run_id"] != nil {
					t.Fatalf("explicit rejection must remain an uncommitted failure: %+v", env)
				}
			})
		}
	}
}
