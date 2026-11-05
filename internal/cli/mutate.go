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
	"sync"
	"time"

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
	missingKey, restoreApply := watchMissingWriteKey(ops)
	defer restoreApply()
	var observe func(op mutation.Op, item mutation.ResultItem) error
	recordOutcomes := resolveMutationMode(flags).Apply && (mutationOutcomeRecorder != nil || mirrorPermanentDelete != nil)
	if recordOutcomes {
		// Record each landed write next to its side effect. A kill, a closed
		// terminal or a panic in a later op would otherwise lose the journal
		// line and the permanent-delete marker of every write that already
		// reached the library, because both used to be produced once from the
		// final envelope.
		if mutationOutcomeRecorder != nil && opts.RunID == "" {
			opts.RunID = mutation.NewRunID(time.Now())
		}
		mode := mutation.ResolveMode(opts).Mode
		observe = func(op mutation.Op, item mutation.ResultItem) error {
			// The marker goes first, and regardless of what the journal record
			// does: the delete has landed, and until the marker exists a sync
			// inside the Web-API-write / local-read lag window re-inserts the
			// item (ADR-0007).
			if mirrorPermanentDelete != nil && op.Kind == "item_delete" && item.Status == "applied" && item.Key != "" {
				mirrorPermanentDelete(item.Key)
			}
			if mutationOutcomeRecorder == nil {
				return nil
			}
			err := mutationOutcomeRecorder(opts.RunID, operation, mode, op, item)
			if err == nil {
				return nil
			}
			// Fail closed, without a retry: a first attempt that appended its
			// line and then failed to sync would leave the op recorded twice,
			// and the journal reader does not fold duplicate op records. The
			// run's final entry would repeat this op, but a kill before that
			// entry lands leaves a write that reached the library with no
			// journal line to undo it — and, for a permanent delete, nothing
			// that stops the next sync resurrecting the item. The engine holds
			// back every later write rather than widen that window. With no
			// write left to hold back it is a warning, and the final entry is
			// the run's record.
			return preconditionErr(&journalBarrierError{err: fmt.Errorf("the journal could not record the landed %s write for %s in run %s: %w", opKindLabel(op), opTargetLabel(op, item), opts.RunID, err)})
		}
	}
	env, err := mutation.RunObserved(opts, operation, ops, observe)
	if len(ops) > 0 {
		env.Undoes = ops[0].Undoes
	}
	recordWorkflowRunMutation(ctx, &env)
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
	if keyErr := missingKey(); keyErr != nil && (env.Result == nil || env.Result.Summary.Applied == 0) {
		// Nothing reached the library because no Web API key exists: that is a
		// setup precondition (exit 9 with credential guidance), not a per-item
		// write failure. The envelope still lists the refused operation.
		return env, keyErr
	}
	return env, classifyMutationErr(env, err)
}

// journalBarrierError is the exit-9 precondition runMutation returns when the
// journal could not record a landed write and the engine therefore held back
// the run's later writes. Commands that replace the engine's generic error with
// their own failed write's class must keep this one: see isJournalBarrier.
type journalBarrierError struct{ err error }

func (e *journalBarrierError) Error() string { return e.err.Error() }
func (e *journalBarrierError) Unwrap() error { return e.err }

// isJournalBarrier reports whether a runMutation error is the journal barrier.
// It outranks an earlier failed write's own error: that write is already in the
// envelope, while the barrier is what tells the operator that writes landed
// with no journal record and that the rest of the run was held back.
func isJournalBarrier(err error) bool {
	var barrier *journalBarrierError
	return errors.As(err, &barrier)
}

// opKindLabel names an op's kind for an error message, falling back to a word
// that reads correctly when a plan builder left Kind empty.
func opKindLabel(op mutation.Op) string {
	if op.Kind == "" {
		return "mutation"
	}
	return op.Kind
}

// opTargetLabel names what an op wrote: its confirmed key, else the planned
// key, else the op id — a create has no key until Apply reports one.
func opTargetLabel(op mutation.Op, item mutation.ResultItem) string {
	if item.Key != "" {
		return item.Key
	}
	if op.Key != "" {
		return op.Key
	}
	return "operation " + op.ID
}

// watchMissingWriteKey wraps each op's Apply, in place, to record the first
// missing-Web-API-key refusal; restore puts the original Apply functions back.
// In place because some Apply functions write back into the op the engine
// holds (duplicate resolve marks a half-applied merge on its own op).
// Selection clients defer the missing-key refusal to the first write so keyless
// planning and no-op applies still work; without this, the refusal surfaced as
// an ordinary failed item and exit 1.
func watchMissingWriteKey(ops []mutation.Op) (missingKey func() error, restore func()) {
	var (
		mu    sync.Mutex
		first error
	)
	originals := make([]func() (string, any, error), len(ops))
	for i := range ops {
		apply := ops[i].Apply
		originals[i] = apply
		if apply == nil {
			continue
		}
		ops[i].Apply = func() (string, any, error) {
			status, reason, err := apply()
			if err != nil && errors.Is(err, errMissingWriteKey) {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
			return status, reason, err
		}
	}
	missingKey = func() error {
		mu.Lock()
		defer mu.Unlock()
		return first
	}
	restore = func() {
		for i := range ops {
			ops[i].Apply = originals[i]
		}
	}
	return missingKey, restore
}

// classifyMutationErr types the engine's errors for the exit-code contract. A
// gate refusal keeps its engine error; invalid local input is a usage error;
// an incomplete or canceled multi-op run is degraded (exit 13) only when some
// op applied and another failed or was not attempted. It does not mark the
// error rendered: only the code that prints the envelope may do that.
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
	if errors.Is(err, mutation.ErrIncomplete) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// Exit 13 means partial: some ops applied and others failed or were
		// never attempted. A run with nothing applied, or one whose only
		// problem is a lost precondition (conflict), keeps exit 1. A
		// cancellation is held to the same gate: Ctrl-C, SIGTERM or a caller's
		// deadline before the first write leaves nothing to reconcile, and
		// reporting that run as partial sends callers looking for writes that
		// never happened.
		if r := env.Result; r != nil && r.Summary.Applied > 0 && r.Summary.Failed+r.Summary.NotAttempted > 0 {
			return degradedErr(err)
		}
		return err
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
