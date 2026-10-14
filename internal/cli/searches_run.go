// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
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

Zotero desktop evaluates the search through its local API; that read is
authoritative. Sync mirrors the saved-search definition, so with
--data-source local, or when the desktop cannot answer (Zotero closed, or a
plane without /searches/{key}/items), the definition is evaluated against the
synced items instead. That local evaluation supports title and other text
fields, creator, tag, collection, item type, and date conditions with any/all
join and the noChildren/includeParents/includeChildren flags. A search that
uses any other condition (full text, notes, nested searches, groups, ...) is
never evaluated partially: the command refuses with a precondition_unmet
envelope (exit 9) naming the condition, instead of an empty-looking answer.
Local results come from the last sync and include child items exactly as
Zotero's search does.`,
		Example:     "  zotio searches run ABCD2345",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
				if flags.dataSource == "live" && isNetworkError(err) {
					// --data-source live forbids the mirror, so an unreachable
					// desktop is the live_local_api precondition, not a bare error.
					return refuseSavedSearchResults(cmd, flags, rawSearchKey, nil,
						fmt.Sprintf("the live read plane is unreachable: %v", err))
				}
				return classifyAPIError(err, flags)
			}

			// Zotero desktop evaluates a saved search's conditions, so the
			// live read is authoritative. --data-source local, and the auto
			// fallback when the live read plane cannot execute the search,
			// evaluate the synced definition against the mirrored items
			// instead (savedsearch_local.go). That evaluator supports a
			// narrow operator subset and refuses the rest whole, so a
			// condition it cannot reproduce ends in the live_local_api
			// precondition below, never in a smaller result set.
			if flags.dataSource == "local" {
				served, detail, err := emitSavedSearchLocalResults(cmd, flags, searchData, "user_requested")
				if err != nil || served {
					return err
				}
				return refuseSavedSearchResults(cmd, flags, rawSearchKey, searchData,
					"--data-source local could not evaluate this search against the mirror: "+detail)
			}
			results, resultErr := c.Get(searchPath+"/items", nil)
			if resultErr != nil {
				// An empty page from a working endpoint is a legitimate answer
				// (the search currently matches nothing) and prints as []. Only
				// a plane that cannot execute the search reaches this branch,
				// and it must be loud: reporting it as ok/empty is what made a
				// closed Zotero indistinguishable from a 0-hit search.
				if isNetworkError(resultErr) || isAPIStatus(resultErr, http.StatusNotFound) {
					liveDetail := fmt.Sprintf("the saved-search results endpoint could not be executed on the configured plane: %v", resultErr)
					if flags.dataSource != "live" {
						served, detail, err := emitSavedSearchLocalResults(cmd, flags, searchData, "api_unreachable")
						if err != nil || served {
							return err
						}
						liveDetail += "; the local mirror could not evaluate it either: " + detail
					}
					return refuseSavedSearchResults(cmd, flags, rawSearchKey, searchData, liveDetail)
				}
				return classifyAPIError(resultErr, flags)
			}
			printProvenance(cmd, countResultItems(results), prov)
			return printSavedSearchItems(cmd, flags, results, prov)
		},
	}
	return cmd
}

// emitSavedSearchLocalResults evaluates a saved-search definition against the
// local mirror and prints the matched items, one stored item payload each, the
// same payloads the local item reads return. served reports whether results
// were printed; when false, detail says why the mirror could not serve the
// search (unsupported condition, no mirror) and nothing was printed. Only an
// unexpected store failure returns an error.
func emitSavedSearchLocalResults(cmd *cobra.Command, flags *rootFlags, definition []byte, reason string) (served bool, detail string, err error) {
	// The live attempt may have consumed the caller's deadline, so the
	// mirror read runs detached from it, as resolveRead's own fallback does.
	ctx := context.WithoutCancel(cmd.Context())
	db, err := openStoreForRead(ctx, "zotio")
	if err != nil {
		return false, fmt.Sprintf("the local mirror could not be opened: %v", err), nil
	}
	if db == nil {
		return false, "there is no local mirror; run 'zotio sync'", nil
	}
	defer db.Close()

	local, err := evaluateSavedSearchDefinition(ctx, db, definition)
	if err != nil {
		var unsupported *savedSearchUnsupportedError
		if errors.As(err, &unsupported) {
			return false, unsupported.Error(), nil
		}
		if errors.Is(err, errSavedSearchItemsNotSynced) {
			return false, err.Error(), nil
		}
		return false, "", err
	}
	prov := localProvenanceContext(ctx, db, "items", reason)
	prov.Scoped = true
	prov = attachFreshness(prov, flags)
	encoded, err := json.Marshal(local.Items)
	if err != nil {
		return false, "", fmt.Errorf("encoding local saved-search results: %w", err)
	}
	printProvenance(cmd, len(local.Items), prov)
	return true, "", printSavedSearchItems(cmd, flags, encoded, prov)
}

// printSavedSearchItems prints saved-search result rows. Under --json/--agent
// or piped stdout the rows are wrapped in the {meta, results} provenance
// envelope every other list read returns.
func printSavedSearchItems(cmd *cobra.Command, flags *rootFlags, data json.RawMessage, prov DataProvenance) error {
	if !flags.quiet && wantsJSONEnvelope(cmd.OutOrStdout(), flags) {
		filtered, err := applyFieldFlags(data, flags)
		if err != nil {
			return err
		}
		wrapped, err := wrapWithProvenance(filtered, prov)
		if err != nil {
			return err
		}
		return printOutput(cmd.OutOrStdout(), wrapped, true)
	}
	return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
}

// refuseSavedSearchResults refuses a saved-search execution the configured
// plane cannot perform, naming the search so the operator knows which one
// failed. The definition is available even offline (it is mirrored), but the
// local evaluator reproduces only a subset of Zotero's conditions, and a
// fabricated broad /items query cannot be distinguished from an unfiltered
// library dump when the plane silently ignores unknown query parameters —
// which Zotero does.
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
