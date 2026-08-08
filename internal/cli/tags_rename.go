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
	key string
	// oldTag/newTag travel with the update because one run can carry several
	// distinct renames: `tags audit fix` plans an alias→canonical rename per
	// group, so the pair cannot come from command flags at apply time.
	oldTag string
	newTag string
	// version is the read plane's object version, recorded for the plan only.
	// The write precondition is resolved from the write plane at apply time
	// (see applyTagRenameUpdate) because the two planes do not share a version
	// space.
	version any
	// tagType is the manual(0)/automatic(1) type of the renamed alias tag on
	// this specific item, captured so the replayed mutation.Change neither
	// fabricates nor drops that distinction in the local mirror.
	tagType int
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
			// Renaming a tag to itself used to PATCH every carrier: item versions
			// bumped, the journal gained a meaningless run, and other clients saw
			// spurious modifications — all to change nothing. No selection query
			// is needed to know that.
			selfRename := flagFrom == flagTo

			var updates []tagRenameUpdate
			matched := 0
			if !selfRename {
				// Selection decides what gets written, so it must see the plane the
				// write lands on, uncached.
				//
				// Reads normally go to the local desktop API while writes route to
				// api.zotero.org, and a tag written moments ago does not exist on
				// the read plane until Zotero syncs it down (~15s). Worse, the
				// response cache would then pin that emptiness for its full
				// 5-minute TTL: a preview run during the propagation window cached
				// the empty result, and the apply that followed served the cache
				// and silently renamed nothing, reporting ok and exit 0.
				// Previewing first — the documented, careful workflow — was itself
				// what broke the apply.
				selectClient, selectErr := flags.newSelectionClient()
				if selectErr != nil {
					return selectErr
				}
				// --dry-run controls the mutation engine, not the discovery GETs.
				selectClient.DryRun = false
				updates, matched, selectErr = listTagRenameUpdates(selectClient, flagFrom, flagTo, flagLimit)
				if selectErr != nil {
					return classifyAPIError(selectErr, flags)
				}
			}

			var renameApply func(tagRenameUpdate) (string, any, error)
			ops := buildTagRenameOps(updates, flagFrom, flagTo, func(update tagRenameUpdate) (string, any, error) {
				if renameApply == nil {
					err := errors.New("write client not initialized")
					return "failed", err.Error(), err
				}
				return renameApply(update)
			})
			// A rename that changes nothing must say WHY: "selected: 0" was
			// byte-identical for a self-rename, a tag that does not exist, and the
			// silent plane/cache failure above.
			if len(ops) == 0 {
				ops = []mutation.Op{{
					ID:         "tags.rename:" + flagFrom,
					Key:        flagFrom,
					Kind:       "tag_rename",
					NoOpReason: emptyTagRenameReason(selfRename, flagFrom, flagTo, matched),
				}}
			}
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

// emptyTagRenameReason explains a rename that planned nothing. Three very
// different situations previously produced the identical `selected: 0, ok: true,
// exit 0`: renaming a tag to itself, a tag no item carries, and a tag whose
// carriers all already hold the target name. A caller cannot act on any of them
// without a code.
func emptyTagRenameReason(selfRename bool, from, to string, matched int) map[string]any {
	switch {
	case selfRename:
		return map[string]any{
			"code":    "same_name",
			"message": fmt.Sprintf("--from and --to are both %q; nothing to rename", from),
			"from":    from,
		}
	case matched == 0:
		return map[string]any{
			"code":    "tag_not_found",
			"message": fmt.Sprintf("no item carries tag %q on the plane this write targets", from),
			"from":    from,
		}
	default:
		return map[string]any{
			"code":    "already_renamed",
			"message": fmt.Sprintf("%d item(s) carry %q, but none as a tag distinct from %q", matched, from, to),
			"from":    from,
			"to":      to,
			"matched": matched,
		}
	}
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
			Changes:         []mutation.Change{{Field: "tags", Remove: oldName, Add: newName, TagType: update.tagType}},
			Destructive:     false,
			Apply: func() (string, any, error) {
				return apply(update)
			},
		})
	}
	return ops
}

