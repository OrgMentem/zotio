// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"zotio/internal/client"
	"zotio/internal/mutation"

	"github.com/spf13/cobra"
)

type tagRenameUpdate struct {
	key     string
	version any
	tags    []any
}

func newTagsRenameCmd(flags *rootFlags) *cobra.Command {
	var flagFrom string
	var flagTo string
	var flagLimit int

	cmd := &cobra.Command{
		Use:   "rename --from <oldTag> --to <newTag>",
		Short: "Rename a tag across matching items",
		Annotations: map[string]string{
			"zotio:endpoint":                   "tags.rename",
			"zotio:method":                     "PATCH",
			"zotio:path":                       "/items/{itemKey}",
			"mcp:read-only":                    "false",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "false",
			"zotio:default-max-changes":        "500",
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
			// Planning a rename always has to read the real matching item set;
			// --dry-run controls the mutation engine, not the discovery GETs.
			c.DryRun = false
			updates, err := listTagRenameUpdates(c, flagFrom, flagTo, flagLimit)
			if err != nil {
				return classifyAPIError(err, flags)
			}

			var renameApply func(tagRenameUpdate) (string, any, error)
			ops := buildTagRenameOps(updates, flagFrom, flagTo, func(update tagRenameUpdate) (string, any, error) {
				if renameApply == nil {
					err := errors.New("write client not initialized")
					return "failed", err.Error(), err
				}
				return renameApply(update)
			})
			if resolveMutationMode(flags).Apply && len(ops) > 0 {
				writeClient, err := flags.newWriteClient()
				if err != nil {
					return err
				}
				renameApply = func(update tagRenameUpdate) (string, any, error) {
					return applyTagRenameUpdate(writeClient, update)
				}
			}

			env, runErr := runMutation(cmd.Context(), flags, "tags.rename", ops)
			renderErr := renderMutation(cmd, flags, env, tagRenameSingleLine(flagFrom, flagTo))
			if renderErr != nil {
				return renderErr
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&flagFrom, "from", "", "Old tag name")
	cmd.Flags().StringVar(&flagTo, "to", "", "New tag name")
	cmd.Flags().IntVar(&flagLimit, "limit", 100, "Maximum number of items to process per page")

	return cmd
}

func buildTagRenameOps(updates []tagRenameUpdate, oldName, newName string, apply func(tagRenameUpdate) (string, any, error)) []mutation.Op {
	ops := make([]mutation.Op, 0, len(updates))
	for _, update := range updates {
		update := update
		ops = append(ops, mutation.Op{
			ID:              "tags.rename:" + update.key,
			Key:             update.key,
			Kind:            "tag_rename",
			ExpectedVersion: mutationExpectedVersion(update.version),
			Changes:         []mutation.Change{{Field: "tag", Remove: oldName, Add: newName}},
			Destructive:     false,
			Apply: func() (string, any, error) {
				return apply(update)
			},
		})
	}
	return ops
}

func applyTagRenameUpdate(c *client.Client, update tagRenameUpdate) (string, any, error) {
	path := replacePathParam("/items/{itemKey}", "itemKey", update.key)
	headers := map[string]string{}
	if version := mutationExpectedVersion(update.version); version > 0 {
		headers["If-Unmodified-Since-Version"] = strconv.Itoa(version)
	}
	_, statusCode, err := c.PatchWithHeaders(path, map[string]any{
		"tags": update.tags,
	}, headers)
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusPreconditionFailed || apiErr.StatusCode == http.StatusPreconditionRequired) {
			return "conflict", apiErr.Body, err
		}
		return "failed", err.Error(), err
	}
	if statusCode < 200 || statusCode >= 300 {
		return "failed", fmt.Sprintf("HTTP %d", statusCode), fmt.Errorf("patch returned HTTP %d", statusCode)
	}
	return "applied", nil, nil
}

func tagRenameSingleLine(oldName, newName string) func(mutation.Envelope) string {
	return func(env mutation.Envelope) string {
		action := "would rename"
		if env.Mode == "apply" {
			action = "renamed"
		}
		return fmt.Sprintf("%s tag %s -> %s in %d item(s)", action, oldName, newName, env.Plan.Summary.Planned)
	}
}

func listTagRenameUpdates(c *client.Client, oldName, newName string, limit int) ([]tagRenameUpdate, error) {
	// Zotero caps /items?tag pages, so walk start offsets until a short page
	// instead of reporting the first page as a complete rename.
	if limit > 100 {
		limit = 100
	}
	var all []tagRenameUpdate
	for start := 0; ; start += limit {
		data, err := c.Get("/items", map[string]string{
			"tag":   oldName,
			"limit": fmt.Sprintf("%d", limit),
			"start": fmt.Sprintf("%d", start),
		})
		if err != nil {
			return nil, err
		}
		updates, err := buildTagRenameUpdates(data, oldName, newName)
		if err != nil {
			return nil, err
		}
		all = append(all, updates...)
		var page []json.RawMessage
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, fmt.Errorf("parsing items page: %w", err)
		}
		if len(page) < limit {
			break
		}
	}
	return all, nil
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
