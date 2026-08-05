// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"

	"github.com/spf13/cobra"

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
				stdinData, err := io.ReadAll(os.Stdin)
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

			// Preview by default; apply only with --yes (or the resolved apply mode).
			if mode := resolveMutationMode(flags); !mode.Apply {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"action":         "update",
					"resource":       "items",
					"key":            args[0],
					"body":           body,
					"status":         0,
					"success":        false,
					"dry_run":        true,
					"preview_reason": mode.PreviewReason,
				}, flags)
			}

			// Route through the write target and supply the version precondition Zotero
			// requires for key-based writes (PATCH returns HTTP 428 without
			// If-Unmodified-Since-Version). Mirrors items delete; the version GET and
			// the PATCH hit the same library, so an item created on the web but not yet
			// synced locally still resolves. An explicit version in a --stdin body is respected.
			c, err := flags.newWriteClient()
			if err != nil {
				return err
			}
			patchHeaders := map[string]string{}
			if _, hasVersion := body["version"]; !hasVersion {
				_, version, err := c.GetWithVersion(path, nil)
				if err != nil {
					return classifyAPIError(err, flags)
				}
				if version <= 0 {
					return apiErr(fmt.Errorf("reading item version for %s: response did not include a version", args[0]))
				}
				patchHeaders["If-Unmodified-Since-Version"] = strconv.Itoa(version)
			}

			// Only the PATCH itself is the write; routing just this call through the
			// mutation engine journals it and mirror-replays it into the local
			// SQLite cache for read-your-writes. The version precondition above stays
			// outside the gated path since it is not a change to record and a failed
			// read must abort with its own classifyAPIError result, not the engine's
			// generic "mutation incomplete".
			var data json.RawMessage
			var statusCode int
			var applyErr error
			ops := []mutation.Op{{
				ID:      "items.update",
				Key:     args[0],
				Kind:    "item_update",
				Changes: itemsUpdateChanges(body),
				Apply: func() (string, any, error) {
					var patchErr error
					data, statusCode, patchErr = c.PatchWithHeaders(path, body, patchHeaders)
					if patchErr != nil {
						applyErr = classifyAPIError(patchErr, flags)
						return "failed", nil, applyErr
					}
					return "applied", nil, nil
				},
			}}
			if _, runErr := runMutation(cmd.Context(), flags, "items.update", ops); runErr != nil {
				if applyErr != nil {
					return applyErr
				}
				return runErr
			}
			if wantsHumanTable(cmd.OutOrStdout(), flags) {
				// Check if response contains an array (directly or wrapped in "data")
				var items []map[string]any
				if json.Unmarshal(data, &items) == nil && len(items) > 0 {
					if err := printAutoTable(cmd.OutOrStdout(), items); err != nil {
						fmt.Fprintf(os.Stderr, "warning: table rendering failed, falling back to JSON: %v\n", err)
					} else {
						return nil
					}
				} else {
					var wrapped struct {
						Data []map[string]any `json:"data"`
					}
					if json.Unmarshal(data, &wrapped) == nil && len(wrapped.Data) > 0 {
						if err := printAutoTable(cmd.OutOrStdout(), wrapped.Data); err != nil {
							fmt.Fprintf(os.Stderr, "warning: table rendering failed, falling back to JSON: %v\n", err)
						} else {
							return nil
						}
					}
				}
			}
			if flags.asJSON || !isTerminal(cmd.OutOrStdout()) {
				if flags.quiet {
					return nil
				}
				// Apply --compact and --select to the API response before wrapping.
				// --select wins when both are set: explicit field choice trumps the
				// generic high-gravity allow-list. Otherwise --compact still applies
				// when --agent is on but the user did not name fields.
				filtered := data
				if flags.selectFields != "" {
					filtered = filterFields(filtered, flags.selectFields)
				} else if flags.compact {
					filtered = compactFields(filtered)
				}
				envelope := map[string]any{
					"action":   "patch",
					"resource": "items",
					"path":     path,
					"status":   statusCode,
					"success":  statusCode >= 200 && statusCode < 300,
				}
				if flags.dryRun {
					envelope["dry_run"] = true
					envelope["status"] = 0
					envelope["success"] = false
				}
				if len(filtered) > 0 {
					var parsed any
					if err := json.Unmarshal(filtered, &parsed); err == nil {
						envelope["data"] = parsed
					}
				}
				envelopeJSON, err := json.Marshal(envelope)
				if err != nil {
					return err
				}
				return printOutput(cmd.OutOrStdout(), json.RawMessage(envelopeJSON), true)
			}
			return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
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
