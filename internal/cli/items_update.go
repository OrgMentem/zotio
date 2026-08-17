// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

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
			// envelope and applied runs expose journal.run_id. The precondition
			// is resolved on the write plane at apply time via the shared
			// helpers, so the PATCH never carries a stale plan-time version.
			var writeClient *client.Client
			var expectedVersion int
			if resolveMutationMode(flags).Apply {
				var err error
				writeClient, err = flags.newWriteClient()
				if err != nil {
					return err
				}
				if v, hasVersion := body["version"]; hasVersion {
					expectedVersion = mutationExpectedVersion(v)
				} else {
					_, v, err := writeClient.GetFromWriteBaseWithVersionContext(cmd.Context(), path, nil)
					if err != nil {
						return classifyAPIError(err, flags)
					}
					if v <= 0 {
						return apiErr(fmt.Errorf("reading item version for %s: response did not include a version", args[0]))
					}
					expectedVersion = v
				}
			}

			var applyErr error
			// Capture by value for the closure.
			bodyCopy := body
			pathCopy := path
			ops := []mutation.Op{{
				ID:              "items.update",
				Key:             args[0],
				Kind:            "item_update",
				ExpectedVersion: expectedVersion,
				Changes:         itemsUpdateChanges(body),
				Apply: func() (string, any, error) {
					if writeClient == nil {
						return "failed", "no write client", fmt.Errorf("no write client")
					}
					return applyItemsUpdateWithContext(cmd.Context(), writeClient, pathCopy, bodyCopy, &applyErr, flags)
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

// applyItemsUpdateWithContext executes the PATCH under a write-plane
// precondition. When the caller supplied an explicit body version, that
// version is sent as If-Unmodified-Since-Version via the shared guard;
// otherwise the version is re-read from the write plane at apply time so a
// concurrent edit between plan and apply is detected. 412/428 maps to
// "conflict" via classifyWriteError (called inside the helpers) rather than
// a generic "failed".
func applyItemsUpdateWithContext(ctx context.Context, c *client.Client, path string, body map[string]any, applyErr *error, flags *rootFlags) (string, any, error) {
	if c == nil {
		err := fmt.Errorf("no write client")
		if applyErr != nil {
			*applyErr = err
		}
		return "failed", "no write client", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_ = flags
	if v, hasVersion := body["version"]; hasVersion {
		ver := mutationExpectedVersion(v)
		payload := make(map[string]any, len(body))
		for k, val := range body {
			if k == "version" {
				continue
			}
			payload[k] = val
		}
		status, reason, err := patchWithVersionGuard(c, path, payload, ver)
		if applyErr != nil && err != nil {
			*applyErr = err
		} else if applyErr != nil && status != "applied" {
			if e, ok := reason.(error); ok {
				*applyErr = e
			} else if s, ok := reason.(string); ok && s != "" {
				*applyErr = fmt.Errorf("%s", s)
			}
		}
		return status, reason, err
	}
	status, reason, err := patchWithWritePlaneVersion(ctx, c, path, body)
	if applyErr != nil && err != nil {
		*applyErr = err
	}
	return status, reason, err
}
