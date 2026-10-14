// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

func newCollectionsMoveCmd(flags *rootFlags) *cobra.Command {
	var flagTo string

	cmd := &cobra.Command{
		Use:     "move <collectionKey> --to <parentKey>",
		Short:   "Move a collection under a new parent",
		Example: "  zotio collections move ABCD2345 --to EFGH6789\n  zotio collections move ABCD2345 --to root",
		Annotations: map[string]string{
			"zotio:method": "PUT",
			"zotio:path":   "/collections/{collectionKey}",
		},
		// One key per run. Cobra's default for a command with no subcommands is
		// ArbitraryArgs, so `collections move K1 K2 --to P` used to move K1 and
		// drop K2 on the floor: the path and the whole envelope are built from
		// args[0] alone, and nothing reported the ignored keys. A missing key is
		// a usage error, not a help request that exits 0.
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Flag checks run before the preview branch, so a preview never
			// succeeds for input the apply refuses.
			if !cmd.Flags().Changed("to") {
				return usageErr(fmt.Errorf("required flag %q not set", "to"))
			}
			parentCollection := any(flagTo)
			if flagTo == "" || flagTo == "root" {
				parentCollection = false
			} else if err := validateKeyArg("collection", flagTo); err != nil {
				return err
			}
			path := replacePathParam("/collections/{collectionKey}", "collectionKey", args[0])
			body := map[string]any{"parentCollection": parentCollection}

			// Preview and apply run the same reads: the collection (and a new
			// parent) must exist on the write plane, and the collection's
			// version becomes the plan's expected version and the apply's
			// If-Unmodified-Since-Version guard.
			c, err := flags.newWriteClient()
			if err != nil {
				return err
			}
			version, err := readCollectionVersion(c, flags, args[0])
			if err != nil {
				return err
			}
			if s, ok := parentCollection.(string); ok {
				if _, err := readCollectionVersion(c, flags, s); err != nil {
					return err
				}
			}
			ops := []mutation.Op{{
				ID:              "collections.move:" + args[0],
				Key:             args[0],
				Kind:            "collection_move",
				ExpectedVersion: version,
				// Map, not string, on purpose: the item-keyed mirror must never
				// replay a collection key onto an item, and "collection" isn't in
				// reverse.go's reversibleFields, so undo correctly refuses it.
				Changes: []mutation.Change{{Field: "collection", Add: body}},
			}}
			var applyErr error
			// Only the PUT itself is the write; routing just this call through
			// the mutation engine journals it and (best-effort) mirror-replays it.
			ops[0].Apply = func() (string, any, error) {
				_, _, status, detail, putErr := putWithVersionGuard(c, path, body, version)
				if putErr != nil {
					applyErr = putErr
				}
				return status, detail, putErr
			}
			env, runErr := runMutation(cmd.Context(), flags, "collections.move", ops)
			return renderMutationResult(cmd, flags, env, preferApplyErr(runErr, applyErr), collectionWriteLine("move", "moved"))
		},
	}
	cmd.Flags().StringVar(&flagTo, "to", "", "New parent collection key (use root or empty string for top-level)")

	return cmd
}
