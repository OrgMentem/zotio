// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH: Add hand-written collection move workflow missing from the generated CLI.

package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
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

			if flags.dryRun {
				if flags.quiet {
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Would move collection %s under parent %s\n", args[0], flagTo)
				return nil
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}

			parentCollection := any(flagTo)
			if flagTo == "" || flagTo == "root" {
				parentCollection = false
			}
			path := replacePathParam("/collections/{collectionKey}", "collectionKey", args[0])
			data, statusCode, err := c.Put(path, map[string]any{"parentCollection": parentCollection})
			if err != nil {
				return classifyAPIError(err, flags)
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
