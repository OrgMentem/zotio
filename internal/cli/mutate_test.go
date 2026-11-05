// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
	"zotio/internal/store"
)

func TestMutationOptionsFromFlags(t *testing.T) {
	got := mutationOptions(&rootFlags{
		yes:              true,
		dryRun:           true,
		agent:            true,
		maxChanges:       7,
		allowDestructive: true,
		continueOnError:  true,
		maxFailures:      3,
	})
	want := mutation.Options{
		Yes:              true,
		DryRun:           true,
		Agent:            true,
		MaxChanges:       7,
		AllowDestructive: true,
		ContinueOnError:  true,
		MaxFailures:      3,
	}
	if got != want {
		t.Fatalf("mutationOptions = %+v, want %+v", got, want)
	}
	if nilOpts := mutationOptions(nil); nilOpts.MaxChanges != -1 {
		t.Fatalf("nil flags MaxChanges = %d, want -1 (default cap)", nilOpts.MaxChanges)
	}
}

func TestResolveMutationModeDelegates(t *testing.T) {
	if m := resolveMutationMode(&rootFlags{yes: true}); !m.Apply || m.Mode != "apply" {
		t.Errorf("yes -> %+v, want apply", m)
	}
	if m := resolveMutationMode(&rootFlags{dryRun: true}); m.Apply || m.PreviewReason != "dry_run" {
		t.Errorf("dry-run -> %+v, want preview dry_run", m)
	}
}

func TestRunMutationDelegatesApply(t *testing.T) {
	called := 0
	ops := []mutation.Op{{ID: "op", Key: "K", Kind: "test", Changes: []mutation.Change{{Field: "title", Add: "T"}}, Apply: func() (string, any, error) {
		called++
		return "applied", nil, nil
	}}}
	env, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "test", ops)
	if err != nil {
		t.Fatalf("runMutation apply err = %v", err)
	}
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 || called != 1 {
		t.Fatalf("apply via flags = %+v (called=%d)", env, called)
	}
}

// TestRunMutationKeepsEngineErrorWhenJournalingFails pins error precedence. A
// journal failure after a failed run must not hide the engine's failure: the
// run exits 13 (degraded) because the engine reported incomplete work, and the
// journal failure stays on the envelope as a warning.
func TestRunMutationKeepsEngineErrorWhenJournalingFails(t *testing.T) {
	mutationJournalRecorder = func(*mutation.Envelope) error { return errors.New("journal disk full") }
	t.Cleanup(func() { mutationJournalRecorder = nil })

	ops := []mutation.Op{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []mutation.Change{{Field: "title", Add: "A"}}, Apply: func() (string, any, error) {
			return "applied", nil, nil
		}},
		{ID: "op2", Key: "K2", Kind: "test", Changes: []mutation.Change{{Field: "title", Add: "B"}}, Apply: func() (string, any, error) {
			return "failed", "server error", errors.New("upstream failed")
		}},
	}
	env, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "test", ops)
	if err == nil {
		t.Fatal("runMutation err = nil, want the engine's incomplete error")
	}
	if ExitCode(err) != 13 {
		t.Fatalf("exit code = %d, want 13 for a partial run", ExitCode(err))
	}
	if !errors.Is(err, mutation.ErrIncomplete) {
		t.Fatalf("err = %v, want it to wrap the engine's incomplete error", err)
	}
	if len(env.Warnings) == 0 || !strings.Contains(env.Warnings[len(env.Warnings)-1], "not journaled") {
		t.Fatalf("warnings = %v, want the journal failure recorded", env.Warnings)
	}
}

// A journal failure on an otherwise clean run is a warning, not a failure.
func TestRunMutationWarnsOnlyWhenJournalingFailsAfterCleanRun(t *testing.T) {
	mutationJournalRecorder = func(*mutation.Envelope) error { return errors.New("journal disk full") }
	t.Cleanup(func() { mutationJournalRecorder = nil })

	ops := []mutation.Op{{ID: "op", Key: "K", Kind: "test", Changes: []mutation.Change{{Field: "title", Add: "A"}}, Apply: func() (string, any, error) {
		return "applied", nil, nil
	}}}
	env, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "test", ops)
	if err != nil {
		t.Fatalf("runMutation err = %v, want nil (journal failure is a warning)", err)
	}
	if !env.OK || len(env.Warnings) != 1 || !strings.Contains(env.Warnings[0], "not journaled") {
		t.Fatalf("env = %+v, want ok with one journal warning", env)
	}
}

