// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

func newCollectionsCreateCmd(flags *rootFlags) *cobra.Command {
	var bodyName string
	var bodyParentCollection string
	var stdinBody bool

	cmd := &cobra.Command{
		Use:     "create",
		Short:   "Create one or more collections",
		Example: "  zotio collections create --name example-resource",
		Annotations: map[string]string{
			"zotio:endpoint":                   "collections.create",
			"zotio:method":                     "POST",
			"zotio:path":                       "/collections",
			"zotio:destructive":                "false",
			"zotio:supports-dry-run":           "true",
			"zotio:requires-allow-destructive": "false",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if !stdinBody {
				if !cmd.Flags().Changed("name") && !flags.dryRun {
					return fmt.Errorf("required flag \"%s\" not set", "name")
				}
			}

			path := "/collections"
			var body any
			if stdinBody {
				stdinData, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("reading stdin: %w", err)
				}
				var jsonBody any
				if err := json.Unmarshal(stdinData, &jsonBody); err != nil {
					return fmt.Errorf("parsing stdin JSON: %w", err)
				}
				switch payload := jsonBody.(type) {
				case map[string]any:
					body = []map[string]any{payload}
				case []any:
					body = payload
				default:
					return fmt.Errorf("parsing stdin JSON: collection payload must be an object or array of objects")
				}
			} else {
				collection := map[string]any{}
				if bodyName != "" {
					collection["name"] = bodyName
				}
				if bodyParentCollection != "" {
					collection["parentCollection"] = bodyParentCollection
				}
				body = []map[string]any{collection}
			}
			ops := collectionCreateOps(body)
			// Preview unless the caller explicitly applied. The payload is built
			// first so the preview names every collection apply would create.
			if mode := resolveMutationMode(flags); !mode.Apply {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"action":         "create",
					"resource":       "collections",
					"path":           path,
					"body":           body,
					"status":         0,
					"success":        false,
					"dry_run":        true,
					"preview_reason": mode.PreviewReason,
				}, flags)
			}
			if gateFailure := mutation.CheckGates(mutationOptions(flags), ops); gateFailure != nil {
				return fmt.Errorf("%s", gateFailure.Message)
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			// The Web API create is the only write below. Zotero answers a
			// batched POST with HTTP 200 even when it rejects some elements, and
			// the elements it did not reject were still created -- so wrapping
			// the whole batch in one mutation.Op (as before) let a single
			// rejected element erase the journal record of everything created
			// alongside it (recordMutationJournal skips runs with Applied == 0).
			// One Op per collection fixes that: the first collection's Apply
			// issues the single batched POST and caches the decoded response,
			// and every other collection reads its outcome from that cache, so
			// the HTTP request count is unchanged. A non-string Add means
			// applyChangeToItemData always refuses the replay -- a collection
			// key must never land on an item.
			var (
				data           json.RawMessage
				statusCode     int
				applyErr       error // aggregate failure returned as the command's own error
				batchExecuted  bool
				batchTransport error // set only when the POST itself failed (not a per-element rejection)
				batchFailed    map[string]batchWriteFailure
			)
			applyCollection := func(index int) (string, any, error) {
				if !batchExecuted {
					batchExecuted = true
					var postErr error
					data, statusCode, postErr = c.Post(path, body)
					if postErr != nil {
						batchTransport = classifyAPIError(postErr, flags)
						applyErr = batchTransport
					} else {
						batchFailed = decodeBatchWriteResponse(data).Failed
						if bwErr := batchWriteFailuresError("collections create", batchFailed); bwErr != nil {
							applyErr = degradedErr(bwErr)
						}
					}
				}
				if batchTransport != nil {
					return "failed", nil, batchTransport
				}
				if failure, ok := batchFailed[strconv.Itoa(index)]; ok {
					return "failed", fmt.Sprintf("index %d: code %d: %s", index, failure.Code, failure.Message), nil
				}
				return "applied", nil, nil
			}
			for i := range ops {
				index := i
				ops[i].Apply = func() (string, any, error) {
					return applyCollection(index)
				}
			}
			// A rejected element travelled to Zotero alongside its batch-mates
			// and cannot un-submit them, so stopping at the first rejection
			// would report the rest as not_attempted -- a lie that invites a
			// duplicating re-create. Every collection reports its own outcome.
			if _, runErr := runMutation(cmd.Context(), flags, "collections.create", ops, func(o *mutation.Options) {
				o.ContinueOnError = true
			}); runErr != nil {
				// applyErr already carries the command's own classification
				// (classifyAPIError / degradedErr); only fall back to the engine's
				// generic error when Apply never ran (e.g. a gate rejected the op).
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
					"action":   "post",
					"resource": "collections",
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
	cmd.Flags().StringVar(&bodyName, "name", "", "Collection name")
	cmd.Flags().StringVar(&bodyParentCollection, "parent-collection", "", "Parent collection key (omit for top-level)")
	cmd.Flags().BoolVar(&stdinBody, "stdin", false, "Read request body as JSON from stdin")

	return cmd
}

func collectionCreateOps(body any) []mutation.Op {
	var collections []any
	switch payload := body.(type) {
	case []any:
		collections = payload
	case []map[string]any:
		collections = make([]any, len(payload))
		for i := range payload {
			collections[i] = payload[i]
		}
	default:
		return nil
	}

	ops := make([]mutation.Op, 0, len(collections))
	for i, collection := range collections {
		key := fmt.Sprintf("%d", i+1)
		if fields, ok := collection.(map[string]any); ok {
			if name, ok := fields["name"].(string); ok && name != "" {
				key = name
			}
		}
		ops = append(ops, mutation.Op{
			ID:      fmt.Sprintf("collections.create:%d", i+1),
			Key:     key,
			Kind:    "collection_create",
			Changes: []mutation.Change{{Field: "collection", Add: collection}},
		})
	}
	return ops
}
