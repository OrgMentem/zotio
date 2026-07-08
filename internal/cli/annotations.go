// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import "github.com/spf13/cobra"

func newAnnotationsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "annotations",
		Short: "Search and export annotations from your Zotero library",
	}
	cmd.AddCommand(newAnnotationsExportCmd(flags))
	cmd.AddCommand(newAnnotationsTimelineCmd(flags))
	cmd.AddCommand(newAnnotationsSearchCmd(flags))
	return cmd
}
