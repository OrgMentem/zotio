// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"zotio/internal/client"
	"zotio/internal/mutation"

	"github.com/spf13/cobra"
)

// zoteroResultIsEmpty reports whether a saved-search result page carries no
// items. It is a pagination terminator, not an availability check: an
// unreachable plane is refused as a precondition before this runs.
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

func newSearchesMaterializeCmd(flags *rootFlags) *cobra.Command {
	var toCollection string

	cmd := &cobra.Command{
		Use:   "materialize <searchKey> --to <collectionKey>",
		Short: "Add items from a saved search to a collection",
		Annotations: map[string]string{
			"mcp:read-only":                    "false",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "false",
			"zotio:default-max-changes":        "500",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if len(args) > 1 {
				return fmt.Errorf("accepts 1 arg(s), received %d", len(args))
			}
			if toCollection == "" {
				return fmt.Errorf("required flag %q not set", "to")
			}
			return runSearchesMaterializeMutation(cmd, flags, args[0], toCollection)
		},
	}
	cmd.Flags().StringVar(&toCollection, "to", "", "Collection key to add saved-search items into")
	return cmd
}

func runSearchesMaterializeMutation(cmd *cobra.Command, flags *rootFlags, searchKey, toCollection string) error {
	c, err := flags.newWriteClient()
	if err != nil {
		return err
	}

	searchPath := "/searches/" + url.PathEscape(searchKey) + "/items"
	// Walk the saved-search items endpoint to exhaustion. The Zotero API
	// paginates with limit/start (default ~25, max zoteroPageMax=100), so a
	// single unpaginated fetch silently truncates any search larger than one
	// page. Accumulate every page before building the mutation plan.
	//
	// Result membership is not mirrored (sync stores saved-search definitions
	// only), so this read has one plane: Zotero desktop's local API. There is
	// no resolveRead dispatch to make here, and no local fallback to offer.
	var allKeys []string
	seen := make(map[string]bool, zoteroPageMax)
	for start := 0; ; start += zoteroPageMax {
		params := map[string]string{
			"limit": strconv.Itoa(zoteroPageMax),
			"start": strconv.Itoa(start),
		}
		data, err := c.Get(searchPath, params)
		if err != nil {
			// A plane that cannot execute the search is a precondition, not an
			// empty plan. Rendering an empty plan here made "Zotero is closed"
			// look exactly like "the search matches nothing", and the operator
			// would conclude the collection needed no items.
			if start == 0 && (isNetworkError(err) || isAPIStatus(err, http.StatusNotFound)) {
				return emitPreconditionUnmetWithRemediation(cmd.OutOrStdout(), flags, "searches materialize", preconditionLiveLocalAPI,
					fmt.Sprintf("saved search %s could not be executed, so no membership is known to materialize: %v", searchKey, err),
					remediationFor(cmd.Context(), flags, preconditionLiveLocalAPI))
			}
			return fmt.Errorf("fetching saved search %s items at start %d: %w", searchKey, start, err)
		}
		if zoteroResultIsEmpty(data) {
			if start == 0 {
				return renderEmptySearchesMaterializePlan(cmd, flags, "saved search returned no items")
			}
			break
		}

		keys, err := searchMaterializeItemKeys(data)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			if start == 0 {
				return renderEmptySearchesMaterializePlan(cmd, flags, "saved search returned no item keys")
			}
			break
		}
		// A server that ignores start would repeat keys forever and either
		// loop or double-file items. Treat a cross-page repeat as a
		// pagination failure rather than silently duplicating operations.
		// A duplicate *within* a single page is a benign server anomaly:
		// skip the repeat and emit one operation. This keeps the
		// pagination-integrity signal (hard error for cross-page repeats)
		// while preventing duplicate mutation ops from a single-page quirk.
		pageSeen := make(map[string]bool, len(keys))
		unique := make([]string, 0, len(keys))
		for _, key := range keys {
			if seen[key] {
				return fmt.Errorf("pagination for saved search %s ignored start %d (duplicate key %s)", searchKey, start, key)
			}
			if pageSeen[key] {
				continue
			}
			pageSeen[key] = true
			unique = append(unique, key)
		}
		for _, key := range unique {
			seen[key] = true
		}
		allKeys = append(allKeys, unique...)
		if len(keys) < zoteroPageMax {
			break
		}
	}
	if len(allKeys) == 0 {
		return renderEmptySearchesMaterializePlan(cmd, flags, "saved search returned no item keys")
	}

	ops := make([]mutation.Op, 0, len(allKeys))
	for _, key := range allKeys {
		keyCopy := key
		pathCopy := replacePathParam("/items/{itemKey}", "itemKey", keyCopy)
		toCopy := toCollection
		ops = append(ops, mutation.Op{
			ID:          "searches.materialize:" + keyCopy,
			Key:         keyCopy,
			Kind:        "collection_add",
			Changes:     []mutation.Change{{Field: "collections", Add: toCollection}},
			Destructive: false,
			Apply: func() (string, any, error) {
				return applySearchesMaterializeCollectionAdd(c, pathCopy, toCopy)
			},
		})
	}

	env, runErr := runMutation(cmd.Context(), flags, "searches.materialize", ops)
	renderErr := renderMutation(cmd, flags, env, searchesMaterializeSingleLine(toCollection))
	if renderErr != nil {
		return renderErr
	}
	return runErr
}

