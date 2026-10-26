// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/mutation"
	"zotio/internal/store"
)

func newCollectionsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collections",
		Short: "Manage collections in your Zotero library",
		Long: `Manage collections in your Zotero library.

This group covers the collections themselves (create, update, move, delete,
list, export); it does not add items to a collection. To add an item, use
'zotio items move --to <collectionKey>' (by key, also --from and --keys-from
for many items at once) or 'zotio items add-to-collection <itemKey>
--collection-name <name>' (by name, creating the collection if it does not
exist yet).`,
	}

	cmd.AddCommand(newCollectionsCreateCmd(flags))
	cmd.AddCommand(newCollectionsDeleteCmd(flags))
	cmd.AddCommand(newCollectionsGetCmd(flags))
	cmd.AddCommand(newCollectionsItemsCmd(flags))
	cmd.AddCommand(newCollectionsListCmd(flags))
	cmd.AddCommand(newCollectionsBundleCmd(flags))
	// Register collection citation-gap discovery.
	cmd.AddCommand(newCollectionsGapsCmd(flags))
	cmd.AddCommand(newCollectionsExportCmd(flags))
	cmd.AddCommand(newCollectionsMoveCmd(flags))
	cmd.AddCommand(newCollectionsStatsCmd(flags))
	cmd.AddCommand(newCollectionsSubcollectionsCmd(flags))
	cmd.AddCommand(newCollectionsTagsCmd(flags))
	cmd.AddCommand(newCollectionsTopCmd(flags))
	cmd.AddCommand(newCollectionsUpdateCmd(flags))
	return cmd
}

// verifyCollectionExists reads /collections/<key> through the caller's data
// source and maps a miss to exit 3. Zotero's local API answers
// /collections/<bad>/items with the whole library instead of a 404, so every
// collection-scoped command runs this first. c may be nil under
// --data-source local, which never touches the client.
func verifyCollectionExists(ctx context.Context, c *client.Client, flags *rootFlags, key string) error {
	path := replacePathParam("/collections/{collectionKey}", "collectionKey", key)
	_, _, err := resolveRead(ctx, c, flags, "collections", false, path, map[string]string{}, nil)
	if err == nil {
		return nil
	}
	var typed *cliError
	if errors.As(err, &typed) {
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, "not found in local store") {
		return notFoundErr(collectionMissingFromMirror(key))
	}
	if strings.Contains(msg, "no local data") {
		return preconditionErr(err)
	}
	return classifyAPIError(err, flags)
}

// verifyMirrorCollectionExists is verifyCollectionExists for the commands that
// read the local mirror only.
func verifyMirrorCollectionExists(ctx context.Context, db *store.Store, key string) error {
	if _, err := db.GetContext(ctx, "collections", key); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Same rule as a keyed mirror read: a key absent from a synced
			// mirror does not exist there (exit 3); collections that were
			// never synced say nothing about the key (exit 9).
			if _, lastSynced, _, syncErr := db.GetSyncStateContext(ctx, "collections"); syncErr == nil && lastSynced.IsZero() {
				return preconditionErr(fmt.Errorf("no local data for %q. Run 'zotio sync' first", "collections"))
			}
			return notFoundErr(collectionMissingFromMirror(key))
		}
		return fmt.Errorf("checking collection %s in the local mirror: %w", key, err)
	}
	return nil
}

func collectionMissingFromMirror(key string) error {
	return fmt.Errorf("collection %q not found in the local mirror; run 'zotio sync' if it was created recently", key)
}

// printCollectionsRead renders a collection read for every output mode: the
// provenance envelope for JSON, a table on a terminal, the standard pipeline
// otherwise.
func printCollectionsRead(cmd *cobra.Command, flags *rootFlags, data json.RawMessage, prov DataProvenance) error {
	out := cmd.OutOrStdout()
	if flags.quiet {
		return nil
	}
	printProvenance(cmd, countResultItems(data), prov)
	if wantsJSONEnvelope(out, flags) {
		filtered, err := applyFieldFlags(data, flags)
		if err != nil {
			return err
		}
		wrapped, err := wrapWithProvenance(filtered, prov)
		if err != nil {
			return err
		}
		return printOutput(out, wrapped, true)
	}
	if wantsHumanTable(out, flags) {
		var items []map[string]any
		if json.Unmarshal(data, &items) == nil && len(items) > 0 {
			if err := printAutoTable(out, items); err != nil {
				return err
			}
			if len(items) >= 25 {
				fmt.Fprintf(os.Stderr, "\nShowing %d results. To narrow: add --limit, --json --select, or filter flags.\n", len(items))
			}
			return nil
		}
	}
	return printOutputWithFlags(out, data, flags)
}

// collectionWriteLine words a one-operation collection write for a terminal.
func collectionWriteLine(wouldVerb, doneVerb string) func(mutation.Envelope) string {
	return func(env mutation.Envelope) string {
		key := ""
		if len(env.Plan.Operations) == 1 {
			key = env.Plan.Operations[0].Key
			if env.Plan.Operations[0].Kind == "collection_delete" {
				for _, change := range env.Plan.Operations[0].Changes {
					switch change.Field {
					case "name":
						key += fmt.Sprintf(" %q", change.Remove)
					case "items":
						key += fmt.Sprintf(" (%v items)", change.Remove)
					case "subcollections":
						key += fmt.Sprintf(" (%v subcollections)", change.Remove)
					}
				}
			}
		}
		if env.Mode == "apply" && env.Result != nil && len(env.Result.Items) == 1 {
			item := env.Result.Items[0]
			if item.Key != "" {
				key = item.Key
			}
			if item.Status != "applied" {
				return fmt.Sprintf("%s collection %s", item.Status, key)
			}
			return fmt.Sprintf("%s collection %s", doneVerb, key)
		}
		return fmt.Sprintf("would %s collection %s", wouldVerb, key)
	}
}

// readCollectionVersion reads one collection from the write plane and returns
// its version. Previews and applies both run it before the plan is built, so a
// missing collection is exit 3 and the plan carries the version the apply's
// If-Unmodified-Since-Version guard will use.
func readCollectionVersion(c *client.Client, flags *rootFlags, key string) (int, error) {
	path := replacePathParam("/collections/{collectionKey}", "collectionKey", key)
	_, version, err := c.GetWithVersion(path, nil)
	if err != nil {
		return 0, classifyAPIError(err, flags)
	}
	if version <= 0 {
		return 0, apiErr(fmt.Errorf("reading collection version for %s: response did not include a version", key))
	}
	return version, nil
}

// preferApplyErr keeps the classified error of the write that failed (its HTTP
// class, its message) over the engine's generic "mutation incomplete". Exit 13
// stays: it means part of the run applied, which only the engine knows.
func preferApplyErr(runErr, applyErr error) error {
	if runErr == nil || applyErr == nil {
		return runErr
	}
	var typed *cliError
	if errors.As(runErr, &typed) && typed.code == 13 {
		return degradedErr(applyErr)
	}
	return applyErr
}
