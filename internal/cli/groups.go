// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean 9bfn): group-library discovery command (not in the generated CLI).

package cli

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

func newGroupsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "groups",
		Short:       "Discover Zotero group libraries you can access",
		Annotations: map[string]string{"mcp:read-only": "true"},
	}
	cmd.AddCommand(newGroupsListCmd(flags))
	return cmd
}

func newGroupsListCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the group libraries the configured user belongs to",
		Example: `  zotero-pp-cli groups list
  zotero-pp-cli groups list --json

  # Then sync and search a specific group:
  zotero-pp-cli sync --group <id>
  zotero-pp-cli search --group <id> <query>`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			// Groups are enumerated under the personal-library prefix
			// (<base>/users/<id>/groups, which c.Get reaches via "/groups").
			// A group-scoped base URL has no user segment to list from.
			if _, ok := userIDFromBaseURL(c.BaseURL); !ok {
				return usageErr(fmt.Errorf("set a personal-library base URL (…/users/<id>) to list groups; the current base URL targets a group library"))
			}
			data, err := c.Get("/groups", nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			if flags.asJSON {
				return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
			}
			rows, err := groupTableRows(data)
			if err != nil {
				return err
			}
			return flags.printTable(cmd, []string{"id", "name", "type", "numItems"}, rows)
		},
	}
	return cmd
}

func groupTableRows(data json.RawMessage) ([][]string, error) {
	var groups []map[string]any
	if err := json.Unmarshal(data, &groups); err != nil {
		return nil, fmt.Errorf("parsing groups response: %w", err)
	}
	rows := make([][]string, 0, len(groups))
	for _, g := range groups {
		rows = append(rows, []string{
			groupFieldString(g, "id"),
			groupFieldString(g, "name"),
			groupFieldString(g, "type"),
			groupFieldString(g, "numItems"),
		})
	}
	return rows, nil
}

// groupFieldString resolves a field from a Zotero group object, checking the
// top level first, then the nested data and meta sub-objects (name/type live
// under data, numItems under meta), and rendering numbers without exponent.
func groupFieldString(g map[string]any, field string) string {
	v := g[field]
	if v == nil {
		if data, ok := g["data"].(map[string]any); ok && data[field] != nil {
			v = data[field]
		} else if meta, ok := g["meta"].(map[string]any); ok && meta[field] != nil {
			v = meta[field]
		}
	}
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprintf("%v", t)
	}
}
