// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"github.com/spf13/cobra"
)

func newCollectionsGetCmd(flags *rootFlags) *cobra.Command {

	cmd := &cobra.Command{
		Use:         "get <collectionKey>",
		Short:       "Get a specific collection",
		Example:     "  zotio collections get ABCD2345",
		Annotations: map[string]string{"zotio:endpoint": "collections.get", "zotio:method": "GET", "zotio:path": "/collections/{collectionKey}", "mcp:read-only": "true"},
		// One key per run. Cobra's default for a command with no subcommands is
		// ArbitraryArgs, so `collections get K1 K2` used to read K1 and drop K2
		// on the floor: the request path is built from args[0] alone, and
		// nothing reported the ignored keys. A missing key is a usage error, not
		// a help request that exits 0.
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := flags.newClient()
			if err != nil {
				return err
			}

			path := "/collections/{collectionKey}"
			path = replacePathParam(path, "collectionKey", args[0])
			params := map[string]string{}
			data, prov, err := resolveRead(cmd.Context(), c, flags, "collections", false, path, params, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			return printCollectionsRead(cmd, flags, data, prov)
		},
	}

	return cmd
}
