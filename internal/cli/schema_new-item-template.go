// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/client"
)

func newSchemaNewItemTemplateCmd(flags *rootFlags) *cobra.Command {
	var flagItemType string

	cmd := &cobra.Command{
		Use:         "new-item-template",
		Short:       "Get a blank template for creating a new item of a given type",
		Example:     "  zotio schema new-item-template --item-type example-value",
		Annotations: map[string]string{"zotio:endpoint": "schema.new-item-template", "zotio:method": "GET", "zotio:path": "/items/new", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("item-type") && !flags.dryRun {
				return fmt.Errorf("required flag \"%s\" not set", "item-type")
			}
			// /items/new is global (Web API), but the local API does not serve it.
			// Build the same template from the global schema endpoints that local
			// Zotero does serve. This needs no Web API credentials or round trip.
			c, err := newSchemaClient(flags)
			if err != nil {
				return err
			}

			itemType := strings.TrimSpace(flagItemType)
			data, prov, err := buildLocalItemTemplate(cmd.Context(), c, flags, itemType)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			// Print provenance to stderr for human-facing output
			printProvenance(cmd, countResultItems(data), prov)
			// For JSON output, wrap with provenance envelope before passing through flags.
			// --select wins over --compact when both are set; --compact only runs when
			// no explicit fields were requested.
			if wantsJSONEnvelope(cmd.OutOrStdout(), flags) {
				filtered := data
				if flags.selectFields != "" {
					filtered = filterFields(filtered, flags.selectFields)
				} else if flags.compact {
					filtered = compactFields(filtered)
				}
				wrapped, wrapErr := wrapWithProvenance(filtered, prov)
				if wrapErr != nil {
					return wrapErr
				}
				return printOutput(cmd.OutOrStdout(), wrapped, true)
			}
			// For all other output modes (table, csv, plain, quiet), use the standard pipeline
			if wantsHumanTable(cmd.OutOrStdout(), flags) {
				var items []map[string]any
				if json.Unmarshal(data, &items) == nil && len(items) > 0 {
					if err := printAutoTable(cmd.OutOrStdout(), items); err != nil {
						return err
					}
					if len(items) >= 25 {
						fmt.Fprintf(os.Stderr, "\nShowing %d results. To narrow: add --limit, --json --select, or filter flags.\n", len(items))
					}
					return nil
				}
			}
			return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
		},
	}
	cmd.Flags().StringVar(&flagItemType, "item-type", "", "Item type to get template for (e.g. journalArticle)")

	return cmd
}

// buildLocalItemTemplate mirrors Zotero's /items/new response from schema
// endpoints served by both the Web API and local desktop API. The first
// creator type is the primary type used by Zotero's template endpoint.
func buildLocalItemTemplate(ctx context.Context, c *client.Client, flags *rootFlags, itemType string) (json.RawMessage, DataProvenance, error) {
	params := map[string]string{"itemType": itemType}
	fieldsData, prov, err := resolveRead(ctx, c, flags, "schema", true, "/itemTypeFields", params, nil)
	if err != nil {
		return nil, DataProvenance{}, err
	}
	fields, err := decodeOrderedSchemaValues(fieldsData, "/itemTypeFields", "field")
	if err != nil {
		return nil, DataProvenance{}, err
	}
	if len(fields) == 0 {
		return nil, DataProvenance{}, fmt.Errorf("/itemTypeFields returned no fields for %q", itemType)
	}

	creatorTypesData, _, err := resolveRead(ctx, c, flags, "schema", true, "/itemTypeCreatorTypes", params, nil)
	if err != nil {
		return nil, DataProvenance{}, err
	}
	creatorTypes, err := decodeOrderedSchemaValues(creatorTypesData, "/itemTypeCreatorTypes", "creatorType")
	if err != nil {
		return nil, DataProvenance{}, err
	}

	creatorFieldsData, _, err := resolveRead(ctx, c, flags, "schema", true, "/creatorFields", nil, nil)
	if err != nil {
		return nil, DataProvenance{}, err
	}
	creatorFields, err := decodeOrderedSchemaValues(creatorFieldsData, "/creatorFields", "field")
	if err != nil {
		return nil, DataProvenance{}, err
	}

	data, err := marshalLocalItemTemplate(itemType, fields, creatorTypes, creatorFields)
	if err != nil {
		return nil, DataProvenance{}, err
	}
	return data, prov, nil
}

// decodeOrderedSchemaValues extracts schema names without sorting: endpoint
// order is significant for the primary creator type and matches Zotero's
// template field order.
func decodeOrderedSchemaValues(data json.RawMessage, path, key string) ([]string, error) {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("parsing %s response: %w", path, err)
	}
	seen := make(map[string]struct{}, len(rows))
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		raw, ok := row[key]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values, nil
}

func marshalLocalItemTemplate(itemType string, fields, creatorTypes, creatorFields []string) (json.RawMessage, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	first := true
	write := func(key string, value any) error {
		if !first {
			out.WriteByte(',')
		}
		first = false
		keyJSON, err := json.Marshal(key)
		if err != nil {
			return err
		}
		valueJSON, err := json.Marshal(value)
		if err != nil {
			return err
		}
		out.Write(keyJSON)
		out.WriteByte(':')
		out.Write(valueJSON)
		return nil
	}
	if err := write("itemType", itemType); err != nil {
		return nil, err
	}

	seen := map[string]struct{}{"itemType": {}}
	if len(creatorTypes) > 0 {
		creator := map[string]any{"creatorType": creatorTypes[0]}
		creatorKeys := creatorTemplateFields(creatorFields)
		for _, key := range creatorKeys {
			creator[key] = ""
		}
		if err := write("creators", []map[string]any{creator}); err != nil {
			return nil, err
		}
	}
	for _, field := range fields {
		if _, duplicate := seen[field]; duplicate {
			continue
		}
		if field == "tags" || field == "collections" || field == "relations" || field == "creators" {
			continue
		}
		seen[field] = struct{}{}
		if err := write(field, ""); err != nil {
			return nil, err
		}
	}
	if err := write("tags", []map[string]any{}); err != nil {
		return nil, err
	}
	if err := write("collections", []string{}); err != nil {
		return nil, err
	}
	if err := write("relations", map[string]any{}); err != nil {
		return nil, err
	}
	out.WriteByte('}')
	return json.RawMessage(out.Bytes()), nil
}

func creatorTemplateFields(fields []string) []string {
	hasFirst, hasLast, hasName := false, false, false
	for _, field := range fields {
		switch field {
		case "firstName":
			hasFirst = true
		case "lastName":
			hasLast = true
		case "name":
			hasName = true
		}
	}
	if hasFirst && hasLast {
		return []string{"firstName", "lastName"}
	}
	if hasName {
		return []string{"name"}
	}
	return fields
}
