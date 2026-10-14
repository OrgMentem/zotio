// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newCollectionsTopCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int

	cmd := &cobra.Command{
		Use:         "top",
		Short:       "List only top-level collections (no parents)",
		Example:     "  zotio collections top --limit 20",
		Annotations: map[string]string{"zotio:endpoint": "collections.top", "zotio:method": "GET", "zotio:path": "/collections/top", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateLimitFlag("--limit", flagLimit); err != nil {
				return err
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			path := "/collections/top"
			params := map[string]string{}
			if flagLimit != 0 {
				params["limit"] = fmt.Sprintf("%v", flagLimit)
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

	return cmd
}