// applyTagRenameUpdate performs the rename against the write plane.
//
// It deliberately re-reads the item from the write plane instead of PATCHing the
// tag list captured at plan time. Plan-time state comes from the read plane (the
// local desktop API), whose object versions live in a different version space
// and are empty for items never pushed upstream — so the plan-time version
// yields no If-Unmodified-Since-Version header and Zotero refuses the write
// outright ("Either If-Unmodified-Since-Version or object version property must
// be provided for key-based writes"). Re-reading also means the PATCH replaces
// the write plane's own tag list rather than overwriting it with a stale copy.
func applyTagRenameUpdate(c *client.Client, update tagRenameUpdate) (string, any, error) {
	path := replacePathParam("/items/{itemKey}", "itemKey", update.key)
	currentData, currentVersion, err := c.GetFromWriteBaseWithVersion(path, nil)
	if err != nil {
		return "failed", err.Error(), err
	}
	var current map[string]any
	if err := json.Unmarshal(currentData, &current); err != nil {
		wrapped := fmt.Errorf("parsing item %s from the write plane: %w", update.key, err)
		return "failed", wrapped.Error(), wrapped
	}
	tags, changed, _, err := renamedItemTags(current, update.oldTag, update.newTag)
	if err != nil {
		wrapped := fmt.Errorf("item %s: %w", update.key, err)
		return "failed", wrapped.Error(), wrapped
	}
	if !changed {
		return "no_op", map[string]any{
			"code":    "tag_absent",
			"message": fmt.Sprintf("item no longer carries tag %q on the write plane", update.oldTag),
			"from":    update.oldTag,
			"to":      update.newTag,
		}, nil
	}

	// Defence in depth: nothing may PATCH without a precondition. Zotero refuses
	// such a write anyway ("Either If-Unmodified-Since-Version or object version
	// property must be provided for key-based writes"); failing here reports the
	// real cause instead of relaying that opaque message.
	if currentVersion <= 0 {
		err := fmt.Errorf("no write-plane version for item %s; refusing to write without an If-Unmodified-Since-Version precondition", update.key)
		return "failed", err.Error(), err
	}
	headers := map[string]string{"If-Unmodified-Since-Version": strconv.Itoa(currentVersion)}
	_, statusCode, err := c.PatchWithHeaders(path, map[string]any{
		"tags": tags,
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

// listTagRenameUpdates returns the items to rename, plus how many items the tag
// query actually matched. A caller must not report "nothing to do" when the tag
// demonstrably exists: that output is indistinguishable from "no such tag",
// which is how New-1's version-less-row bug stayed invisible.
func listTagRenameUpdates(c *client.Client, oldName, newName string, limit int) ([]tagRenameUpdate, int, error) {
	// Zotero caps /items?tag pages, so walk start offsets until a short page
	// instead of reporting the first page as a complete rename.
	if limit > 100 {
		limit = 100
	}
	var all []tagRenameUpdate
	matched := 0
	for start := 0; ; start += limit {
		data, err := c.Get("/items", map[string]string{
			"tag":   oldName,
			"limit": fmt.Sprintf("%d", limit),
			"start": fmt.Sprintf("%d", start),
		})
		if err != nil {
			return nil, 0, err
		}
		updates, err := buildTagRenameUpdates(data, oldName, newName)
		if err != nil {
			return nil, 0, err
		}
		all = append(all, updates...)
		var page []json.RawMessage
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, 0, fmt.Errorf("parsing items page: %w", err)
		}
		matched += len(page)
		if len(page) < limit {
			break
		}
	}
	return all, matched, nil
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
		// The read plane's version is recorded for the plan only. A row may
		// legitimately carry none: write-through strips the stale version from
		// rows it replays, because keeping it would assert a version that plane
		// never issued. Requiring one here made every item zotio had just
		// written invisible to `tags rename` (silently, as selected: 0) and
		// aborted a whole `tags audit fix` batch on the first such row. The
		// write precondition comes from the write plane at apply time, so a
		// missing version is not an obstacle to renaming.
		version := item["version"]
		// The renamed tag list is recomputed from the write plane at apply time;
		// only the fact that this item matches, and the alias tag's type, matter
		// to the plan.
		_, changed, tagType, err := renamedItemTags(item, oldTag, newTag)
		if err != nil {
			return nil, fmt.Errorf("item %s: %w", key, err)
		}
		if !changed {
			continue
		}
		updates = append(updates, tagRenameUpdate{
			key:     key,
			oldTag:  oldTag,
			newTag:  newTag,
			version: version,
			tagType: tagType,
		})
	}
	return updates, nil
}

func renamedItemTags(item map[string]any, oldTag, newTag string) ([]any, bool, int, error) {
	dataObj, ok := item["data"].(map[string]any)
	if !ok {
		return nil, false, 0, fmt.Errorf("missing data object")
	}
	rawTags, ok := dataObj["tags"].([]any)
	if !ok {
		return []any{}, false, 0, nil
	}

	renamed := make([]any, 0, len(rawTags))
	seen := make(map[string]struct{}, len(rawTags))
	changed := false
	tagType := 0
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
		tagName, ok := copied["tag"].(string)
		if ok && tagName == oldTag {
			// Capture the alias's own manual/automatic type before renaming it:
			// Zotero tracks type per tag object, and the replayed mutation.Change
			// must carry it forward so the local mirror doesn't flip it.
			tagType = itemTagType(copied)
			copied["tag"] = newTag
			tagName = newTag
			changed = true
		}
		if ok {
			if _, exists := seen[tagName]; exists {
				continue
			}
			seen[tagName] = struct{}{}
		}
		renamed = append(renamed, copied)
	}
	return renamed, changed, tagType, nil
}
