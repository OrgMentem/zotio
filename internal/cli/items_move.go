// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written item move workflow missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

func newItemsMoveCmd(flags *rootFlags) *cobra.Command {
	var flagTo string

	cmd := &cobra.Command{
		Use:         "move <itemKey> --to <collectionKey>",
		Short:       "Add an item to a collection",
		Annotations: map[string]string{"pp:endpoint": "items.move", "pp:method": "PATCH", "pp:path": "/items/{itemKey}"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if flagTo == "" {
				return fmt.Errorf("required flag %q not set", "to")
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			if flags.dryRun {
				c.DryRun = false
			}

			path := "/items/{itemKey}"
			path = replacePathParam(path, "itemKey", args[0])
			data, err := c.Get(path, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			collections, err := itemCollections(data)
			if err != nil {
				return err
			}
			for _, collection := range collections {
				if collection == flagTo {
					fmt.Fprintf(cmd.OutOrStdout(), "Item already in collection %s\n", flagTo)
					return nil
				}
			}
			collections = append(collections, flagTo)
			body := map[string]any{"collections": collections}
			if flags.dryRun {
				envelope := map[string]any{
					"action":   "patch",
					"body":     body,
					"dry_run":  true,
					"path":     path,
					"resource": "items",
					"status":   0,
					"success":  false,
				}
				envelopeJSON, err := json.Marshal(envelope)
				if err != nil {
					return err
				}
				return printOutput(cmd.OutOrStdout(), json.RawMessage(envelopeJSON), true)
			}

			data, statusCode, err := c.Patch(path, body)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			envelope := map[string]any{"action": "patch", "resource": "items", "path": path, "status": statusCode, "success": statusCode >= 200 && statusCode < 300}
			if len(data) > 0 {
				var p any
				if json.Unmarshal(data, &p) == nil {
					envelope["data"] = p
				}
			}
			envelopeJSON, err := json.Marshal(envelope)
			if err != nil {
				return err
			}
			return printOutput(cmd.OutOrStdout(), json.RawMessage(envelopeJSON), true)
		},
	}
	cmd.Flags().StringVar(&flagTo, "to", "", "Collection key to move item into")

	return cmd
}

func itemCollections(data json.RawMessage) ([]string, error) {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("parsing item response: %w", err)
	}
	dataObj, ok := obj["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("item response missing data object")
	}
	rawCollections, ok := dataObj["collections"].([]any)
	if !ok {
		return []string{}, nil
	}
	collections := make([]string, 0, len(rawCollections))
	for _, raw := range rawCollections {
		if collection, ok := raw.(string); ok && collection != "" {
			collections = append(collections, collection)
		}
	}
	return collections, nil
}
