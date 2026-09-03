// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"zotio/internal/client"

	"github.com/spf13/cobra"
)

func newSearchesRunCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run <searchKey>",
		Short: "Run a saved Zotero search when the API exposes results",
		Long: `Executes a saved search and returns the matching items.

Sync mirrors saved-search DEFINITIONS, never their result membership, so the
result read runs against Zotero desktop's local API. With Zotero closed, or on
a plane without /searches/{key}/items, the command refuses with a
precondition_unmet envelope (exit 9) instead of an empty-looking answer.`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			// encode the saved-search key as a single path segment.
			rawSearchKey := args[0]
			searchKey := url.PathEscape(rawSearchKey)

			var c *client.Client
			if flags.dataSource != "local" {
				var err error
				c, err = flags.newClient()
				if err != nil {
					return err
				}
			}

			// The saved-search definition IS mirrored, so it reads through
			// resolveRead like every other keyed read: --data-source, provenance,
			// and the auto fallback all behave the same here.
			//
			// It is read before the results, and not only for reporting: a 404
			// from /searches/{key}/items is ambiguous between "no such saved
			// search" and "this plane has no such endpoint", and only a
			// successful definition read separates them.
			searchPath := "/searches/" + searchKey
			searchData, prov, err := resolveRead(cmd.Context(), c, flags, "searches", false, searchPath, nil, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}

			// Result membership requires evaluating the saved search's condition
			// set, which only Zotero can do (ADR-0002 defers local evaluation
			// until the supported operator subset is decided). So this read has
			// exactly one plane, and --data-source local is a refusal rather
			// than a live read wearing a local label.
			if flags.dataSource == "local" {
				return refuseSavedSearchResults(cmd, flags, rawSearchKey, searchData,
					"sync mirrors saved-search definitions only, so --data-source local cannot evaluate this search's conditions")
			}
			results, resultErr := c.Get(searchPath+"/items", nil)
			if resultErr != nil {
				// An empty page from a working endpoint is a legitimate answer
				// (the search currently matches nothing) and prints as []. Only
				// a plane that cannot execute the search reaches this branch,
				// and it must be loud: reporting it as ok/empty is what made a
				// closed Zotero indistinguishable from a 0-hit search.
				if isNetworkError(resultErr) || isAPIStatus(resultErr, http.StatusNotFound) {
					return refuseSavedSearchResults(cmd, flags, rawSearchKey, searchData,
						fmt.Sprintf("the saved-search results endpoint could not be executed on the configured plane: %v", resultErr))
				}
				return classifyAPIError(resultErr, flags)
			}
			printProvenance(cmd, countResultItems(results), prov)
			return printOutputWithFlags(cmd.OutOrStdout(), results, flags)
		},
	}
	return cmd
}

// refuseSavedSearchResults refuses a saved-search execution the configured
// plane cannot perform, naming the search so the operator knows which one
// failed. The definition is available even offline (it is mirrored); the
// result membership is not, and a fabricated broad /items query cannot be
// distinguished from an unfiltered library dump when the plane silently
// ignores unknown query parameters — which Zotero does.
func refuseSavedSearchResults(cmd *cobra.Command, flags *rootFlags, searchKey string, definition []byte, detail string) error {
	name := savedSearchName(definition)
	label := searchKey
	if name != "" {
		label = fmt.Sprintf("%s (%q)", searchKey, name)
	}
	return emitPreconditionUnmetWithRemediation(cmd.OutOrStdout(), flags, "searches run", preconditionLiveLocalAPI,
		fmt.Sprintf("saved search %s cannot be executed: %s", label, detail),
		remediationFor(cmd.Context(), flags, preconditionLiveLocalAPI))
}

// savedSearchName reads the human name out of a mirrored or live saved-search
// definition so a refusal can name the search instead of only its key.
func savedSearchName(definition []byte) string {
	if len(definition) == 0 {
		return ""
	}
	var row struct {
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(definition, &row); err != nil {
		return ""
	}
	if row.Data.Name != "" {
		return row.Data.Name
	}
	return row.Name
}

// isAPIStatus reports whether err is an API response carrying exactly status.
func isAPIStatus(err error, status int) bool {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == status
}
