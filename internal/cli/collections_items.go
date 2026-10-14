// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newCollectionsItemsCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int
	var flagStart int
	var flagFormat string

	cmd := &cobra.Command{
		Use:         "items <collectionKey>",
		Short:       "List all items in a collection",
		Example:     "  zotio collections items ABCD2345 --limit 25",
		Annotations: map[string]string{"zotio:endpoint": "collections.items", "zotio:method": "GET", "zotio:path": "/collections/{collectionKey}/items", "mcp:read-only": "true"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateLimitFlag("--limit", flagLimit); err != nil {
				return err
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			if err := verifyCollectionExists(cmd.Context(), c, flags, args[0]); err != nil {
				return err
			}

			path := "/collections/{collectionKey}/items"
			path = replacePathParam(path, "collectionKey", args[0])
			params := map[string]string{}
			if flagLimit != 0 {
				params["limit"] = fmt.Sprintf("%v", flagLimit)
			}
			if flagStart != 0 {
				params["start"] = fmt.Sprintf("%v", flagStart)
			}
			if flagFormat != "" {
				params["format"] = fmt.Sprintf("%v", flagFormat)
			}
			data, prov, err := resolveRead(cmd.Context(), c, flags, "collections", false, path, params, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			// Honor --limit when the API accepts but ignores ?limit=N.
			data = truncateJSONArray(data, flagLimit)
			return printCollectionsRead(cmd, flags, data, prov)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum number of items to return (0 = no limit)")
	cmd.Flags().IntVar(&flagStart, "start", 0, "Pagination offset")
	cmd.Flags().StringVar(&flagFormat, "format", "", "Response format (json, bibtex, ris, etc.)")

	return cmd
}