// A preview that apply would refuse reports ok:false with the same error and
// exit code as apply.
func TestRunMutationPreviewRefusalMatchesApply(t *testing.T) {
	apply := func() (string, any, error) { return "applied", nil, nil }
	cases := []struct {
		name string
		ops  []mutation.Op
		flag rootFlags
		code string
	}{
		{
			name: "max changes",
			ops: []mutation.Op{
				{ID: "a", Key: "A", Changes: []mutation.Change{{Field: "title", Add: "1"}}, Apply: apply},
				{ID: "b", Key: "B", Changes: []mutation.Change{{Field: "title", Add: "2"}}, Apply: apply},
			},
			flag: rootFlags{maxChanges: 1},
			code: "max_changes_exceeded",
		},
		{
			name: "agent cap",
			ops:  manyChangeOps(51, apply),
			flag: rootFlags{agent: true, maxChanges: -1},
			code: "max_changes_exceeded",
		},
		{
			name: "destructive",
			ops:  []mutation.Op{{ID: "d", Key: "D", Destructive: true, Changes: []mutation.Change{{Field: "collections", Remove: "C"}}, Apply: apply}},
			flag: rootFlags{maxChanges: -1},
			code: "destructive_opt_in_required",
		},
		{
			name: "invalid input",
			ops:  []mutation.Op{{ID: "i", Key: "I", InvalidReason: "file not found: x.pdf", Changes: []mutation.Change{{Field: "file", Add: "x.pdf"}}, Apply: apply}},
			flag: rootFlags{maxChanges: -1},
			code: mutation.CodeInvalidInput,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			preview := tc.flag
			previewEnv, previewErr := runMutation(context.Background(), &preview, "test", tc.ops)
			applying := tc.flag
			applying.yes = true
			applyEnv, applyErr := runMutation(context.Background(), &applying, "test", tc.ops)
			if previewErr == nil || applyErr == nil {
				t.Fatalf("preview err = %v, apply err = %v, want both refused", previewErr, applyErr)
			}
			if applyEnv.Error == nil || previewEnv.Error == nil || *applyEnv.Error != *previewEnv.Error {
				t.Fatalf("preview error %+v != apply error %+v", previewEnv.Error, applyEnv.Error)
			}
			if previewEnv.OK || previewEnv.Error == nil || previewEnv.Error.Code != tc.code {
				t.Fatalf("preview envelope = %+v, want ok:false error %s", previewEnv, tc.code)
			}
			if previewEnv.Mode != "preview" || previewEnv.Result != nil {
				t.Fatalf("preview envelope mode=%q result=%v, want preview with null result", previewEnv.Mode, previewEnv.Result)
			}
			if ExitCode(previewErr) != ExitCode(applyErr) {
				t.Fatalf("preview exit %d != apply exit %d", ExitCode(previewErr), ExitCode(applyErr))
			}
			if tc.code == mutation.CodeInvalidInput && ExitCode(previewErr) != 2 {
				t.Fatalf("invalid input exit = %d, want 2", ExitCode(previewErr))
			}
		})
	}
}

func manyChangeOps(n int, apply func() (string, any, error)) []mutation.Op {
	ops := make([]mutation.Op, n)
	for i := range ops {
		ops[i] = mutation.Op{ID: fmt.Sprint(i), Key: fmt.Sprint(i), Changes: []mutation.Change{{Field: "title", Add: "x"}}, Apply: apply}
	}
	return ops
}

// A preview that passes keeps ok:true and a null result.
func TestRunMutationPassingPreviewIsOK(t *testing.T) {
	ops := []mutation.Op{{ID: "a", Key: "A", Changes: []mutation.Change{{Field: "title", Add: "1"}}}}
	env, err := runMutation(context.Background(), &rootFlags{maxChanges: -1}, "test", ops)
	if err != nil || !env.OK || env.Mode != "preview" || env.Result != nil || env.Error != nil {
		t.Fatalf("preview = %+v err=%v, want ok preview with null result", env, err)
	}
}

