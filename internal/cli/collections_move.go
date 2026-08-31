// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

func newCollectionsMoveCmd(flags *rootFlags) *cobra.Command {
	var flagTo string

	cmd := &cobra.Command{
		Use:         "move <collectionKey> --to <parentKey>",
		Short:       "Move a collection under a new parent",
		Annotations: map[string]string{"zotio:method": "PUT", "zotio:path": "/collections/{collectionKey}"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if !cmd.Flags().Changed("to") {
				return fmt.Errorf("required flag %q not set", "to")
			}

			parentCollection := any(flagTo)
			if flagTo == "" || flagTo == "root" {
				parentCollection = false
			}
			path := replacePathParam("/collections/{collectionKey}", "collectionKey", args[0])

			if !resolveMutationMode(flags).Apply {
				if flags.quiet {
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Would move collection %s under parent %s\n", args[0], flagTo)
				return nil
			}

			c, err := flags.newWriteClient()
			if err != nil {
				return err
			}

			// Read the current version immediately before the PUT so the
			// precondition protects against concurrent collection edits. This stays
			// outside the gated Apply closure (mirrors collections update): it is
			// not a change to record, and a failed read must abort with its own
			// classifyAPIError result, not the engine's generic "mutation incomplete".
			_, version, err := c.GetWithVersion(path, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}

			// Only the PUT itself is the write; routing just this call through the
			// mutation engine journals it and (best-effort) mirror-replays it.
			var data json.RawMessage
			var statusCode int
			var applyErr error
			ops := []mutation.Op{{
				ID:   "collections.move:" + args[0],
				Key:  args[0],
				Kind: "collection_move",
				// Map, not string, on purpose: the item-keyed mirror must never
				// replay a collection key onto an item, and "collection" isn't in
				// reverse.go's reversibleFields, so undo correctly refuses it.
				Changes: []mutation.Change{{Field: "collection", Add: map[string]any{"parentCollection": parentCollection}}},
				Apply: func() (string, any, error) {
					var (
						status string
						detail any
						putErr error
					)
					data, statusCode, status, detail, putErr = putWithVersionGuard(c, path, map[string]any{"parentCollection": parentCollection}, version)
					if putErr != nil {
						applyErr = putErr
					}
					return status, detail, putErr
				},
			}}
			if _, runErr := runMutation(cmd.Context(), flags, "collections.move", ops); runErr != nil {
				if applyErr != nil {
					return applyErr
				}
				return runErr
			}

			envelope := map[string]any{
				"action":   "put",
				"resource": "collections",
				"path":     path,
				"status":   statusCode,
				"success":  statusCode >= 200 && statusCode < 300,
			}
			if len(data) > 0 {
				var parsed any
				if json.Unmarshal(data, &parsed) == nil {
					envelope["data"] = parsed
				}
			}
			return printJSONFiltered(cmd.OutOrStdout(), envelope, flags)
		},
	}
	cmd.Flags().StringVar(&flagTo, "to", "", "New parent collection key (use root or empty string for top-level)")

	return cmd
}
