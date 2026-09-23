// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
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

func newSearchesMaterializeCmd(flags *rootFlags) *cobra.Command {
	var toCollection string
	var prune bool

	cmd := &cobra.Command{
		Use:   "materialize <searchKey> --to <collectionKey> [--prune]",
		Short: "Refresh a collection from a saved search, optionally removing stale members",
		Long: `Refresh a collection from a saved search. Add only missing items; report
unchanged and stale members. By default, leave stale members in the collection.
Use --prune to remove stale members; writes still require --yes. Refuse to prune
when no fileable search items would empty a non-empty collection. Child items
cannot be filed; add their parents to the saved search instead.

No search-to-collection binding is stored. For a scheduled refresh, put this
command in refresh.json, then run 'zotio watch --workflow refresh.json --yes'
after each sync (or run 'zotio workflow run refresh.json --yes').`,
		Annotations: map[string]string{
			"mcp:read-only":                    "false",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "false",
			"zotio:default-max-changes":        "500",
		},
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if toCollection == "" {
				return fmt.Errorf("required flag %q not set", "to")
			}
			return runSearchesMaterializeMutation(cmd, flags, args[0], toCollection, prune)
		},
	}
	cmd.Flags().StringVar(&toCollection, "to", "", "Collection key to refresh from saved-search items")
	cmd.Flags().BoolVar(&prune, "prune", false, "Remove collection members absent from the saved search (requires --yes to apply)")
	return cmd
}

func runSearchesMaterializeMutation(cmd *cobra.Command, flags *rootFlags, searchKey, toCollection string, prune bool) error {
	readClient, err := flags.newClient()
	if err != nil {
		return err
	}

	// Both sets come from the configured read plane. /collections/{key}/items
	// covers every item the collection endpoint returns; /items/top would
	// omit child rows and cannot scope to this collection. Zotero can return
	// child attachments despite their empty data.collections; only explicit
	// memberships count. Apply-time reads and PATCHes use the write plane.
	searchPath := "/searches/" + url.PathEscape(searchKey) + "/items"
	searchItems, err := searchesMaterializeItems(readClient, searchPath, "saved search "+searchKey)
	if err != nil {
		if isNetworkError(err) || isAPIStatus(err, http.StatusNotFound) {
			return emitPreconditionUnmetWithRemediation(cmd.OutOrStdout(), flags, "searches materialize", preconditionLiveLocalAPI,
				fmt.Sprintf("saved search %s could not be executed, so no membership is known to materialize: %v", searchKey, err),
				remediationFor(cmd.Context(), flags, preconditionLiveLocalAPI))
		}
		return err
	}
	collectionPath := "/collections/" + url.PathEscape(toCollection) + "/items"
	collectionItems, err := searchesMaterializeItems(readClient, collectionPath, "collection "+toCollection)
	if err != nil {
		return classifyAPIError(fmt.Errorf("cannot read target collection %s membership; refusing refresh: %w", toCollection, err), flags)
	}
	allKeys := make([]string, 0, len(searchItems))
	skippedChildren := make([]string, 0)
	for _, item := range searchItems {
		if item.Data.ParentItem != "" {
			skippedChildren = append(skippedChildren, item.Key)
			continue
		}
		allKeys = append(allKeys, item.Key)
	}
	currentKeys := make([]string, 0, len(collectionItems))
	for _, item := range collectionItems {
		if stringSliceContains(item.Data.Collections, toCollection) {
			currentKeys = append(currentKeys, item.Key)
		}
	}
	if prune && len(allKeys) == 0 && len(currentKeys) != 0 {
		return preconditionErr(fmt.Errorf("refusing --prune: saved search %s returned zero fileable items; pruning would empty collection %s (%d members), possibly because Zotero is closed or the search is broken", searchKey, toCollection, len(currentKeys)))
	}

	current := make(map[string]bool, len(currentKeys))
	for _, key := range currentKeys {
		current[key] = true
	}
	matched := make(map[string]bool, len(allKeys))
	adds := make([]string, 0, len(allKeys))
	unchanged := 0
	for _, key := range allKeys {
		matched[key] = true
		if current[key] {
			unchanged++
		} else {
			adds = append(adds, key)
		}
	}
	stale := make([]string, 0)
	for _, key := range currentKeys {
		if !matched[key] {
			stale = append(stale, key)
		}
	}

	var writeClient *client.Client
	if resolveMutationMode(flags).Apply && (len(adds) != 0 || (prune && len(stale) != 0)) {
		writeClient, err = flags.newWriteClient()
		if err != nil {
			return err
		}
	}
	ops := make([]mutation.Op, 0, len(adds)+len(stale))
	for _, key := range adds {
		keyCopy := key
		pathCopy := replacePathParam("/items/{itemKey}", "itemKey", keyCopy)
		ops = append(ops, mutation.Op{
			ID:          "searches.materialize:" + keyCopy,
			Key:         keyCopy,
			Kind:        "collection_add",
			Changes:     []mutation.Change{{Field: "collections", Add: toCollection}},
			Destructive: false,
			Apply: func() (string, any, error) {
				return applySearchesMaterializeCollectionAdd(writeClient, pathCopy, toCollection)
			},
		})
	}
	if prune {
		for _, key := range stale {
			keyCopy := key
			pathCopy := replacePathParam("/items/{itemKey}", "itemKey", keyCopy)
			ops = append(ops, mutation.Op{
				ID:          "searches.materialize:" + keyCopy,
				Key:         keyCopy,
				Kind:        "collection_remove",
				Changes:     []mutation.Change{{Field: "collections", Remove: toCollection}},
				Destructive: false, // As with items move --from, membership removal is reversible.
				Apply: func() (string, any, error) {
					return applyItemCollectionMove(writeClient, pathCopy, toCollection, "")
				},
			})
		}
	}
	env, runErr := runMutation(cmd.Context(), flags, "searches.materialize", ops)
	// Merge into the journal object rather than replace it: after an applied
	// run it already carries the run_id that journal undo and workflow steps
	// read, and --prune removals must stay undoable by that ID.
	journal, _ := env.Journal.(map[string]any)
	if journal == nil {
		journal = map[string]any{}
	}
	journal["unchanged_count"] = unchanged
	journal["stale_count"] = len(stale)
	journal["stale_keys"] = stale
	journal["skipped_child_count"] = len(skippedChildren)
	journal["skipped_child_keys"] = skippedChildren
	if len(skippedChildren) != 0 {
		journal["skipped_child_hint"] = "Child items cannot be filed; include their parents in the saved search to file them."
	}
	if len(stale) != 0 && !prune {
		journal["prune_hint"] = "Run again with --prune to remove stale members (and --yes to apply)."
	}
	if len(allKeys) == 0 {
		journal["message"] = "saved search returned no fileable items"
	}
	env.Journal = journal
	renderErr := renderMutation(cmd, flags, env, searchesMaterializeSingleLine(toCollection))
	if renderErr != nil {
		return renderErr
	}
	if (flags == nil || !flags.asJSON) && isTerminal(cmd.OutOrStdout()) {
		fmt.Fprintf(cmd.OutOrStdout(), "%d unchanged; %d stale", unchanged, len(stale))
		if len(stale) != 0 {
			fmt.Fprintf(cmd.OutOrStdout(), " (%s)", strings.Join(stale, ", "))
		}
		fmt.Fprintln(cmd.OutOrStdout())
		if hint, ok := journal["prune_hint"]; ok {
			fmt.Fprintln(cmd.OutOrStdout(), hint)
		}
		if len(skippedChildren) != 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "Skipped %d child item(s) (%s): %s\n", len(skippedChildren), strings.Join(skippedChildren, ", "), journal["skipped_child_hint"])
		}
	}
	return runErr
}