func TestRenderMutationQuietPrintsNothing(t *testing.T) {
	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	env := mutation.Envelope{OK: true, Mode: "preview", Warnings: []string{"w"}}
	for _, asJSON := range []bool{true, false} {
		if err := renderMutation(cmd, &rootFlags{quiet: true, asJSON: asJSON}, env, nil); err != nil {
			t.Fatalf("renderMutation quiet: %v", err)
		}
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Fatalf("quiet output stdout=%q stderr=%q, want nothing", out.String(), errOut.String())
	}
}

// installOutcomeHooks installs the production per-op hooks for one test and
// restores the previous ones.
func installOutcomeHooks(t *testing.T) {
	t.Helper()
	prevJournal, prevOutcome, prevMirror, prevDelete := mutationJournalRecorder, mutationOutcomeRecorder, mirrorWriteThrough, mirrorPermanentDelete
	t.Cleanup(func() {
		mutationJournalRecorder, mutationOutcomeRecorder, mirrorWriteThrough, mirrorPermanentDelete = prevJournal, prevOutcome, prevMirror, prevDelete
	})
	InstallRuntimeHooks()
}

// killedRun drives runMutation with three ops and a process "kill" (a panic the
// test recovers) inside the third Apply, after the first two writes landed.
func killedRun(t *testing.T, kind string, change mutation.Change, keys [3]string) {
	t.Helper()
	ops := make([]mutation.Op, 0, len(keys))
	for i, key := range keys {
		index := i
		ops = append(ops, mutation.Op{
			ID: fmt.Sprintf("op%d", i+1), Key: key, Kind: kind,
			Changes: []mutation.Change{change},
			Apply: func() (string, any, error) {
				if index == 2 {
					panic("killed between ops")
				}
				return "applied", nil, nil
			},
		})
	}
	defer func() {
		if recover() == nil {
			t.Fatal("the third op did not die; the test did not simulate a kill")
		}
	}()
	_, _ = runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1, allowDestructive: true}, "test", ops)
}

// A run killed after some writes landed must leave each of them journaled and
// undoable: the run's own entry used to be written once, after the last op.
func TestRunMutationKilledMidRunKeepsLandedOpsJournaled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	installOutcomeHooks(t)

	killedRun(t, "tag_add", mutation.Change{Field: "tags", Add: "x", TagType: 0}, [3]string{"AAAA1111", "BBBB2222", "CCCC3333"})

	dir, err := journalDir()
	if err != nil {
		t.Fatalf("journalDir: %v", err)
	}
	entries, err := mutation.ListEntries(dir)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("journal has %d runs, want the one killed run: %+v", len(entries), entries)
	}
	run := entries[0]
	if !run.Partial || run.OK || run.Summary.Applied != 2 || len(run.Ops) != 2 {
		t.Fatalf("run = %+v, want a partial run with its two landed ops", run)
	}
	for i, want := range []string{"AAAA1111", "BBBB2222"} {
		if run.Ops[i].Key != want || run.Ops[i].Status != "applied" {
			t.Fatalf("op %d = %+v, want applied %s", i, run.Ops[i], want)
		}
	}
	if inverse, refused := mutation.InverseOps(run); len(inverse) != 2 || len(refused) != 0 {
		t.Fatalf("InverseOps = %d inverse, %d refused, want both landed ops undoable", len(inverse), len(refused))
	}
}

