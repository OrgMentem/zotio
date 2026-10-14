// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"github.com/spf13/cobra"
)

func newCollectionsTagsCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int
	var flagStart int
	var flagQ string
	var flagQMode string
	cmd := &cobra.Command{
		Use:         "tags <collectionKey>",
		Short:       "List tags used within a collection",
		Example:     "  zotio collections tags ABCD2345 --query ethics",
		Annotations: map[string]string{"zotio:endpoint": "collections.tags", "zotio:method": "GET", "zotio:path": "/collections/{collectionKey}/tags", "mcp:read-only": "true"},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateLimitFlag("--limit", flagLimit); err != nil {
				return err
			}
			params, err := tagListParams(flagLimit, flagStart, flagQ, flagQMode)
			if err != nil {
				return usageErr(err)
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			if err := verifyCollectionExists(cmd.Context(), c, flags, args[0]); err != nil {
				return err
			}

			path := "/collections/{collectionKey}/tags"
			path = replacePathParam(path, "collectionKey", args[0])
			data, prov, err := resolveRead(cmd.Context(), c, flags, "collections", false, path, params, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			data = truncateJSONArray(data, flagLimit)
			return printCollectionsRead(cmd, flags, data, prov)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum number of tags to return (0 = no limit)")
	cmd.Flags().IntVar(&flagStart, "start", 0, "Pagination offset")
	cmd.Flags().StringVar(&flagQ, "query", "", "Filter tags by name substring")
	cmd.Flags().StringVar(&flagQMode, "query-mode", "contains", "Tag query mode: contains or startsWith")

	return cmd
}
