// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"zotio/internal/store"

	"github.com/spf13/cobra"
)

func newWorkflowCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Compound workflows that combine multiple API operations",
	}

	cmd.AddCommand(newWorkflowArchiveCmd(flags))
	cmd.AddCommand(newWorkflowRunCmd(flags))
	cmd.AddCommand(newWorkflowStatusCmd(flags))

	return cmd
}

// newWorkflowArchiveCmd runs `sync --strict` over the default resources.
// It builds a fresh sync command, as watch does each cycle, so archive and
// sync share one fetch path, one JSON event stream, and one exit policy.
func newWorkflowArchiveCmd(flags *rootFlags) *cobra.Command {
	var dbFlag string
	var full bool

	cmd := &cobra.Command{
		Use:   "archive",
		Short: "Run zotio sync --strict over the default resources",
		Long: `Archive runs 'zotio sync --strict' over the default sync resources. It
uses the same code, JSON events, and exit codes as sync, so any resource that
fails to sync makes it exit non-zero. --full and --db mean what they mean for
sync. Run 'zotio sync' directly to choose resources, --since, or concurrency.`,
		Example: `  # Incremental sync of the default resources; a failed resource exits non-zero
  zotio workflow archive

  # Full resync, the same as zotio sync --full --strict
  zotio workflow archive --full`,
		RunE: func(cmd *cobra.Command, args []string) error {
			syncArgs := []string{"--strict"}
			if full {
				syncArgs = append(syncArgs, "--full")
			}
			if dbFlag != "" {
				syncArgs = append(syncArgs, "--db", dbFlag)
			}
			syncCmd := newSyncCmd(flags)
			syncCmd.SilenceErrors, syncCmd.SilenceUsage = true, true
			syncCmd.SetArgs(syncArgs)
			syncCmd.SetOut(cmd.OutOrStdout())
			syncCmd.SetErr(cmd.ErrOrStderr())
			return syncCmd.ExecuteContext(cmd.Context())
		},
	}

	cmd.Flags().StringVar(&dbFlag, "db", "", "Database path (default: ~/.local/share/zotio/data.db)")
	cmd.Flags().BoolVar(&full, "full", false, "Full resync, as sync --full: ignore the stored checkpoint and per-row versions")

	return cmd
}

func newWorkflowStatusCmd(flags *rootFlags) *cobra.Command {
	var dbFlag string

	cmd := &cobra.Command{
		Use:         "status",
		Short:       "Show local archive status and sync state for all resources",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Example: `  # Show archive status
  zotio workflow status

  # Show status as JSON
  zotio workflow status --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The mirror path is a local, resolved on every invocation and
			// never written back into dbFlag, so a `--group all` re-entry
			// reports its own library: see resolveDBPath.
			dbPath, err := resolveDBPath(dbFlag, "zotio")
			if err != nil {
				return err
			}
			if _, err := os.Stat(dbPath); err != nil {
				if os.IsNotExist(err) {
					if flags.asJSON {
						enc := json.NewEncoder(cmd.OutOrStdout())
						enc.SetIndent("", "  ")
						return enc.Encode(map[string]int{})
					}
					fmt.Fprintln(cmd.OutOrStdout(), "No archived data. Run 'workflow archive' to sync.")
					return nil
				}
			}
			s, err := store.OpenReadOnlyContext(cmd.Context(), dbPath)
			if err != nil {
				return fmt.Errorf("opening store: %w", err)
			}
			defer s.Close()

			status, err := s.Status()
			if err != nil {
				return err
			}

			if flags.asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(status)
			}

			if len(status) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No archived data. Run 'workflow archive' to sync.")
				return nil
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Archive Status:")
			total := 0
			for resource, count := range status {
				fmt.Fprintf(cmd.OutOrStdout(), "  %-30s %d items\n", resource, count)
				total += count
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\n  Total: %d items\n", total)
			fmt.Fprintf(cmd.OutOrStdout(), "  Store: %s\n", dbPath)
			return nil
		},
	}

	cmd.Flags().StringVar(&dbFlag, "db", "", "Database path")

	return cmd
}

// resolveDBPath and defaultDBPath are defined in helpers.go