// A run that finishes keeps exactly one journal run: its final entry replaces
// the op-level records written along the way.
func TestRunMutationFinishedRunFoldsToItsFinalEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	installOutcomeHooks(t)

	ops := []mutation.Op{
		{ID: "op1", Key: "AAAA1111", Kind: "item_trash", Changes: []mutation.Change{{Field: "deleted", Add: true}}, Apply: func() (string, any, error) { return "applied", nil, nil }},
		{ID: "op2", Key: "BBBB2222", Kind: "item_trash", Changes: []mutation.Change{{Field: "deleted", Add: true}}, Apply: func() (string, any, error) { return "applied", nil, nil }},
	}
	env, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "test", ops)
	if err != nil {
		t.Fatalf("runMutation: %v", err)
	}
	dir, _ := journalDir()
	entries, err := mutation.ListEntries(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListEntries = %d entries, err %v, want one folded run", len(entries), err)
	}
	if entries[0].Partial || !entries[0].OK || len(entries[0].Ops) != 2 {
		t.Fatalf("entry = %+v, want the final, complete entry", entries[0])
	}
	if journal, _ := env.Journal.(map[string]any); journal["run_id"] != entries[0].RunID {
		t.Fatalf("envelope journal = %+v, want the run id %s shared with the op-level records", env.Journal, entries[0].RunID)
	}
}

// A permanent delete kills the same way: the items that were purged before the
// kill must already carry their deletion marker, or offline reads keep listing
// destroyed items and a rerun reports "nothing was deleted".
func TestRunMutationKilledMidRunMarksPurgedItems(t *testing.T) {
	db, dbPath := reapTestStore(t)
	for _, key := range []string{"AAAA1111", "BBBB2222", "CCCC3333"} {
		if _, _, err := db.UpsertBatch("items", []json.RawMessage{
			json.RawMessage(`{"key":"` + key + `","version":1,"data":{"key":"` + key + `","itemType":"book"}}`),
		}); err != nil {
			t.Fatalf("seed mirror: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	installOutcomeHooks(t)

	killedRun(t, "item_delete", mutation.Change{Field: "deleted", Add: true}, [3]string{"AAAA1111", "BBBB2222", "CCCC3333"})

	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db.Close()
	for key, wantGone := range map[string]bool{"AAAA1111": true, "BBBB2222": true, "CCCC3333": false} {
		marked, err := db.PendingDeletion("items", key)
		if err != nil {
			t.Fatalf("PendingDeletion(%s): %v", key, err)
		}
		if marked != wantGone || mirroredItemKeys(t, db, "items")[key] == wantGone {
			t.Fatalf("%s: deletion marked=%t mirrored=%t, want marked=%t mirrored=%t", key, marked, mirroredItemKeys(t, db, "items")[key], wantGone, !wantGone)
		}
	}
}

// Exit 13 is the "part of the run applied" signal, so a cancellation that
// reached no write at all must not claim it: an agent reading 13 goes looking
// for landed writes to reconcile, and a Ctrl-C or a caller's deadline before
// the first op leaves none.
func TestRunMutationCanceledWithNothingAppliedIsNotDegraded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	applied := 0
	apply := func() (string, any, error) {
		applied++
		return "applied", nil, nil
	}
	ops := []mutation.Op{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []mutation.Change{{Field: "title", Add: "A"}}, Apply: apply},
		{ID: "op2", Key: "K2", Kind: "test", Changes: []mutation.Change{{Field: "title", Add: "B"}}, Apply: apply},
	}
	env, err := runMutation(ctx, &rootFlags{yes: true, maxChanges: -1}, "test", ops)
	if err == nil {
		t.Fatal("runMutation err = nil, want the cancellation error")
	}
	if applied != 0 || env.Result == nil || env.Result.Summary.Applied != 0 {
		t.Fatalf("applied=%d summary=%+v, want nothing applied", applied, env.Result)
	}
	if got := ExitCode(err); got == 13 {
		t.Fatalf("exit code = 13 for a run that applied nothing; want the generic failure code")
	} else if got != 1 {
		t.Fatalf("exit code = %d, want 1", got)
	}
	if strings.Contains(err.Error(), "mutation incomplete") {
		t.Fatalf("err = %q, want it not to report a partial run", err)
	}
}

// The other half of the contract: once a write has landed, a cancellation IS
// partial and keeps exit 13.
func TestRunMutationCanceledAfterAWriteLandedStaysDegraded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	applied := 0
	ops := []mutation.Op{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []mutation.Change{{Field: "title", Add: "A"}}, Apply: func() (string, any, error) {
			applied++
			cancel()
			return "applied", nil, nil
		}},
		{ID: "op2", Key: "K2", Kind: "test", Changes: []mutation.Change{{Field: "title", Add: "B"}}, Apply: func() (string, any, error) {
			applied++
			return "applied", nil, nil
		}},
	}
	env, err := runMutation(ctx, &rootFlags{yes: true, maxChanges: -1}, "test", ops)
	if err == nil {
		t.Fatal("runMutation err = nil, want the cancellation error")
	}
	if applied != 1 || env.Result.Summary.Applied != 1 || env.Result.Summary.NotAttempted != 1 {
		t.Fatalf("applied=%d summary=%+v, want one landed write and one op not attempted", applied, env.Result.Summary)
	}
	if ExitCode(err) != 13 {
		t.Fatalf("exit code = %d, want 13 for a cancellation with a landed write", ExitCode(err))
	}
}

