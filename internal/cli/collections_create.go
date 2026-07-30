// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

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
				return gateErr(fmt.Errorf("%s", gateFailure.Message))
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			data, statusCode, err := c.Post(path, body)
			if err != nil {
				return classifyAPIError(err, flags)
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
