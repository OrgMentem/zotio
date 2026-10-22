// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"zotio/internal/client"
	"zotio/internal/mutation"

	"github.com/spf13/cobra"
)

// tagsDeleteCarrierKeys walks every page of /items?tag=<name> for each tag and
// returns the distinct item keys in first-seen order, plus how many items the
// queries matched. Zotero caps the page size at 100, so it follows start
// offsets until a short page. It never calls DELETE /tags: removal goes
// through the item writes so versions, the journal and the mirror all see it.
func tagsDeleteCarrierKeys(c *client.Client, tagNames []string, limit int) ([]string, int, error) {
	if limit > 100 {
		limit = 100
	}
	seen := map[string]struct{}{}
	var keys []string
	matched := 0
	for _, tagName := range tagNames {
		query := tagName
		if strings.HasPrefix(query, "-") {
			// A leading dash negates the match in Zotero's tag filter.
			query = `\` + query
		}
		for start := 0; ; start += limit {
			data, err := c.Get("/items", map[string]string{
				"tag":   query,
				"limit": fmt.Sprintf("%d", limit),
				"start": fmt.Sprintf("%d", start),
			})
			if err != nil {
				return nil, 0, err
			}
			var page []struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(data, &page); err != nil {
				return nil, 0, fmt.Errorf("parsing items page: %w", err)
			}
			for _, item := range page {
				matched++
				if item.Key == "" {
					continue
				}
				if _, ok := seen[item.Key]; ok {
					continue
				}
				seen[item.Key] = struct{}{}
				keys = append(keys, item.Key)
			}
			if len(page) < limit {
				break
			}
		}
	}
	return keys, matched, nil
}

// noCarriersRun reports a delete that found nothing to remove. The reason says
// which of the two situations it is, because a caller cannot act on a bare
// "selected: 0".
func noCarriersRun(cmd *cobra.Command, flags *rootFlags, operation string, tagNames []string, reason map[string]any) error {
	ops := []mutation.Op{{
		ID:         operation + ":" + strings.Join(tagNames, ","),
		Key:        strings.Join(tagNames, ","),
		Kind:       "tag_remove",
		NoOpReason: reason,
	}}
	env, runErr := runMutation(cmd.Context(), flags, operation, ops)
	return renderMutationResult(cmd, flags, env, runErr, itemTagsSingleLine(false, tagNames))
}

func newTagsDeleteCmd(flags *rootFlags) *cobra.Command {
	var automaticOnly bool
	var batch bool
	var pageSize int

	cmd := &cobra.Command{
		Use:   "delete <name>...",
		Short: "Delete tags by removing them from every item that carries them",
		Long: "Delete one or more tags by removing each from every item that carries it. " +
			"Each carrier is one version-guarded item write, so the run is previewed by default, " +
			"journaled, and undone with `journal undo`. Zotero's DELETE /tags endpoint is never called.",
		Args: cobra.MinimumNArgs(1),
		Annotations: map[string]string{
			"mcp:read-only": "false",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			tagNames, err := normalizeTagNames(args)
			if err != nil {
				return err
			}
			if pageSize <= 0 {
				return usageErr(fmt.Errorf("invalid --page-size %d: must be greater than zero", pageSize))
			}
			// Selection must see the plane the write lands on, uncached; see
			// the same note in `tags rename`.
			selectClient, selectErr := flags.newSelectionClient()
			if selectErr != nil {
				var cliErr *cliError
				if errors.As(selectErr, &cliErr) {
					return selectErr
				}
				return classifyAPIError(selectErr, flags)
			}
			selectClient.DryRun = false
			keys, matched, selectErr := tagsDeleteCarrierKeys(selectClient, tagNames, pageSize)
			if selectErr != nil {
				return classifyAPIError(selectErr, flags)
			}
			if len(keys) == 0 {
				return noCarriersRun(cmd, flags, "tags.delete", tagNames, map[string]any{
					"reason":  "no item carries this tag in the write plane; nothing to delete",
					"tags":    tagNames,
					"matched": matched,
				})
			}
			return runTagsMutationForKeys(cmd, flags, "tags.delete", "tag_remove", tagNames, keys, false, automaticOnly, batch)
		},
	}
	cmd.Flags().BoolVar(&automaticOnly, "automatic-only", false, "Remove only Zotero automatic tags (type 1) with this name; leave manual tags alone")
	cmd.Flags().BoolVar(&batch, "batch", false, batchTagFlagHelp)
	cmd.Flags().IntVar(&pageSize, "page-size", 100, "Items to fetch per page while selecting carriers of the tag (1-100)")
	return cmd
}

// automaticTagCount is how many distinct mirrored items carry a tag as an
// automatic (type 1) tag, and which.
type automaticTagCount struct {
	Tag   string   `json:"tag"`
	Items int      `json:"items"`
	Keys  []string `json:"keys"`
}

// automaticTagCounts counts distinct items per automatic tag from mirror rows
// shaped {item_key, tag_json}, ordered by count then name.
func automaticTagCounts(rows []map[string]any) []automaticTagCount {
	byTag := map[string]*automaticTagCount{}
	seen := map[struct{ tag, key string }]struct{}{}
	for _, row := range rows {
		var tag struct {
			Tag  string `json:"tag"`
			Type int    `json:"type"`
		}
		if err := json.Unmarshal([]byte(sqlStringValue(row["tag_json"])), &tag); err != nil || tag.Tag == "" || tag.Type != 1 {
			continue
		}
		key := sqlStringValue(row["item_key"])
		id := struct{ tag, key string }{tag.Tag, key}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		entry := byTag[tag.Tag]
		if entry == nil {
			entry = &automaticTagCount{Tag: tag.Tag}
			byTag[tag.Tag] = entry
		}
		entry.Items++
		entry.Keys = append(entry.Keys, key)
	}
	out := make([]automaticTagCount, 0, len(byTag))
	for _, entry := range byTag {
		sort.Strings(entry.Keys)
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Items != out[j].Items {
			return out[i].Items < out[j].Items
		}
		return strings.ToLower(out[i].Tag) < strings.ToLower(out[j].Tag)
	})
	return out
}

func newTagsPruneCmd(flags *rootFlags) *cobra.Command {
	var automatic bool
	var minItems int
	var batch bool

	cmd := &cobra.Command{
		Use:   "prune --automatic [--min-items N]",
		Short: "Delete automatic tags carried by fewer than N items",
		Long: "Find automatic (type 1) tags carried by fewer than --min-items items in the local mirror and " +
			"remove them from their carriers. Without --yes the command only lists the candidates, from the " +
			"mirror alone, with no network request. With --yes each carrier is one version-guarded item write, " +
			"journaled and undone with `journal undo`.",
		Args: cobra.NoArgs,
		Annotations: map[string]string{
			"mcp:read-only": "false",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if !automatic {
				return usageErr(fmt.Errorf("tags prune needs --automatic: it only prunes automatic tags"))
			}
			if minItems < 1 {
				return usageErr(fmt.Errorf("invalid --min-items %d: must be at least 1", minItems))
			}
			if err := requireMirrorOnlySource(flags, cmd.CommandPath()); err != nil {
				return err
			}
			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			if rawDB == nil {
				return errUnsyncedStoreFor(cmd, flags)
			}
			defer rawDB.Close()
			rows, err := localQueryStore{rawDB}.QueryRaw(`
SELECT i.id AS item_key, t.value AS tag_json
FROM resources i, json_each(json_extract(i.data,'$.data.tags')) AS t
WHERE i.resource_type='items'`)
			if err != nil {
				return fmt.Errorf("querying library tag usage: %w", err)
			}
			var candidates []automaticTagCount
			for _, entry := range automaticTagCounts(rows) {
				if entry.Items < minItems {
					candidates = append(candidates, entry)
				}
			}

			if !resolveMutationMode(flags).Apply {
				if candidates == nil {
					candidates = []automaticTagCount{}
				}
				data, err := json.Marshal(candidates)
				if err != nil {
					return err
				}
				return printMirrorRows(cmd, flags, json.RawMessage(data), localProvenance(rawDB, "tags", "local_only"))
			}
			if len(candidates) == 0 {
				return noCarriersRun(cmd, flags, "tags.prune", []string{"(automatic tags under --min-items)"}, map[string]any{
					"reason":    "no automatic tag in the mirror is carried by fewer than --min-items items",
					"min_items": minItems,
				})
			}
			tagNames := make([]string, 0, len(candidates))
			seen := map[string]struct{}{}
			var keys []string
			for _, entry := range candidates {
				tagNames = append(tagNames, entry.Tag)
				for _, key := range entry.Keys {
					if _, ok := seen[key]; !ok {
						seen[key] = struct{}{}
						keys = append(keys, key)
					}
				}
			}
			sort.Strings(keys)
			rawDB.Close()
			return runTagsMutationForKeys(cmd, flags, "tags.prune", "tag_remove", tagNames, keys, false, true, batch)
		},
	}
	cmd.Flags().BoolVar(&automatic, "automatic", false, "Prune automatic (type 1) tags; required")
	cmd.Flags().IntVar(&minItems, "min-items", 2, "Prune automatic tags carried by fewer than this many items")
	cmd.Flags().BoolVar(&batch, "batch", false, batchTagFlagHelp)
	return cmd
}
