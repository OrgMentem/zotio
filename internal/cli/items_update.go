// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/mutation"
)

func newItemsUpdateCmd(flags *rootFlags) *cobra.Command {
	var bodyTitle string
	var bodyAbstractNote string
	var bodyTags string
	var bodyCollections string
	var bodyExtra string
	var stdinBody bool

	cmd := &cobra.Command{
		Use:   "update <itemKey>",
		Short: "Update a specific item",
		// Use an item-key placeholder, not an API-token placeholder.
		Example:     "  zotio items update ABCD1234 --title \"Updated title\"",
		Annotations: map[string]string{"zotio:endpoint": "items.update", "zotio:method": "PATCH", "zotio:path": "/items/{itemKey}"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}

			// replacePathParam percent-encodes the key as a single path segment;
			// pre-escaping here would double-encode it.
			path := replacePathParam("/items/{itemKey}", "itemKey", args[0])
			var body map[string]any
			if stdinBody {
				stdinData, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("reading stdin: %w", err)
				}
				var jsonBody map[string]any
				if err := json.Unmarshal(stdinData, &jsonBody); err != nil {
					return fmt.Errorf("parsing stdin JSON: %w", err)
				}
				body = jsonBody
			} else {
				body = map[string]any{}
				if bodyTitle != "" {
					body["title"] = bodyTitle
				}
				if bodyAbstractNote != "" {
					body["abstractNote"] = bodyAbstractNote
				}
				if bodyTags != "" {
					var parsedTags any
					if err := json.Unmarshal([]byte(bodyTags), &parsedTags); err != nil {
						return fmt.Errorf("parsing --tags JSON: %w", err)
					}
					body["tags"] = parsedTags
				}
				if bodyCollections != "" {
					var parsedCollections any
					if err := json.Unmarshal([]byte(bodyCollections), &parsedCollections); err != nil {
						return fmt.Errorf("parsing --collections JSON: %w", err)
					}
					body["collections"] = parsedCollections
				}
				if bodyExtra != "" {
					body["extra"] = bodyExtra
				}
			}

			// Route through the mutation engine so preview/apply share one stable
			// envelope and applied runs expose journal.run_id. The precondition GET
			// remains outside Apply and still comes from the write plane.
			var writeClient *client.Client
			if resolveMutationMode(flags).Apply {
				var err error
				writeClient, err = flags.newWriteClient()
				if err != nil {
					return err
				}
			}
			patchHeaders := map[string]string{}
			if _, hasVersion := body["version"]; !hasVersion && writeClient != nil {
				_, version, err := writeClient.GetWithVersion(path, nil)
				if err != nil {
					return classifyAPIError(err, flags)
				}
				if version <= 0 {
					return apiErr(fmt.Errorf("reading item version for %s: response did not include a version", args[0]))
				}
				patchHeaders["If-Unmodified-Since-Version"] = strconv.Itoa(version)
			}

			var applyErr error
			ops := []mutation.Op{{
				ID:      "items.update",
				Key:     args[0],
				Kind:    "item_update",
				Changes: itemsUpdateChanges(body),
				Apply: func() (string, any, error) {
					if writeClient == nil {
						return "failed", "no write client", fmt.Errorf("no write client")
					}
					_, statusCode, err := writeClient.PatchWithHeaders(path, body, patchHeaders)
					if err != nil {
						applyErr = classifyAPIError(err, flags)
						return "failed", nil, applyErr
					}
					if statusCode < 200 || statusCode >= 300 {
						applyErr = apiErr(fmt.Errorf("update returned HTTP %d", statusCode))
						return "failed", nil, applyErr
					}
					return "applied", nil, nil
				},
			}}
			env, runErr := runMutation(cmd.Context(), flags, "items.update", ops)
			if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
				return renderErr
			}
			if runErr != nil {
				if applyErr != nil {
					return applyErr
				}
				return runErr
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&bodyTitle, "title", "", "Item title")
	cmd.Flags().StringVar(&bodyAbstractNote, "abstract-note", "", "Abstract or summary")
	cmd.Flags().StringVar(&bodyTags, "tags", "", "Tags to apply (array of {tag: string} objects)")
	cmd.Flags().StringVar(&bodyCollections, "collections", "", "Collection keys to assign this item to")
	cmd.Flags().StringVar(&bodyExtra, "extra", "", "Extra notes field (also stores Better BibTeX citation key)")
	cmd.Flags().BoolVar(&stdinBody, "stdin", false, "Read request body as JSON from stdin")

	return cmd
}

// itemsUpdateChanges derives one mutation.Change per field the update body
// actually sets, so runMutation can journal and mirror-replay each edit.
// tags/collections REPLACE the whole list rather than toggling membership, so
// they use the "_set" field names: Field:"tags" would let journal undo invert
// a full-list replace into a bogus per-tag removal, and mirror write-through
// would try to merge the slice as a single tag/collection name. version is a
// write precondition, not a user-visible change, so it is never emitted.
func itemsUpdateChanges(body map[string]any) []mutation.Change {
	keys := make([]string, 0, len(body))
	for k := range body {
		if k == "version" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	changes := make([]mutation.Change, 0, len(keys))
	for _, k := range keys {
		field := k
		switch k {
		case "tags":
			field = "tags_set"
		case "collections":
			field = "collections_set"
		}
		changes = append(changes, mutation.Change{Field: field, Add: body[k]})
	}
	return changes
}
