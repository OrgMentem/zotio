// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase3): cli adapter for the internal/mutation engine.
// Binds *rootFlags -> mutation.Options and renders mutation.Envelope with the
// cli's terminal/JSON helpers. The state machine and gates live in the package
// so it stays free of cobra/cli dependencies.

package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"zotero-pp-cli/internal/mutation"
)

// mutationOptions translates command flags into the engine's flag-agnostic
// Options. A nil flags pointer maps MaxChanges to -1 so the default cap applies.
func mutationOptions(flags *rootFlags) mutation.Options {
	if flags == nil {
		return mutation.Options{MaxChanges: -1}
	}
	return mutation.Options{
		Yes:              flags.yes,
		DryRun:           flags.dryRun,
		Agent:            flags.agent,
		MaxChanges:       flags.maxChanges,
		AllowDestructive: flags.allowDestructive,
		ContinueOnError:  flags.continueOnError,
		MaxFailures:      flags.maxFailures,
	}
}

// resolveMutationMode reports whether the run previews or applies, given flags.
func resolveMutationMode(flags *rootFlags) mutation.Mode {
	return mutation.ResolveMode(mutationOptions(flags))
}

// runMutation previews or applies the operations through the shared engine, then
// (on the real CLI path) replays applied writes into the local mirror for
// read-your-writes and records the run to the journal.
func runMutation(ctx context.Context, flags *rootFlags, operation string, ops []mutation.Op) (mutation.Envelope, error) {
	_ = ctx
	env, err := mutation.Run(mutationOptions(flags), operation, ops)
	if mirrorWriteThrough != nil {
		mirrorWriteThrough(&env)
	}
	if mutationJournalRecorder != nil {
		mutationJournalRecorder(env)
	}
	return env, err
}

// renderMutation writes the envelope as JSON (under --json or non-TTY) or a
// human summary. singleLine, when provided, renders a single-op plan compactly.
func renderMutation(cmd *cobra.Command, flags *rootFlags, env mutation.Envelope, singleLine func(mutation.Envelope) string) error {
	out := cmd.OutOrStdout()
	if flags != nil && flags.asJSON || !isTerminal(out) {
		data, err := json.Marshal(env)
		if err != nil {
			return err
		}
		return printOutput(out, json.RawMessage(data), true)
	}

	if env.Error == nil && singleLine != nil && len(env.Plan.Operations) == 1 {
		fmt.Fprintln(out, singleLine(env))
		return nil
	}

	fmt.Fprintf(out, "Plan: %d planned · %d no-op · %d destructive", env.Plan.Summary.Planned, env.Plan.Summary.NoOp, env.Plan.Summary.Destructive)
	if env.Result != nil {
		fmt.Fprintf(out, " · %d applied · %d skipped · %d conflicts · %d failed", env.Result.Summary.Applied, env.Result.Summary.Skipped, env.Result.Summary.Conflicts, env.Result.Summary.Failed)
		if env.Result.Summary.NotAttempted > 0 {
			fmt.Fprintf(out, " · %d not attempted", env.Result.Summary.NotAttempted)
		}
	}
	fmt.Fprintln(out)
	if env.Error != nil {
		fmt.Fprintf(out, "Error: %s — %s\n", env.Error.Code, env.Error.Message)
	}

	rows := mutation.Rows(env)
	for i, row := range rows {
		if i == 50 {
			fmt.Fprintf(out, "… %d more\n", len(rows)-50)
			break
		}
		fmt.Fprintln(out, row)
	}
	return nil
}
