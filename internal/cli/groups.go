// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"zotio/internal/config"
)

func newGroupsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "groups",
		Short:       "Discover Zotero group libraries you can access",
		Annotations: map[string]string{"mcp:read-only": "true"},
	}
	cmd.AddCommand(newGroupsListCmd(flags))
	cmd.AddCommand(newGroupsInspectCmd(flags))
	return cmd
}

func newGroupsListCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the group libraries the configured user belongs to",
		Example: `  zotio groups list
  zotio groups list --json

  # Then sync and search a specific group:
  zotio sync --group <id>
  zotio search --group <id> <query>`,
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

func newGroupsInspectCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "inspect <group-id>",
		Short:       "Check whether a Zotero group library is accessible and writable",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			groupID := args[0]
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			// Groups are enumerated under the personal-library prefix
			// (<base>/users/<id>/groups, which c.Get reaches via "/groups").
			// A group-scoped base URL has no user segment to list from.
			if _, ok := userIDFromBaseURL(c.BaseURL); !ok {
				return usageErr(fmt.Errorf("set a personal-library base URL (…/users/<id>) to inspect groups; the current base URL targets a group library"))
			}
			data, err := c.Get("/groups", nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}

			var groups []map[string]any
			if err := json.Unmarshal(data, &groups); err != nil {
				return fmt.Errorf("parsing groups response: %w", err)
			}

			note := fmt.Sprintf("group %s is not accessible: not a member, or the API key lacks access to it", groupID)
			report := map[string]any{
				"group_id":        groupID,
				"found":           false,
				"name":            "",
				"type":            "",
				"num_items":       "",
				"library_reading": "",
				"library_editing": "",
				"file_editing":    "",
				"ready_for_read":  false,
				"ready_for_write": false,
				"note":            note,
			}
			cfg, err := config.Load(flags.configPath)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			for _, g := range groups {
				if groupFieldString(g, "id") != groupID {
					continue
				}
				libraryEditing := groupFieldString(g, "libraryEditing")
				// writability comes from the API key's
				// per-group permission (/keys/current access), not the group's
				// editing policy, which is near-always non-empty and would
				// over-claim write for read-only keys and admin-only groups.
				canWrite, keyKnown := keyGroupWriteAccess(cmd.Context(), cfg, flags.timeout, groupID)
				readyForWrite := keyKnown && canWrite
				switch {
				case !keyKnown:
					note = "group is readable; write access could not be confirmed — configure an API key with write access to this group"
				case readyForWrite:
					note = "group is accessible for reading and writing"
				default:
					note = "group is readable; the configured API key lacks write access to this group"
				}
				report["found"] = true
				report["name"] = groupFieldString(g, "name")
				report["type"] = groupFieldString(g, "type")
				report["num_items"] = groupFieldString(g, "numItems")
				report["library_reading"] = groupFieldString(g, "libraryReading")
				report["library_editing"] = libraryEditing
				report["file_editing"] = groupFieldString(g, "fileEditing")
				report["ready_for_read"] = true
				report["ready_for_write"] = readyForWrite
				report["key_write_known"] = keyKnown
				report["note"] = note
				break
			}

			marshaled, err := json.Marshal(report)
			if err != nil {
				return err
			}
			if flags.asJSON {
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(marshaled), flags)
			}
			if !report["found"].(bool) {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), report["note"])
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "group %s %s: read=%t write=%t, %s items\n",
				groupID,
				report["name"],
				report["ready_for_read"],
				report["ready_for_write"],
				report["num_items"],
			)
			return err
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