// searchesMaterializeItems reads every page and refuses a cross-page repeat.
// A duplicate within one page is harmless and produces only one item.
func searchesMaterializeItems(c *client.Client, path, label string) ([]searchMaterializeItem, error) {
	var allItems []searchMaterializeItem
	seen := make(map[string]bool, zoteroPageMax)
	for start := 0; ; start += zoteroPageMax {
		data, err := c.Get(path, map[string]string{
			"limit": strconv.Itoa(zoteroPageMax),
			"start": strconv.Itoa(start),
		})
		if err != nil {
			return nil, fmt.Errorf("fetching %s items at start %d: %w", label, start, err)
		}
		items, err := searchMaterializeItems(data)
		if err != nil {
			return nil, fmt.Errorf("parsing %s items at start %d: %w", label, start, err)
		}
		pageSeen := make(map[string]bool, len(items))
		for _, item := range items {
			if seen[item.Key] {
				return nil, fmt.Errorf("pagination for %s ignored start %d (duplicate key %s)", label, start, item.Key)
			}
			if !pageSeen[item.Key] {
				pageSeen[item.Key] = true
				allItems = append(allItems, item)
			}
		}
		for key := range pageSeen {
			seen[key] = true
		}
		if len(items) < zoteroPageMax {
			break
		}
	}
	return allItems, nil
}

type searchMaterializeItem struct {
	Key  string `json:"key"`
	Data struct {
		Collections []string `json:"collections"`
		ParentItem  string   `json:"parentItem"`
	} `json:"data"`
}

func searchMaterializeItems(data json.RawMessage) ([]searchMaterializeItem, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("expected an item array, not an empty or malformed response")
	}
	var items []searchMaterializeItem
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("parsing saved search items: %w", err)
	}
	for i, item := range items {
		if item.Key == "" {
			return nil, fmt.Errorf("saved search item %d missing key", i)
		}
	}
	return items, nil
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
		op := env.Plan.Operations[0]
		key := op.Key
		if op.Kind == "collection_remove" {
			if env.Mode == "apply" {
				if env.Result != nil && len(env.Result.Items) == 1 {
					switch env.Result.Items[0].Status {
					case "no_op":
						return fmt.Sprintf("%s no longer in %s", key, toCollection)
					case "conflict", "failed", "not_attempted", "skipped":
						return fmt.Sprintf("%s %s removing from %s", env.Result.Items[0].Status, key, toCollection)
					}
				}
				return fmt.Sprintf("removed %s from %s", key, toCollection)
			}
			return fmt.Sprintf("would remove %s from %s", key, toCollection)
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
