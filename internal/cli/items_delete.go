// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/mutation"
)

func newItemsDeleteCmd(flags *rootFlags) *cobra.Command {
	var permanent bool

	cmd := &cobra.Command{
		Use:   "delete <itemKey>",
		Short: "Move an item to the trash",
		Long: `Move an item to the trash, where 'zotio items restore' can bring it back.

This is a reversible trash operation (the item's "deleted" flag is set), matching
what 'zotio items trash' lists and 'zotio items restore' reverses.

--permanent instead destroys the item and its child attachments outright. That
cannot be undone by 'items restore' and requires --allow-destructive.`,
		// Use an item key placeholder, not a token.
		Example:     "  zotio items delete ABC12345",
		Annotations: map[string]string{"zotio:endpoint": "items.delete", "zotio:method": "PATCH", "zotio:path": "/items/{itemKey}"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			path := "/items/{itemKey}"
			path = replacePathParam(path, "itemKey", args[0])
			// The engine renders plan and apply in the same shape, so callers can
			// rely on .result.items[0] here as they do for move/rename/tags.
			//
			// The write client is built here, not inside Apply: a config-load
			// failure must keep its own exit code (10), and routing it through
			// Apply funnels it into classifyAPIError, which downgrades it to a
			// generic API error (5). Only apply mode needs it, so a preview still
			// requires no credentials.
			var writeClient *client.Client
			if resolveMutationMode(flags).Apply {
				var clientErr error
				writeClient, clientErr = flags.newWriteClient()
				if clientErr != nil {
					return clientErr
				}
			}
			var applyErr error
			kind := "item_trash"
			if permanent {
				kind = "item_delete"
			}
			ops := []mutation.Op{{
				ID:   "items.delete:" + args[0],
				Key:  args[0],
				Kind: kind,
				// A permanent delete cannot be undone by items restore, so it must
				// pass the --allow-destructive gate. Trashing is reversible and does not.
				Destructive: permanent,
				// Boolean on purpose: the mirror must never replay a trash, and
				// "deleted" isn't in reverse.go's reversibleFields, so undo correctly refuses it.
				Changes: []mutation.Change{{Field: "deleted", Add: true}},
				Apply: func() (string, any, error) {
					c := writeClient
					// Zotero requires If-Unmodified-Since-Version on both the DELETE
					// (HTTP 428 without it) and the trash PATCH. newWriteClient points at
					// the write target, so this version GET and the write hit the same
					// library (the Web API under hybrid routing) — correct even for an
					// item just created on the web and not yet synced local.
					//
					// A 404 here is NOT "already deleted" by default: a trashed item
					// still GETs fine with data.deleted=1, so a 404 means the key never
					// existed, was permanently destroyed, or was created moments ago
					// through the connector and has not yet propagated from the local
					// desktop up to this plane (~15-20s observed). Reporting success in
					// that window is a false success: `SDLDFA9W` was "deleted" this way
					// and then materialized on the write plane, untrashed. Fail honestly
					// instead, exactly like items tags add / items move already do on the
					// identical 404 — UNLESS the caller opted into --ignore-missing,
					// which exists precisely to accept that risk for idempotent retries.
					body, version, verErr := c.GetWithVersion(path, nil)
					if verErr != nil {
						if flags.ignoreMissing && strings.Contains(verErr.Error(), "HTTP 404") {
							return "no_op", map[string]any{
								"code":    "already_deleted",
								"message": "item does not exist on the write plane; --ignore-missing treats this as already done",
							}, nil
						}
						applyErr = verErr
						return "failed", nil, verErr
					}
					if version <= 0 {
						applyErr = apiErr(fmt.Errorf("reading item version for %s: response did not include a version", args[0]))
						return "failed", nil, applyErr
					}
					// The actual "already deleted" case: the write plane's own copy
					// already carries deleted=1. Trashing it again would be a redundant
					// PATCH — real version churn and journal noise for zero effect.
					// --permanent is exempt: destroying an already-trashed item is
					// exactly what it is for, not a no-op.
					if !permanent && itemAlreadyTrashed(body) {
						return "no_op", map[string]any{
							"code":    "already_deleted",
							"message": "item is already in the trash",
						}, nil
					}
					headers := map[string]string{"If-Unmodified-Since-Version": strconv.Itoa(version)}
					var writeErr error
					if permanent {
						_, _, writeErr = c.DeleteWithHeaders(path, headers)
					} else {
						// The trash flag, which items restore clears and items trash lists.
						// A hard DELETE here destroyed the item and its child attachments
						// outright while the help promised the trash.
						_, _, writeErr = c.PatchWithHeaders(path, map[string]any{"deleted": 1}, headers)
					}
					if writeErr != nil {
						// The rare GET-then-write race: the item vanished in the moment
						// between the version read and this call.
						if flags.ignoreMissing && strings.Contains(writeErr.Error(), "HTTP 404") {
							return "no_op", map[string]any{
								"code":    "already_deleted",
								"message": "item was removed between the version read and the write; --ignore-missing treats this as already done",
							}, nil
						}
						applyErr = writeErr
						return "failed", nil, writeErr
					}
					return "applied", nil, nil
				},
			}}
			env, runErr := runMutation(cmd.Context(), flags, "items.delete", ops)
			if runErr != nil {
				if applyErr != nil {
					return classifyAPIError(applyErr, flags)
				}
				return runErr
			}
			return renderMutation(cmd, flags, env, nil)
		},
	}

	cmd.Flags().BoolVar(&permanent, "permanent", false,
		"Destroy the item and its attachments outright instead of trashing it (irreversible; requires --allow-destructive)")

	return cmd
}

// itemAlreadyTrashed reports whether a Zotero item's own write-plane copy
// already carries deleted=1. Fails closed (false) on any unexpected shape,
// since the worst consequence of a false negative is a redundant PATCH, while
// a false positive would silently skip a real trash.
func itemAlreadyTrashed(body json.RawMessage) bool {
	var envelope struct {
		Data struct {
			Deleted json.Number `json:"deleted"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return false
	}
	n, err := envelope.Data.Deleted.Int64()
	return err == nil && n != 0
}
