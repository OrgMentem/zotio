// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

type searchesRunFallback struct {
	ResultsAvailable bool            `json:"results_available"`
	Message          string          `json:"message"`
	ResultError      string          `json:"result_error,omitempty"`
	Search           json.RawMessage `json:"search"`
}

func newSearchesRunCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "run <searchKey>",
		Short:       "Run a saved Zotero search when the API exposes results",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			// encode the saved-search key as a single path segment.
			rawSearchKey := args[0]
			searchKey := url.PathEscape(rawSearchKey)
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			searchData, err := c.Get("/searches/"+searchKey, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}

			results, resultErr := c.Get("/searches/"+searchKey+"/items", nil)
			if resultErr == nil && !zoteroResultIsEmpty(results) {
				return printOutputWithFlags(cmd.OutOrStdout(), results, flags)
			}

			errText := ""
			if resultErr != nil {
				errText = resultErr.Error()
			}
			// The correct endpoint for saved-search results is /searches/{key}/items.
			// A fabricated broad query against /items cannot be distinguished from an
			// unfiltered library dump when the plane ignores unknown query parameters
			// (Zotero silently ignores them), so the fallback is removed entirely;
			// report unavailable instead of risking unrelated items.
			reason := "Saved search results are unavailable: the saved-search results endpoint is unavailable on the configured plane and saved-search conditions cannot be evaluated remotely."
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", reason)
			fallback := searchesRunFallback{
				ResultsAvailable: false,
				Message:          reason,
				ResultError:      errText,
				Search:           searchData,
			}
			data, err := json.Marshal(fallback)
			if err != nil {
				return err
			}
			return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
		},
	}
	return cmd
}

func zoteroResultIsEmpty(data json.RawMessage) bool {
	if len(strings.TrimSpace(string(data))) == 0 {
		return true
	}
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err == nil {
		return len(items) == 0
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return false
	}
	for _, key := range []string{"data", "items", "results"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		if json.Unmarshal(raw, &items) == nil {
			return len(items) == 0
		}
	}
	return false
}
