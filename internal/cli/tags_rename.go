// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written tag rename workflow missing from the generated CLI.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
	"zotio/internal/client"
)

type tagRenameUpdate struct {
	key     string
	version any
	tags    []any
}

type tagRenameResult struct {
	Key    string `json:"key"`
	OldTag string `json:"old_tag"`
	NewTag string `json:"new_tag"`
	Status string `json:"status"`
}

func newTagsRenameCmd(flags *rootFlags) *cobra.Command {
	var flagFrom string
	var flagTo string
	var flagLimit int

	cmd := &cobra.Command{
		Use:   "rename --from <oldTag> --to <newTag>",
		Short: "Rename a tag across matching items",
		Annotations: map[string]string{
			"pp:endpoint":                   "tags.rename",
			"pp:method":                     "PATCH",
			"pp:path":                       "/items/{itemKey}",
			"mcp:read-only":                 "false",
			"pp:destructive":                "false",
			"pp:supports-dry-run":           "true",
			"pp:requires-allow-destructive": "false",
			"pp:default-max-changes":        "500",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagFrom == "" {
				return fmt.Errorf("required flag %q not set", "from")
			}
			if flagTo == "" {
				return fmt.Errorf("required flag %q not set", "to")
			}
			if flagLimit <= 0 {
				return fmt.Errorf("--limit must be greater than zero")
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}
			if flags.dryRun {
				c.DryRun = false
				data, err := c.Get("/items", map[string]string{
					"tag":   flagFrom,
					"limit": fmt.Sprintf("%d", flagLimit),
				})
				if err != nil {
					return classifyAPIError(err, flags)
				}

				updates, err := buildTagRenameUpdates(data, flagFrom, flagTo)
				if err != nil {
					return err
				}
				results := tagRenameResults(updates, flagFrom, flagTo, "dry_run")
				if flags.asJSON {
					return printFullTagRenameResults(cmd, results)
				}
				if flags.csv || flags.plain {
					return printJSONFiltered(cmd.OutOrStdout(), results, flags)
				}
				if flags.quiet {
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Would update %d items: rename tag %s -> %s\n", len(updates), flagFrom, flagTo)
				for _, update := range updates {
					fmt.Fprintln(cmd.OutOrStdout(), update.key)
				}
				return nil
			}

			// PATCH(glean write-safety): share the live rename operation with tags audit fix.
			status, reason, err := renameTagWithLimit(c, flagFrom, flagTo, flagLimit)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			results, _ := reason.([]tagRenameResult)
			if status == "no_op" {
				results = []tagRenameResult{}
			}

			if flags.asJSON {
				return printFullTagRenameResults(cmd, results)
			}
			if flags.csv || flags.plain {
				return printJSONFiltered(cmd.OutOrStdout(), results, flags)
			}
			if flags.quiet {
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Renamed tag '%s' to '%s' in %d items\n", flagFrom, flagTo, len(results))
			return nil
		},
	}
	cmd.Flags().StringVar(&flagFrom, "from", "", "Old tag name")
	cmd.Flags().StringVar(&flagTo, "to", "", "New tag name")
	cmd.Flags().IntVar(&flagLimit, "limit", 100, "Maximum number of items to process per page")

	return cmd
}

func renameTag(c *client.Client, oldName, newName string) (string, any, error) {
	return renameTagWithLimit(c, oldName, newName, 100)
}

func renameTagWithLimit(c *client.Client, oldName, newName string, limit int) (string, any, error) {
	data, err := c.Get("/items", map[string]string{
		"tag":   oldName,
		"limit": fmt.Sprintf("%d", limit),
	})
	if err != nil {
		return "failed", err.Error(), err
	}

	updates, err := buildTagRenameUpdates(data, oldName, newName)
	if err != nil {
		return "failed", err.Error(), err
	}
	if len(updates) == 0 {
		return "no_op", []tagRenameResult{}, nil
	}

	results := make([]tagRenameResult, 0, len(updates))
	for _, update := range updates {
		path := replacePathParam("/items/{itemKey}", "itemKey", update.key)
		_, _, err := c.Patch(path, map[string]any{
			"version": update.version,
			"tags":    update.tags,
		})
		if err != nil {
			var apiErr *client.APIError
			if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusPreconditionFailed || apiErr.StatusCode == http.StatusPreconditionRequired) {
				return "conflict", apiErr.Body, err
			}
			return "failed", err.Error(), err
		}
		results = append(results, tagRenameResult{
			Key:    update.key,
			OldTag: oldName,
			NewTag: newName,
			Status: "updated",
		})
	}
	return "applied", results, nil
}

func printFullTagRenameResults(cmd *cobra.Command, results []tagRenameResult) error {
	data, err := json.Marshal(results)
	if err != nil {
		return err
	}
	return printOutput(cmd.OutOrStdout(), json.RawMessage(data), true)
}

func buildTagRenameUpdates(data json.RawMessage, oldTag, newTag string) ([]tagRenameUpdate, error) {
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("parsing items response: %w", err)
	}

	updates := make([]tagRenameUpdate, 0, len(items))
	for _, item := range items {
		key, ok := item["key"].(string)
		if !ok || key == "" {
			return nil, fmt.Errorf("item response missing key")
		}
		version, ok := item["version"]
		if !ok {
			return nil, fmt.Errorf("item %s missing version", key)
		}
		tags, changed, err := renamedItemTags(item, oldTag, newTag)
		if err != nil {
			return nil, fmt.Errorf("item %s: %w", key, err)
		}
		if !changed {
			continue
		}
		updates = append(updates, tagRenameUpdate{
			key:     key,
			version: version,
			tags:    tags,
		})
	}
	return updates, nil
}

func renamedItemTags(item map[string]any, oldTag, newTag string) ([]any, bool, error) {
	dataObj, ok := item["data"].(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("missing data object")
	}
	rawTags, ok := dataObj["tags"].([]any)
	if !ok {
		return []any{}, false, nil
	}

	renamed := make([]any, 0, len(rawTags))
	changed := false
	for _, rawTag := range rawTags {
		tagObj, ok := rawTag.(map[string]any)
		if !ok {
			renamed = append(renamed, rawTag)
			continue
		}
		copied := make(map[string]any, len(tagObj))
		for k, v := range tagObj {
			copied[k] = v
		}
		if tagName, ok := copied["tag"].(string); ok && tagName == oldTag {
			copied["tag"] = newTag
			changed = true
		}
		renamed = append(renamed, copied)
	}
	return renamed, changed, nil
}

func tagRenameResults(updates []tagRenameUpdate, oldTag, newTag, status string) []tagRenameResult {
	results := make([]tagRenameResult, 0, len(updates))
	for _, update := range updates {
		results = append(results, tagRenameResult{
			Key:    update.key,
			OldTag: oldTag,
			NewTag: newTag,
			Status: status,
		})
	}
	return results
}