// A per-op journal record that keeps failing must stop the run before the next
// write. Otherwise op2's side effect runs with nothing durable describing op1,
// and a kill before the run's final entry loses op1's journal line — and, for a
// permanent delete, its ADR-0007 marker, so the next sync resurrects the item.
func TestRunMutationStopsWhenOutcomeRecordKeepsFailing(t *testing.T) {
	db, dbPath := reapTestStore(t)
	for _, key := range []string{"AAAA1111", "BBBB2222"} {
		if _, _, err := db.UpsertBatch("items", []json.RawMessage{
			json.RawMessage(`{"key":"` + key + `","version":1,"data":{"key":"` + key + `","itemType":"book"}}`),
		}); err != nil {
			t.Fatalf("seed mirror: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	installOutcomeHooks(t)
	records := 0
	mutationOutcomeRecorder = func(string, string, string, mutation.Op, mutation.ResultItem) error {
		records++
		return errors.New("journal disk full")
	}

	var applied []string
	ops := make([]mutation.Op, 0, 2)
	for _, key := range []string{"AAAA1111", "BBBB2222"} {
		target := key
		ops = append(ops, mutation.Op{
			ID: "op-" + target, Key: target, Kind: "item_delete",
			Changes: []mutation.Change{{Field: "deleted", Add: true}},
			Apply: func() (string, any, error) {
				applied = append(applied, target)
				return "applied", nil, nil
			},
		})
	}
	// --continue-on-error must not soften this: it tolerates a failed write,
	// not a landed one nothing recorded.
	env, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1, continueOnError: true}, "test", ops)
	if err == nil {
		t.Fatal("runMutation err = nil, want the unrecorded-write precondition")
	}
	if ExitCode(err) != 9 {
		t.Fatalf("exit code = %d (err %v), want 9 (precondition)", ExitCode(err), err)
	}
	if len(applied) != 1 || applied[0] != "AAAA1111" {
		t.Fatalf("applied %v, want the run stopped before the second write", applied)
	}
	if records != 1 {
		t.Fatalf("recorder called %d time(s), want 1: a failed record is not retried", records)
	}
	if env.OK || env.Result.Summary.Applied != 1 || env.Result.Summary.NotAttempted != 1 {
		t.Fatalf("env = %+v, want ok:false with one landed write and one op not attempted", env)
	}
	if !strings.Contains(err.Error(), "AAAA1111") {
		t.Fatalf("err = %q, want it to name the write whose record failed", err)
	}
	if len(env.Warnings) == 0 || !strings.Contains(strings.Join(env.Warnings, "|"), "1 operation(s) landed") {
		t.Fatalf("warnings = %v, want the landed set reported", env.Warnings)
	}

	// The delete landed, so its marker exists even though the record failed;
	// the op that never ran leaves no trace.
	reopened, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()
	marked, err := reopened.PendingDeletion("items", "AAAA1111")
	if err != nil || !marked {
		t.Fatalf("PendingDeletion(AAAA1111) = %t, %v, want the ADR-0007 marker written", marked, err)
	}
	if marked, err := reopened.PendingDeletion("items", "BBBB2222"); err != nil || marked {
		t.Fatalf("PendingDeletion(BBBB2222) = %t, %v, want no marker for the op that never ran", marked, err)
	}
}

// A record attempt can append its line and still report failure (the append
// landed, the sync did not). Re-recording would put the op in the journal
// twice, and a run killed before its final entry is read back from those
// op-level records, so journal show and journal undo would list the write
// twice. The op must appear once.
func TestRunMutationFailedOutcomeRecordIsNotDuplicated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	prevJournal, prevOutcome := mutationJournalRecorder, mutationOutcomeRecorder
	t.Cleanup(func() { mutationJournalRecorder, mutationOutcomeRecorder = prevJournal, prevOutcome })
	// No final entry: this stands in for a kill right after the run stops.
	mutationJournalRecorder = nil
	mutationOutcomeRecorder = func(runID, operation, mode string, op mutation.Op, item mutation.ResultItem) error {
		if err := recordMutationOutcome(runID, operation, mode, op, item); err != nil {
			return err
		}
		return errors.New("syncing journal entry: input/output error")
	}

	apply := func() (string, any, error) { return "applied", nil, nil }
	ops := []mutation.Op{
		{ID: "op1", Key: "AAAA2222", Kind: "tag_add", Changes: []mutation.Change{{Field: "tags", Add: "x"}}, Apply: apply},
		{ID: "op2", Key: "BBBB3333", Kind: "tag_add", Changes: []mutation.Change{{Field: "tags", Add: "x"}}, Apply: apply},
	}
	_, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "items.tags.add", ops)
	if ExitCode(err) != 9 {
		t.Fatalf("exit code = %d (err %v), want 9", ExitCode(err), err)
	}
	entries, err := mutation.ListEntries(helpersTestJournalDir(t))
	if err != nil || len(entries) != 1 {
		t.Fatalf("journal entries = %+v, %v; want the one interrupted run", entries, err)
	}
	if got := entries[0]; !got.Partial || len(got.Ops) != 1 || got.Ops[0].Key != "AAAA2222" || got.Summary.Applied != 1 {
		t.Fatalf("interrupted run = %+v, want op1 recorded exactly once", got)
	}
}

