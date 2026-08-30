// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	// Direct batch creates can route through the desktop connector.
	"zotio/internal/connector"
	"zotio/internal/mutation"
)

func newItemsCreateCmd(flags *rootFlags) *cobra.Command {
	var bodyItems string
	var stdinBody bool

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create one or more items",
		Long: `Create one or more items from JSON supplied with --items or --stdin.

This command shares one connector session across the invocation when routed
through --via connector; the Zotero desktop surfaces its own progress UI and
zotio cannot dismiss it — no connector endpoint closes or completes a save
session. Observed 2026-08-22: roughly 78 consecutive one-per-item
invocations left Zotero unresponsive with progress windows accumulating; no
proven mechanism has been established. Prefer one invocation with many items:
import file --via connector and this command share one session, while import
apply currently opens one per manifest entry. --rate-limit governs only Web
API requests and does not pace connector calls.`,
		Example:     "  zotio items create",
		Annotations: map[string]string{"zotio:endpoint": "items.create", "zotio:method": "POST", "zotio:path": "/items"},
		RunE: func(cmd *cobra.Command, args []string) error {

			path := "/items"
			// Zotero's POST /items requires a bare JSON array of item objects.
			// The generated shape wrapped it as {"items": [...]}, which the API rejects
			// ("Uploaded data must be a JSON array"). Send the array directly, and accept
			// either an array or an object from stdin.
			var body any
			if stdinBody {
				// cmd.InOrStdin(), not os.Stdin: under a stdio MCP server os.Stdin IS
				// the JSON-RPC transport, so a model-issued --stdin would consume the
				// protocol stream and hang the session.
				stdinData, err := io.ReadAll(cmd.InOrStdin())
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
			if !resolveMutationMode(flags).Apply {
				env, runErr := runMutation(cmd.Context(), flags, "items.create", []mutation.Op{{
					ID:      "items.create",
					Kind:    "item_create",
					Changes: []mutation.Change{{Field: "items", Add: body}},
				}})
				if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
					return renderErr
				}
				return runErr
			}
			// CheckGates counts operations, not the Changes inside them, so a
			// single batched Op (one Op, N Changes) would charge an N-item array
			// as 1 against --max-changes. itemsCreatePreflightOps charges one op
			// per item instead, before any network call -- this also covers the
			// connector branch below, which writes via conn.SaveItems and never
			// reaches the journaled ops' gate check. The same per-item ops feed
			// the real write below, so the preflight count and the applied count
			// always agree.
			ops := itemsCreatePreflightOps(body)
			if gateFailure := mutation.CheckGates(mutationOptions(flags), ops); gateFailure != nil {
				return fmt.Errorf("%s", gateFailure.Message)
			}
			c, err := flags.newClient()
			if err != nil {
				return err
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
					// Zotero's connector can succeed at SaveItems and then fail
					// at the follow-up UpdateSession target filing. There is no
					// transaction across the two calls; returning bare err here
					// would make a successfully created batch look like a total
					// failure and invite a duplicating retry. Preserve the
					// correlation and surface the filing error separately.
					createdAfter := time.Now().UTC().Add(-recentItemClockSkew)
					if err := conn.SaveItems(cmd.Context(), sessionID, "", items); err != nil {
						return err
					}
					if target := strings.TrimSpace(flags.connectorTarget); target != "" {
						if err := conn.UpdateSession(cmd.Context(), sessionID, target, nil, ""); err != nil {
							// SaveItems committed; recover best-effort WebKeys so
							// the caller can correlate instead of blindly retrying.
							// Refresh the mirror so local reads see the new items
							// even though filing failed.
							// This batch path runs OUTSIDE the mutation engine and
							// therefore remains UNJOURNALLED; the JSON response
							// below carries the committed keys structurally so a
							// retry can file without re-creating.
							refreshItemsFromLocalAPI(cmd.Context(), flags)
							var recovered []string
							for _, it := range items {
								if k, _, _ := confirmConnectorCreate(cmd.Context(), flags, it, createdAfter); k != "" {
									recovered = append(recovered, k)
								}
							}
							msg := fmt.Sprintf("created %d item(s) via connector (session %s) but target %q filing failed: %v; retry filing only, do not re-create the item", len(items), sessionID, target, err)
							if len(recovered) > 0 {
								msg = fmt.Sprintf("created %d item(s) via connector (session %s, keys %v) but target %q filing failed: %v; retry filing only, do not re-create the item", len(items), sessionID, recovered, target, err)
							}
							if flags.asJSON || flags.agent {
								payload, mErr := json.Marshal(map[string]any{
									"via":           "connector",
									"status":        "created",
									"count":         len(items),
									"keys":          recovered,
									"session":       sessionID,
									"target":        target,
									"filing_failed": true,
									"filing_error":  err.Error(),
									"message":       msg,
								})
								if mErr == nil {
									_ = printOutput(cmd.OutOrStdout(), json.RawMessage(payload), true)
								}
							}
							if len(recovered) > 0 {
								return fmt.Errorf("created %d item(s) via connector (session %s, keys %v) but target %q filing failed: %w; retry filing only, do not re-create the item", len(items), sessionID, recovered, target, err)
							}
							return fmt.Errorf("created %d item(s) via connector (session %s) but target %q filing failed: %w (items remain; retry filing, not creation)", len(items), sessionID, target, err)
						}
					}
					refreshItemsFromLocalAPI(cmd.Context(), flags)
					if flags.asJSON || flags.agent {
						// Best-effort: include recovered WebKeys structurally instead
						// of hardcoding key:nil. If resolution races the desktop,
						// keys may be empty but session is still present.
						var recovered []string
						for _, it := range items {
							if k, _, _ := confirmConnectorCreate(cmd.Context(), flags, it, createdAfter); k != "" {
								recovered = append(recovered, k)
							}
						}
						m := map[string]any{"via": "connector", "status": "created", "count": len(items), "keys": recovered, "session": sessionID}
						if len(recovered) == 1 {
							m["key"] = recovered[0]
						} else if len(recovered) > 1 {
							m["key"] = recovered
						} else {
							m["key"] = nil
						}
						payload, err := json.Marshal(m)
						if err != nil {
							return err
						}
						return printOutput(cmd.OutOrStdout(), json.RawMessage(payload), true)
					}
					fmt.Fprintln(cmd.OutOrStdout(), "Created in desktop Zotero (key assigned on save; syncs on next sync).")
					return nil
				}
			}
			// The Web API create is the only write below. Zotero answers a
			// batched POST with HTTP 200 even when it rejects some elements, and
			// the elements it did not reject were still created -- so wrapping
			// the whole batch in one mutation.Op (as before) let a single
			// rejected element erase the journal record of everything created
			// alongside it (recordMutationJournal skips runs with Applied == 0).
			// One Op per item fixes that: the first item's Apply issues the
			// single batched POST and caches the decoded response, and every
			// other item reads its outcome from that cache, so the HTTP request
			// count is unchanged. Key stays empty on each item op: the item
			// doesn't exist yet, so there is nothing for write-through to key a
			// mirror update on.
			var (
				data           json.RawMessage
				statusCode     int
				applyErr       error // aggregate failure returned as the command's own error
				batchExecuted  bool
				batchTransport error // set only when the POST itself failed (not a per-element rejection)
				batchFailed    map[string]batchWriteFailure
			)
			applyItem := func(index int) (string, any, error) {
				if !batchExecuted {
					batchExecuted = true
					var postErr error
					data, statusCode, postErr = c.Post(path, body)
					if postErr != nil {
						batchTransport = classifyAPIError(postErr, flags)
						applyErr = batchTransport
					} else {
						batchFailed = decodeBatchWriteResponse(data).Failed
						if bwErr := batchWriteFailuresError("items create", batchFailed); bwErr != nil {
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
					return applyItem(index)
				}
			}
			// A rejected element travelled to Zotero alongside its batch-mates
			// and cannot un-submit them, so stopping at the first rejection
			// would report the rest as not_attempted -- a lie that invites a
			// duplicating re-create. Every item reports its own outcome.
			if _, runErr := runMutation(cmd.Context(), flags, "items.create", ops, func(o *mutation.Options) {
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

// itemsCreatePreflightOps builds one mutation.Op per item in a JSON object
// array body, mirroring collectionCreateOps (collections_create.go) and the
// per-record preflight pattern used before other batched writes. Beyond
// running mutation.CheckGates before any network call -- CheckGates counts
// operations, not the Changes within them, so without this an N-item array
// would charge as a single planned op no matter how large N is -- these same
// ops carry the real write's per-item Apply closures below, so the pre-write
// gate and the journaled result always count the same N. Non-object-array
// bodies (e.g. a single object from --stdin) still charge, and apply, as one
// op.
func itemsCreatePreflightOps(body any) []mutation.Op {
	items, ok := itemsCreateObjects(body)
	if !ok {
		return []mutation.Op{{
			ID:      "items.create:1",
			Kind:    "item_create",
			Changes: []mutation.Change{{Field: "item", Add: body}},
		}}
	}
	ops := make([]mutation.Op, 0, len(items))
	for i, item := range items {
		key := fmt.Sprintf("%d", i+1)
		if title, ok := item["title"].(string); ok && title != "" {
			key = title
		}
		ops = append(ops, mutation.Op{
			ID:      fmt.Sprintf("items.create:%d", i+1),
			Key:     key,
			Kind:    "item_create",
			Changes: []mutation.Change{{Field: "item", Add: item}},
		})
	}
	return ops
}
