// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH: Add hand-written annotation listing workflow missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newItemsAnnotationsCmd(flags *rootFlags) *cobra.Command {
	var flagColor string
	var flagType string

	cmd := &cobra.Command{
		Use:         "annotations <itemKey>",
		Short:       "List annotation children for an item",
		Annotations: map[string]string{"pp:endpoint": "items.annotations", "pp:method": "GET", "pp:path": "/items/{itemKey}/children", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			path := "/items/{itemKey}/children"
			path = replacePathParam(path, "itemKey", args[0])
			params := map[string]string{"itemType": "annotation"}
			data, prov, err := resolveRead(cmd.Context(), c, flags, "items", false, path, params, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			if flagColor != "" || flagType != "" {
				data, err = filterAnnotationItems(data, flagColor, flagType)
				if err != nil {
					return err
				}
			}
			{
				var items []json.RawMessage
				_ = json.Unmarshal(data, &items)
				printProvenance(cmd, len(items), prov)
			}
			if flags.asJSON || !isTerminal(cmd.OutOrStdout()) {
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
	cmd.Flags().StringVar(&flagColor, "color", "", "Filter by annotation color (yellow, red, green, blue, purple, orange)")
	cmd.Flags().StringVar(&flagType, "type", "", "Filter by annotation type (highlight, note, image)")

	return cmd
}

func filterAnnotationItems(data json.RawMessage, color, annotationType string) (json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("parsing annotations response: %w", err)
	}
	color = strings.TrimSpace(color)
	annotationType = strings.TrimSpace(annotationType)
	filtered := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		if color != "" && !strings.EqualFold(jsonStringField(item, "annotationColor"), color) {
			continue
		}
		if annotationType != "" && !strings.EqualFold(jsonStringField(item, "annotationType"), annotationType) {
			continue
		}
		filtered = append(filtered, item)
	}
	out, err := json.Marshal(filtered)
	if err != nil {
		return nil, err
	}
	return out, nil
}