// A record failure on the last landed write has no later write to hold back,
// so it must not turn a fully applied run into exit 9: a caller reading 9
// fixes the environment and retries, and a retried create lands twice. The
// final entry repeats every op and is the run's record; the per-op failure is
// a warning.
func TestRunMutationLastOutcomeRecordFailureIsAWarning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	prevJournal, prevOutcome := mutationJournalRecorder, mutationOutcomeRecorder
	t.Cleanup(func() { mutationJournalRecorder, mutationOutcomeRecorder = prevJournal, prevOutcome })
	mutationJournalRecorder = recordMutationJournal
	mutationOutcomeRecorder = func(string, string, string, mutation.Op, mutation.ResultItem) error {
		return errors.New("journal disk full")
	}

	ops := []mutation.Op{{ID: "op1", Key: "AAAA1111", Kind: "tag_add", Changes: []mutation.Change{{Field: "tags", Add: "x"}}, Apply: func() (string, any, error) {
		return "applied", nil, nil
	}}}
	env, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "items.tags.add", ops)
	if err != nil {
		t.Fatalf("runMutation err = %v (exit %d), want nil: the only write landed and no write was held back", err, ExitCode(err))
	}
	if !env.OK || env.Result.Summary.Applied != 1 {
		t.Fatalf("env = %+v, want ok:true with the write applied", env)
	}
	if warnings := strings.Join(env.Warnings, "|"); !strings.Contains(warnings, "AAAA1111") || !strings.Contains(warnings, "no later write was held back") {
		t.Fatalf("warnings = %v, want the failed record reported", env.Warnings)
	}
	journal, _ := env.Journal.(map[string]any)
	runID, _ := journal["run_id"].(string)
	entry, err := mutation.ReadEntry(helpersTestJournalDir(t), runID)
	if err != nil || entry.Partial || len(entry.Ops) != 1 || entry.Ops[0].Key != "AAAA1111" || entry.Ops[0].Status != "applied" {
		t.Fatalf("journal entry %q = %+v, %v; want the final entry recording the landed write", runID, entry, err)
	}
}
