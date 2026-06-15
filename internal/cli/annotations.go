// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written annotation export/search command group missing from the generated CLI.

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
