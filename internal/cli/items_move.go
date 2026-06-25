// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written item move workflow missing from the generated CLI.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/spf13/cobra"
	"zotero-pp-cli/internal/client"
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

			// PATCH(glean write-safety): plan first, then let the shared helper decide preview vs apply.
			c, err := flags.newWriteClient()
			if err != nil {
				return err
			}
			path := replacePathParam("/items/{itemKey}", "itemKey", args[0])
			data, version, err := c.GetWithVersion(path, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			collections, err := itemCollections(data)
			if err != nil {
				return err
			}

			changes := []mutationChange{{Field: "collections", Add: flagTo}}
			for _, collection := range collections {
				if collection == flagTo {
					changes = nil
					break
				}
			}

			itemKey := args[0]
			op := plannedOp{
				ID:              "items.move:" + itemKey,
				Key:             itemKey,
				Kind:            "collection_add",
				ExpectedVersion: version,
				Changes:         changes,
				Destructive:     false,
				apply: func() (string, any, error) {
					// PATCH(glean write-safety): apply re-reads from the write target before patching.
					currentData, currentVersion, err := c.GetWithVersion(path, nil)
					if err != nil {
						return "failed", err.Error(), err
					}
					currentCollections, err := itemCollections(currentData)
					if err != nil {
						return "failed", err.Error(), err
					}
					for _, collection := range currentCollections {
						if collection == flagTo {
							return "no_op", "already in target collection", nil
						}
					}
					nextCollections := append(append([]string{}, currentCollections...), flagTo)
					body := map[string]any{"collections": nextCollections}
					headers := map[string]string{}
					if currentVersion > 0 {
						headers["If-Unmodified-Since-Version"] = strconv.Itoa(currentVersion)
					}
					_, statusCode, err := c.PatchWithHeaders(path, body, headers)
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
				},
			}
			env, runErr := runMutation(cmd.Context(), flags, "items.move", []plannedOp{op})
			renderErr := renderMutation(cmd, flags, env, func(env mutationEnvelope) string {
				status := "would move"
				if env.Mode == "apply" {
					status = "moved"
					if env.Result != nil && len(env.Result.Items) == 1 {
						switch env.Result.Items[0].Status {
						case "no_op":
							status = "already in"
						case "conflict", "failed", "not_attempted":
							status = env.Result.Items[0].Status
						}
					}
				} else if len(env.Plan.Operations) == 1 && len(env.Plan.Operations[0].Changes) == 0 {
					status = "already in"
				}
				if status == "already in" {
					return fmt.Sprintf("%s already in %s", itemKey, flagTo)
				}
				return fmt.Sprintf("%s %s → %s", status, itemKey, flagTo)
			})
			if renderErr != nil {
				return renderErr
			}
			return runErr
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
