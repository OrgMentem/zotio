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
