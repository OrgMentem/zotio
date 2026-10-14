// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newItemsStaleCmd(flags *rootFlags) *cobra.Command {
	var flagDays int
	var flagNoPDF bool
	var flagNoAnnotations bool
	var flagLimit int

	cmd := &cobra.Command{
		Use:         "stale",
		Short:       "Find old items without PDFs or annotations",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireMirrorOnlySource(flags, cmd.CommandPath()); err != nil {
				return err
			}
			if flagDays < 0 {
				return usageErr(fmt.Errorf("--older-than-days must be 0 or greater"))
			}
			if err := validateLimitFlag("--limit", flagLimit); err != nil {
				return err
			}
			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if rawDB == nil {
				return errUnsyncedStoreFor(cmd, flags)
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			rows, err := queryStaleItems(db, flagDays, flagNoPDF, flagNoAnnotations, flagLimit)
			if err != nil {
				return fmt.Errorf("querying stale items: %w", err)
			}
			data, err := json.Marshal(rows)
			if err != nil {
				return err
			}
			prov := localProvenance(rawDB, "items", "local_only")
			printProvenance(cmd, len(rows), prov)
			return printMirrorRows(cmd, flags, data, prov)
		},
	}
	cmd.Flags().IntVar(&flagDays, "older-than-days", 365, "Items added more than this many days ago")
	cmd.Flags().BoolVar(&flagNoPDF, "no-pdf", false, "Include only items without a PDF attachment")
	cmd.Flags().BoolVar(&flagNoAnnotations, "no-annotations", false, "Include only items without annotation children")
	cmd.Flags().IntVar(&flagLimit, "limit", 50, "Maximum number of items to return (0 = no limit)")

	return cmd
}

// errUnsyncedStoreFor is the precondition error for a mirror reader that finds
// no synced store. It carries the shared precondition envelope: kind, code,
// capability, precondition "synced_store", detail and remediation, printed
// under --json/--agent or piped output.
func errUnsyncedStoreFor(cmd *cobra.Command, flags *rootFlags) error {
	return emitPreconditionUnmetWithRemediation(cmd.OutOrStdout(), flags, commandRegistryPath(cmd), preconditionSyncedStore,
		"local store not found; no synced data", preconditionRemediation(preconditionSyncedStore))
}

// printMirrorRows prints a local-mirror row list: the {meta, results} envelope
// for JSON/piped output, a table (or --plain/--csv) otherwise. Field flags
// apply to the rows in both forms.
func printMirrorRows(cmd *cobra.Command, flags *rootFlags, data json.RawMessage, prov DataProvenance) error {
	if !wantsJSONEnvelope(cmd.OutOrStdout(), flags) {
		return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
	}
	if flags.quiet {
		return nil
	}
	filtered, err := applyFieldFlags(data, flags)
	if err != nil {
		return err
	}
	wrapped, err := wrapWithProvenance(filtered, prov)
	if err != nil {
		return err
	}
	return printOutput(cmd.OutOrStdout(), wrapped, true)
}

func queryStaleItems(db localQueryStore, days int, noPDF, noAnnotations bool, limit int) ([]map[string]any, error) {
	conditions := []string{
		"i.resource_type='items'",
		"json_extract(i.data,'$.data.itemType') NOT IN ('attachment','note','annotation')",
		"DATE(json_extract(i.data,'$.data.dateAdded')) <= DATE('now', '-' || ? || ' days')",
	}
	args := []any{days}

	applyNoPDF := noPDF || (!noPDF && !noAnnotations)
	applyNoAnnotations := noAnnotations || (!noPDF && !noAnnotations)
	// Children are joined on the indexed parent_key column, which the store
	// fills from parentItem for both nested and flat payloads. Annotations hang
	// off attachments, not the top-level item, so they need the attachment hop.
	if applyNoPDF {
		conditions = append(conditions, `NOT EXISTS (
	SELECT 1 FROM resources a WHERE a.resource_type='items'
		AND a.parent_key=i.id
		AND a.item_type='attachment'
		AND json_extract(a.data,'$.data.contentType')='application/pdf'
)`)
	}
	if applyNoAnnotations {
		conditions = append(conditions, `NOT EXISTS (
	SELECT 1 FROM resources att
	JOIN resources a ON a.resource_type='items' AND a.parent_key=att.id AND a.item_type='annotation'
	WHERE att.resource_type='items' AND att.parent_key=i.id
)`)
	}

	query := fmt.Sprintf(`
SELECT
	i.id AS key,
	json_extract(i.data,'$.data.title') AS title,
	json_extract(i.data,'$.data.itemType') AS item_type,
	json_extract(i.data,'$.data.dateAdded') AS date_added
FROM resources i
WHERE %s
ORDER BY date_added ASC`, strings.Join(conditions, "\n\tAND "))
	if limit > 0 {
		query += `
LIMIT ?`
		args = append(args, limit)
	}
	return db.QueryRaw(query, args...)
}
