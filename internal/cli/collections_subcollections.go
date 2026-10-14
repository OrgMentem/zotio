// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"github.com/spf13/cobra"
)

func newCollectionsSubcollectionsCmd(flags *rootFlags) *cobra.Command {

	cmd := &cobra.Command{
		Use:         "subcollections <collectionKey>",
		Short:       "List subcollections of a collection",
		Example:     "  zotio collections subcollections ABCD2345",
		Annotations: map[string]string{"zotio:endpoint": "collections.subcollections", "zotio:method": "GET", "zotio:path": "/collections/{collectionKey}/collections", "mcp:read-only": "true"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			if err := verifyCollectionExists(cmd.Context(), c, flags, args[0]); err != nil {
				return err
			}

			path := "/collections/{collectionKey}/collections"
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