func renderEmptySearchesMaterializePlan(cmd *cobra.Command, flags *rootFlags, message string) error {
	env, runErr := runMutation(cmd.Context(), flags, "searches.materialize", nil)
	env.Journal = map[string]any{"message": message}
	renderErr := renderMutation(cmd, flags, env, searchesMaterializeSingleLine(""))
	if renderErr != nil {
		return renderErr
	}
	if (flags == nil || !flags.asJSON) && isTerminal(cmd.OutOrStdout()) {
		fmt.Fprintln(cmd.OutOrStdout(), message)
	}
	return runErr
}

func searchMaterializeItemKeys(data json.RawMessage) ([]string, error) {
	var items []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("parsing saved search items: %w", err)
	}
	keys := make([]string, 0, len(items))
	for i, item := range items {
		if item.Key == "" {
			return nil, fmt.Errorf("saved search item %d missing key", i)
		}
		keys = append(keys, item.Key)
	}
	return keys, nil
}

func applySearchesMaterializeCollectionAdd(c *client.Client, path, toCollection string) (string, any, error) {
	currentData, currentVersion, err := c.GetWithVersion(path, nil)
	if err != nil {
		return "failed", err.Error(), err
	}
	currentCollections, err := itemCollections(currentData)
	if err != nil {
		return "failed", err.Error(), err
	}
	if stringSliceContains(currentCollections, toCollection) {
		return "no_op", "already in target collection", nil
	}
	nextCollections := append(append([]string(nil), currentCollections...), toCollection)
	body := map[string]any{"collections": nextCollections}
	// Fail closed when no write-plane version is available rather than
	// dispatching a preconditionless PATCH that Zotero would reject with an
	// opaque 428. patchWithVersionGuard sets If-Unmodified-Since-Version and
	// maps 412/428 to "conflict".
	return patchWithVersionGuard(c, path, body, currentVersion)
}

func searchesMaterializeSingleLine(toCollection string) func(mutation.Envelope) string {
	return func(env mutation.Envelope) string {
		key := "item"
		if len(env.Plan.Operations) == 1 {
			key = env.Plan.Operations[0].Key
		}
		if env.Mode == "apply" {
			if env.Result != nil && len(env.Result.Items) == 1 {
				switch env.Result.Items[0].Status {
				case "no_op":
					return fmt.Sprintf("%s already in %s", key, toCollection)
				case "conflict", "failed", "not_attempted", "skipped":
					return fmt.Sprintf("%s %s adding to %s", env.Result.Items[0].Status, key, toCollection)
				}
			}
			return fmt.Sprintf("added %s → %s", key, toCollection)
		}
		return fmt.Sprintf("would add %s → %s", key, toCollection)
	}
}
