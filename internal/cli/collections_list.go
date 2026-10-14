// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newCollectionsListCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int
	var flagStart int

	cmd := &cobra.Command{
		Use:         "list",
		Short:       "List all collections",
		Example:     "  zotio collections list --limit 50",
		Annotations: map[string]string{"zotio:endpoint": "collections.list", "zotio:method": "GET", "zotio:path": "/collections", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateLimitFlag("--limit", flagLimit); err != nil {
				return err
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			path := "/collections"
			params := map[string]string{}
			if flagLimit != 0 {
				params["limit"] = fmt.Sprintf("%v", flagLimit)
			}
			if flagStart != 0 {
				params["start"] = fmt.Sprintf("%v", flagStart)
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
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum number of collections to return (0 = no limit)")
	cmd.Flags().IntVar(&flagStart, "start", 0, "Pagination offset")

	return cmd
}
