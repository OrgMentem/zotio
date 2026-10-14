// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// cli adapter for the internal/mutation engine.
// Binds *rootFlags -> mutation.Options and renders mutation.Envelope with the
// cli's terminal/JSON helpers. The state machine and gates live in the package
// so it stays free of cobra/cli dependencies.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
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
// read-your-writes and records the run to the journal. tune adjusts the resolved
// options for callers whose transport imposes its own semantics.
func runMutation(ctx context.Context, flags *rootFlags, operation string, ops []mutation.Op, tune ...func(*mutation.Options)) (mutation.Envelope, error) {
	opts := mutationOptions(flags)
	opts.Context = ctx
	for _, adjust := range tune {
		adjust(&opts)
	}
	env, err := mutation.Run(opts, operation, ops)
	if mirrorWriteThrough != nil {
		mirrorWriteThrough(&env)
	}
	if mutationJournalRecorder != nil {
		if journalErr := mutationJournalRecorder(&env); journalErr != nil {
			// The writes landed; only the undo record is missing. That is a
			// warning (printed on stderr by renderMutation), not a failure,
			// so the exit code stays whatever the engine decided.
			env.Warnings = append(env.Warnings, fmt.Sprintf("applied but not journaled: %v (journal undo unavailable for this run)", journalErr))
		}
	}
	return env, classifyMutationErr(env, err)
}

// classifyMutationErr types the engine's errors for the exit-code contract. A
// gate refusal keeps its engine error; invalid local input is a usage error;
// an incomplete multi-op run is degraded (exit 13) only when some op applied and another failed. It does not mark the error
// rendered: only the code that prints the envelope may do that.
func classifyMutationErr(env mutation.Envelope, err error) error {
	if err == nil {
		return nil
	}
	var gate *mutation.Error
	if errors.As(err, &gate) {
		if gate.Code == mutation.CodeInvalidInput {
			return usageErr(err)
		}
		return err
	}
	var typed *cliError
	if errors.As(err, &typed) {
		return err
	}
	if errors.Is(err, mutation.ErrIncomplete) {
		// Exit 13 means partial: some ops applied and others failed or were
		// never attempted. A run with nothing applied, or one whose only
		// problem is a lost precondition (conflict), keeps exit 1.
		if r := env.Result; r != nil && r.Summary.Applied > 0 && r.Summary.Failed+r.Summary.NotAttempted > 0 {
			return degradedErr(err)
		}
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return degradedErr(err)
	}
	return err
}

// renderMutationResult renders the envelope and returns runErr marked as
// rendered, so the central JSON error printer does not print a second object.
// Callers that return runErr without rendering must not use markRendered.
func renderMutationResult(cmd *cobra.Command, flags *rootFlags, env mutation.Envelope, runErr error, singleLine func(mutation.Envelope) string) error {
	if renderErr := renderMutation(cmd, flags, env, singleLine); renderErr != nil {
		return renderErr
	}
	if runErr == nil {
		return nil
	}
	return markRendered(runErr)
}

// renderMutation writes the envelope as JSON (under --json or non-TTY) or a
// human summary. singleLine, when provided, renders a single-op plan compactly.
func renderMutation(cmd *cobra.Command, flags *rootFlags, env mutation.Envelope, singleLine func(mutation.Envelope) string) error {
	if flags != nil && flags.quiet {
		// --quiet: print nothing; the exit code is the answer.
		return nil
	}
	out := cmd.OutOrStdout()
	// Warnings go to stderr in every mode so JSON callers still see them.
	for _, warning := range env.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
	}
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

	// Row text splices server-controlled strings — a rejection message, an
	// API error body — into the terminal. The process error sink sanitizes
	// the returned error, but nothing sanitized stdout, so an escape sequence
	// in a server message could rewrite the operator's terminal. Sanitizing
	// here covers every mutation command at once.
	rows := mutation.Rows(env)
	for i, row := range rows {
		if i == 50 {
			fmt.Fprintf(out, "… %d more\n", len(rows)-50)
			break
		}
		fmt.Fprintln(out, SanitizeForTerminal(row))
	}
	return nil
}
