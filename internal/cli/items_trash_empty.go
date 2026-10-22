// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

var olderThanDaysRe = regexp.MustCompile(`^(\d+)d$`)

// parseOlderThan reads a trash age: whole days ("30d") or a Go duration
// ("720h"). A zero or negative age is refused, since it would select the
// whole trash while looking like a filter.
func parseOlderThan(raw string) (time.Duration, error) {
	if m := olderThanDaysRe.FindStringSubmatch(raw); m != nil {
		days, err := strconv.Atoi(m[1])
		if err == nil && days > 0 {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	} else if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d, nil
	}
	return 0, usageErr(fmt.Errorf("invalid --older-than %q: use a positive age such as 30d or 720h", raw))
}

func newItemsTrashEmptyCmd(flags *rootFlags) *cobra.Command {
	var olderThan, keysFrom string

	cmd := &cobra.Command{
		Use:   "empty [--older-than DUR] [--keys-from <file|->]",
		Short: "Permanently delete items from the trash",
		Long: `Permanently delete items that are already in the trash. This cannot be undone
by 'items restore' or 'journal undo', so it is previewed by default and applying
needs --yes and --allow-destructive.

The command pages the whole live trash and plans one permanent delete per item.
--older-than keeps only items whose dateModified is older than the age (30d or
720h); an item with no readable dateModified is never selected by an age filter.
--keys-from (a file or '-' for stdin) keeps only the listed keys, and a key that
is not in the trash is a usage error. Without a filter the whole trash is
selected, so read the count in the preview first.

Each permanent delete leaves a deletion marker in the local mirror, as
'items delete --permanent' does.`,
		Example: "  zotio items trash empty --older-than 30d\n  zotio items trash empty --older-than 30d --yes --allow-destructive",
		Args:    cobra.NoArgs,
		Annotations: map[string]string{
			"mcp:read-only": "false",
			"zotio:method":  "DELETE",
			"zotio:path":    "/items/{itemKey}",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var cutoff time.Time
			if olderThan != "" {
				age, err := parseOlderThan(olderThan)
				if err != nil {
					return err
				}
				cutoff = time.Now().Add(-age)
			}
			var wanted map[string]bool
			if keysFrom != "" {
				keys, err := resolveKeys(nil, keysFrom, cmd.InOrStdin())
				if err != nil {
					return err
				}
				wanted = make(map[string]bool, len(keys))
				for _, key := range keys {
					wanted[key] = true
				}
			}

			c, clientErr := flags.newWriteClient()
			if clientErr != nil {
				return clientErr
			}
			data, _, fetchErr := fetchAllLiveTrash(cmd.Context(), c, flags, "/items/trash")
			if fetchErr != nil {
				return fetchErr
			}
			var entries []json.RawMessage
			if err := json.Unmarshal(data, &entries); err != nil {
				return apiErr(fmt.Errorf("parsing trash response: %w", err))
			}

			var applyErr error
			ops := make([]mutation.Op, 0, len(entries))
			for _, entry := range sortTrashItems(entries) {
				key := jsonStringField(entry, "key")
				if key == "" {
					continue
				}
				if wanted != nil {
					if !wanted[key] {
						continue
					}
					delete(wanted, key)
				}
				if !cutoff.IsZero() {
					modified, ok := trashDateModified(entry)
					if !ok || !modified.Before(cutoff) {
						continue
					}
				}
				version := trashEntryVersion(entry)
				path := replacePathParam("/items/{itemKey}", "itemKey", key)
				ops = append(ops, mutation.Op{
					ID:              "items.trash.empty:" + key,
					Key:             key,
					Kind:            "item_delete",
					Destructive:     true,
					ExpectedVersion: version,
					Changes:         []mutation.Change{{Field: "deleted", Add: true}},
					Apply: func() (string, any, error) {
						// The version came from the trash listing just read, so
						// an ambiguous delete reconciles like items delete does.
						_, _, status, detail, writeErr := deleteWithVersionGuard(c, path, version)
						if writeErr != nil {
							applyErr = writeErr
						}
						return status, detail, writeErr
					},
				})
			}
			if len(wanted) > 0 {
				missing := make([]string, 0, len(wanted))
				for key := range wanted {
					missing = append(missing, key)
				}
				sort.Strings(missing)
				return usageErr(fmt.Errorf("not in the trash, nothing was planned: %s", strings.Join(missing, ", ")))
			}

			if len(ops) == 0 {
				ops = []mutation.Op{{
					ID:   "items.trash.empty:none",
					Key:  "trash",
					Kind: "item_delete",
					NoOpReason: map[string]any{
						"code":    "nothing_to_empty",
						"message": "no trashed item matches the selection",
					},
				}}
			} else if !flags.quiet {
				fmt.Fprintf(cmd.ErrOrStderr(), "%d trashed item(s) selected for permanent deletion\n", len(ops))
			}
			env, runErr := runMutation(cmd.Context(), flags, "items.trash.empty", ops)
			if runErr != nil && applyErr != nil {
				runErr = classifyAPIError(applyErr, flags)
			}
			return renderMutationResult(cmd, flags, env, runErr, nil)
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "", "Only items whose dateModified is older than this age (e.g. 30d, 720h)")
	cmd.Flags().StringVar(&keysFrom, "keys-from", "", "Only these keys, read from a file or '-' for stdin (lines or JSON); each must be in the trash")
	return cmd
}

// trashEntryVersion reads the version a delete must carry from a trash row,
// whether the API nests it under data or puts it at the top.
func trashEntryVersion(raw json.RawMessage) int {
	var head struct {
		Version any `json:"version"`
		Data    struct {
			Version any `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return 0
	}
	if v := snapshotVersion(head.Version); v != 0 {
		return v
	}
	return snapshotVersion(head.Data.Version)
}
