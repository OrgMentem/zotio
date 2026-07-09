// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	// Direct batch creates can route through the desktop connector.
	"zotio/internal/connector"
)

func newItemsCreateCmd(flags *rootFlags) *cobra.Command {
	var bodyItems string
	var stdinBody bool

	cmd := &cobra.Command{
		Use:         "create",
		Short:       "Create one or more items",
		Example:     "  zotio items create",
		Annotations: map[string]string{"zotio:endpoint": "items.create", "zotio:method": "POST", "zotio:path": "/items"},
		RunE: func(cmd *cobra.Command, args []string) error {

			c, err := flags.newClient()
			if err != nil {
				return err
			}

			path := "/items"
			// Zotero's POST /items requires a bare JSON array of item objects.
			// The generated shape wrapped it as {"items": [...]}, which the API rejects
			// ("Uploaded data must be a JSON array"). Send the array directly, and accept
			// either an array or an object from stdin.
			var body any
			if stdinBody {
				stdinData, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("reading stdin: %w", err)
				}
				var jsonBody any
				if err := json.Unmarshal(stdinData, &jsonBody); err != nil {
					return fmt.Errorf("parsing stdin JSON: %w", err)
				}
				body = jsonBody
			} else if bodyItems != "" {
				var parsedItems any
				if err := json.Unmarshal([]byte(bodyItems), &parsedItems); err != nil {
					return fmt.Errorf("parsing --items JSON: %w", err)
				}
				body = parsedItems
			}
			// Batch item creates use the desktop connector when the body is a JSON object array.
			if via, err := flags.resolveCreateVia(cmd.Context(), false); err != nil {
				return err
			} else if via == "connector" {
				items, ok := itemsCreateObjects(body)
				if ok {
					// Connector sessions have one target; preserve per-item collection arrays via Web API unless caller overrides.
					if itemsCreateHasCollections(items) && strings.TrimSpace(flags.connectorTarget) == "" {
						if flags.via == "connector" {
							return fmt.Errorf("--via connector cannot honor per-item collections in items create; use --via web or --connector-target C<n>")
						}
						ok = false
					}
				}
				if ok {
					if flags.dryRun {
						payload, err := json.Marshal(map[string]any{"dry_run": true, "via": "connector", "status": "planned", "count": len(items)})
						if err != nil {
							return err
						}
						return printOutput(cmd.OutOrStdout(), json.RawMessage(payload), true)
					}
					conn, err := flags.newConnector()
					if err != nil {
						return err
					}
					sessionID, err := connector.NewID()
					if err != nil {
						return err
					}
					for _, item := range items {
						connectorKey, err := connector.NewID()
						if err != nil {
							return err
						}
						item["id"] = connectorKey
					}
					if err := conn.SaveItems(cmd.Context(), sessionID, "", items); err != nil {
						return err
					}
					if target := strings.TrimSpace(flags.connectorTarget); target != "" {
						if err := conn.UpdateSession(cmd.Context(), sessionID, target, nil, ""); err != nil {
							return err
						}
					}
					refreshItemsFromLocalAPI(cmd.Context(), flags)
					if flags.asJSON || flags.agent {
						payload, err := json.Marshal(map[string]any{"via": "connector", "status": "created", "key": nil, "count": len(items)})
						if err != nil {
							return err
						}
						return printOutput(cmd.OutOrStdout(), json.RawMessage(payload), true)
					}
					fmt.Fprintln(cmd.OutOrStdout(), "Created in desktop Zotero (key assigned on save; syncs on next sync).")
					return nil
				}
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
	cmd.Flags().StringVar(&bodyItems, "items", "", "Array of item objects to create (use item-types and item-type-fields for schema)")
	cmd.Flags().BoolVar(&stdinBody, "stdin", false, "Read request body as JSON from stdin")

	return cmd
}

// Direct batch create connector routing requires object-array inspection.
func itemsCreateObjects(body any) ([]map[string]any, bool) {
	rawItems, ok := body.([]any)
	if !ok || len(rawItems) == 0 {
		return nil, false
	}
	items := make([]map[string]any, 0, len(rawItems))
	for _, raw := range rawItems {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, false
		}
		items = append(items, item)
	}
	return items, true
}

func itemsCreateHasCollections(items []map[string]any) bool {
	for _, item := range items {
		if connectorCollectionKeyFromItem(item) != "" {
			return true
		}
	}
	return false
}
