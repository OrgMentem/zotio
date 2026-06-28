// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase3): cli surface for the mutation run-journal — the
// recorder hook wired on the real Execute() path, plus `journal list`/`show`.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"zotero-pp-cli/internal/mutation"
)

// mutationJournalRecorder, when non-nil, records applied mutation runs. It is
// set only on the real CLI entry (Execute), so unit tests that drive
// subcommands directly never write to the filesystem.
var mutationJournalRecorder func(env mutation.Envelope)

// journalDir is the per-install directory holding the append-only run journal,
// alongside the synced store.
func journalDir() string {
	return filepath.Join(filepath.Dir(defaultDBPath("zotero-pp-cli")), "journal")
}

// recordMutationJournal appends an entry for any run that applied at least one
// change. Best-effort: a journal failure never fails the mutation.
func recordMutationJournal(env mutation.Envelope) {
	if env.Result == nil || env.Result.Summary.Applied == 0 {
		return
	}
	entry, ok := mutation.BuildJournalEntry(env, time.Now())
	if !ok {
		return
	}
	if err := mutation.WriteEntry(journalDir(), entry); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not record mutation journal: %v\n", err)
	}
}

func newJournalCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "journal",
		Short:       "Inspect the mutation run journal (applied write history)",
		Annotations: map[string]string{"mcp:read-only": "true"},
	}
	cmd.AddCommand(newJournalListCmd(flags))
	cmd.AddCommand(newJournalShowCmd(flags))
	return cmd
}

func newJournalListCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List recorded mutation runs, newest first",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := mutation.ListEntries(journalDir())
			if err != nil {
				return fmt.Errorf("reading journal: %w", err)
			}
			if flags.asJSON {
				data, err := json.Marshal(entries)
				if err != nil {
					return err
				}
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
			}
			out := cmd.OutOrStdout()
			if len(entries) == 0 {
				fmt.Fprintln(out, "No mutation runs recorded yet.")
				return nil
			}
			for _, e := range entries {
				ok := "ok"
				if !e.OK {
					ok = "incomplete"
				}
				fmt.Fprintf(out, "%s  %s  %-24s  applied=%d  %s\n",
					e.RunID, e.Timestamp.Format("2006-01-02 15:04"), e.Operation, e.Summary.Applied, ok)
			}
			return nil
		},
	}
}

func newJournalShowCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:         "show <run-id>",
		Short:       "Show the operations recorded for one mutation run",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			entry, err := mutation.ReadEntry(journalDir(), args[0])
			if err != nil {
				return notFoundErr(err)
			}
			if flags.asJSON {
				data, err := json.Marshal(entry)
				if err != nil {
					return err
				}
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Run %s · %s · %s · applied=%d\n", entry.RunID, entry.Timestamp.Format("2006-01-02 15:04:05"), entry.Operation, entry.Summary.Applied)
			for _, op := range entry.Ops {
				fmt.Fprintf(out, "  [%s] %s %s", op.Status, op.Kind, op.Key)
				if op.Destructive {
					fmt.Fprint(out, " (destructive)")
				}
				fmt.Fprintln(out)
			}
			return nil
		},
	}
}
