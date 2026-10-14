// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

func newCollectionsUpdateCmd(flags *rootFlags) *cobra.Command {
	var bodyName string
	var bodyParentCollection string
	var stdinBody bool

	cmd := &cobra.Command{
		Use:     "update <collectionKey>",
		Short:   "Update a collection",
		Example: "  zotio collections update ABCD2345 --name \"Reading list\"",
		Annotations: map[string]string{
			"zotio:endpoint": "collections.update",
			"zotio:method":   "PUT",
			"zotio:path":     "/collections/{collectionKey}",
		},
		// One key per run. Cobra's default for a command with no subcommands is
		// ArbitraryArgs, so `collections update K1 K2` used to update K1 and drop
		// K2 on the floor: the path, the op id and the whole envelope are built
		// from args[0] alone, and nothing reported the ignored keys. An update
		// that reports one collection as the whole result must not silently do
		// less than it was asked, and a missing key is a usage error, not a help
		// request that exits 0.
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := replacePathParam("/collections/{collectionKey}", "collectionKey", args[0])
			// Every flag and payload check runs before the preview branch, so a
			// preview never succeeds for input the apply refuses.
			body, err := collectionUpdateBody(cmd, bodyName, bodyParentCollection, stdinBody)
			if err != nil {
				return err
			}
			// Preview and apply run the same read: the collection must exist on
			// the write plane, and its version becomes the plan's expected
			// version and the apply's If-Unmodified-Since-Version guard.
			c, err := flags.newWriteClient()
			if err != nil {
				return err
			}
			version, err := readCollectionVersion(c, flags, args[0])
			if err != nil {
				return err
			}
			ops := []mutation.Op{{
				ID:              "collections.update:" + args[0],
				Key:             args[0],
				Kind:            "collection_update",
				ExpectedVersion: version,
				Changes:         []mutation.Change{{Field: "collection", Add: body}},
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
			env, runErr := runMutation(cmd.Context(), flags, "collections.update", ops)
			return renderMutationResult(cmd, flags, env, preferApplyErr(runErr, applyErr), collectionWriteLine("update", "updated"))
		},
	}
	cmd.Flags().StringVar(&bodyName, "name", "", "New collection name")
	cmd.Flags().StringVar(&bodyParentCollection, "parent-collection", "", "New parent collection key (false or empty for top-level)")
	cmd.Flags().BoolVar(&stdinBody, "stdin", false, "Read request body as JSON from stdin")

	return cmd
}

// collectionUpdateBody validates the update flags or stdin payload and returns
// the fields to PUT.
func collectionUpdateBody(cmd *cobra.Command, name, parent string, stdinBody bool) (map[string]any, error) {
	nameSet, parentSet := cmd.Flags().Changed("name"), cmd.Flags().Changed("parent-collection")
	if stdinBody {
		if nameSet || parentSet {
			return nil, usageErr(fmt.Errorf("--stdin cannot be combined with --name or --parent-collection"))
		}
		stdinData, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, fmt.Errorf("reading stdin: %w", err)
		}
		var body map[string]any
		if err := json.Unmarshal(stdinData, &body); err != nil {
			return nil, usageErr(fmt.Errorf("parsing stdin JSON: %w", err))
		}
		if len(body) == 0 {
			return nil, usageErr(fmt.Errorf("parsing stdin JSON: the update body is empty"))
		}
		return body, nil
	}
	if !nameSet && !parentSet {
		return nil, usageErr(fmt.Errorf("nothing to update: pass --name, --parent-collection, or --stdin"))
	}
	body := map[string]any{}
	if nameSet {
		if strings.TrimSpace(name) == "" {
			return nil, usageErr(fmt.Errorf("--name must not be empty"))
		}
		body["name"] = name
	}
	if parentSet {
		if parent == "" || strings.EqualFold(parent, "false") {
			body["parentCollection"] = false
		} else {
			if err := validateKeyArg("collection", parent); err != nil {
				return nil, err
			}
			body["parentCollection"] = parent
		}
	}
	return body, nil
}
