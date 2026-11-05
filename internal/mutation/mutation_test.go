// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Exercises mutation engine state-machine behavior and gates.

package mutation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestResolveMode(t *testing.T) {
	cases := []struct {
		name        string
		opts        Options
		wantApply   bool
		wantMode    string
		wantPreview string
	}{
		{name: "neither", opts: Options{}, wantMode: "preview", wantPreview: "default"},
		{name: "yes", opts: Options{Yes: true}, wantApply: true, wantMode: "apply"},
		{name: "dry run", opts: Options{DryRun: true}, wantMode: "preview", wantPreview: "dry_run"},
		{name: "yes dry run", opts: Options{Yes: true, DryRun: true}, wantMode: "preview", wantPreview: "dry_run"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveMode(tt.opts)
			if got.Apply != tt.wantApply || got.Mode != tt.wantMode || got.PreviewReason != tt.wantPreview {
				t.Fatalf("mode = %+v, want apply=%v mode=%q preview=%q", got, tt.wantApply, tt.wantMode, tt.wantPreview)
			}
		})
	}
}

func TestEffectiveMaxChanges(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want int
	}{
		{name: "unset", opts: Options{MaxChanges: -1}, want: 500},
		{name: "agent", opts: Options{MaxChanges: -1, Agent: true}, want: 50},
		{name: "explicit", opts: Options{MaxChanges: 7, Agent: true}, want: 7},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveMaxChanges(tt.opts); got != tt.want {
				t.Fatalf("EffectiveMaxChanges = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCheckGates(t *testing.T) {
	ops := []Op{
		{ID: "one", Changes: []Change{{Field: "title", Add: "A"}}},
		{ID: "two", Changes: []Change{{Field: "title", Add: "B"}}},
	}
	if err := CheckGates(Options{MaxChanges: 2}, ops); err != nil {
		t.Fatalf("under cap gate = %+v, want nil", err)
	}
	if err := CheckGates(Options{MaxChanges: 1}, ops); err == nil || err.Code != "max_changes_exceeded" || !strings.Contains(err.Message, "--max-changes") {
		t.Fatalf("over cap gate = %+v, want max_changes_exceeded mentioning --max-changes", err)
	}

	destructive := []Op{{ID: "delete", Changes: []Change{{Field: "collections", Remove: "C"}}, Destructive: true}}
	if err := CheckGates(Options{MaxChanges: -1}, destructive); err == nil || err.Code != "destructive_opt_in_required" || !strings.Contains(err.Message, "--allow-destructive") {
		t.Fatalf("destructive gate = %+v, want destructive opt-in", err)
	}
	if err := CheckGates(Options{MaxChanges: -1, AllowDestructive: true}, destructive); err != nil {
		t.Fatalf("destructive with opt-in gate = %+v, want nil", err)
	}
}

// TestRunGateErrorIncludesActionableMessage pins zotio-085603ea0268d08f.
// Without this fix, Run returns the bare "max_changes_exceeded" code, which hides the cap and remediation hint.
func TestRunGateErrorIncludesActionableMessage(t *testing.T) {
	ops := []Op{
		{ID: "one", Changes: []Change{{Field: "title", Add: "A"}}},
		{ID: "two", Changes: []Change{{Field: "title", Add: "B"}}},
	}

	env, err := Run(Options{Yes: true, MaxChanges: 1}, "test", ops)
	if err == nil {
		t.Fatal("Run gate err = nil, want max-changes refusal")
	}
	for _, want := range []string{"cap of 1", "raise the limit with --max-changes"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Run gate error = %q, want %q", err, want)
		}
	}
	var gateErr *Error
	if !errors.As(err, &gateErr) || gateErr != env.Error {
		t.Fatalf("Run gate error = %v, envelope error = %+v, want the same typed gate error", err, env.Error)
	}
	if env.OK || env.Error == nil || env.Error.Code != "max_changes_exceeded" {
		t.Fatalf("Run gate envelope = %+v, want unchanged max-changes refusal", env)
	}
}

func TestRunPreviewDoesNotApply(t *testing.T) {
	called := 0
	ops := []Op{{ID: "op", Key: "K", Kind: "test", Changes: []Change{{Field: "title", Add: "T"}}, Apply: func() (string, any, error) {
		called++
		return "applied", nil, nil
	}}}
	env, err := Run(Options{MaxChanges: -1}, "test", ops)
	if err != nil {
		t.Fatalf("Run preview err = %v", err)
	}
	if !env.OK || env.Mode != "preview" || env.PreviewReason != "default" || env.Result != nil {
		t.Fatalf("preview envelope = %+v", env)
	}
	if called != 0 {
		t.Fatalf("apply called %d time(s), want 0", called)
	}
}

func TestRunApplySuccess(t *testing.T) {
	ops := []Op{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []Change{{Field: "title", Add: "T"}}, Apply: func() (string, any, error) { return "applied", nil, nil }},
		{ID: "op2", Key: "K2", Kind: "test"},
	}
	env, err := Run(Options{Yes: true, MaxChanges: -1}, "test", ops)
	if err != nil {
		t.Fatalf("Run apply err = %v", err)
	}
	if !env.OK || env.Result == nil {
		t.Fatalf("apply envelope = %+v", env)
	}
	if env.Result.Summary.Applied != 1 || env.Result.Summary.NoOp != 1 || env.Result.Summary.Attempted != 2 {
		t.Fatalf("summary = %+v", env.Result.Summary)
	}
}
func TestRunAdoptsCreatedKeyAndDoesNotUseTypePlaceholder(t *testing.T) {
	ops := []Op{{
		ID: "create", Key: "journalArticle", Kind: "item_create",
		Changes: []Change{{Field: "item", Add: map[string]any{"itemType": "journalArticle"}}},
		Apply: func() (string, any, error) {
			return "applied", map[string]any{"key": "REALKEY1"}, nil
		},
	}}
	env, err := Run(Options{Yes: true, MaxChanges: -1}, "items.new", ops)
	if err != nil {
		t.Fatalf("Run create err = %v", err)
	}
	if got := env.Result.Items[0].Key; got != "REALKEY1" {
		t.Fatalf("result key = %q, want REALKEY1", got)
	}
	entry, ok := BuildJournalEntry(env, time.Now())
	if !ok || entry.Ops[0].Key != "REALKEY1" {
		t.Fatalf("journal entry = %+v, want real created key", entry)
	}
}

func TestRunApplyConflictFailFast(t *testing.T) {
	ops := []Op{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []Change{{Field: "title", Add: "A"}}, Apply: func() (string, any, error) { return "applied", nil, nil }},
		{ID: "op2", Key: "K2", Kind: "test", Changes: []Change{{Field: "title", Add: "B"}}, Apply: func() (string, any, error) { return "conflict", "stale version", errors.New("precondition failed") }},
		{ID: "op3", Key: "K3", Kind: "test", Changes: []Change{{Field: "title", Add: "C"}}, Apply: func() (string, any, error) { return "applied", nil, nil }},
	}
	env, err := Run(Options{Yes: true, MaxChanges: -1}, "test", ops)
	if err == nil {
		t.Fatal("Run conflict err = nil, want non-nil")
	}
	if env.OK || env.Result == nil {
		t.Fatalf("conflict envelope = %+v", env)
	}
	if env.Result.Summary.Applied != 1 || env.Result.Summary.Conflicts != 1 || env.Result.Summary.NotAttempted != 1 {
		t.Fatalf("summary = %+v", env.Result.Summary)
	}
	if got := env.Result.Items[2].Status; got != "not_attempted" {
		t.Fatalf("third status = %q, want not_attempted", got)
	}
}

// Cancelling mid-run stops the engine before the next operation; the operation
// already in flight is aborted by its own transport.
func TestRunApplyStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	applied := 0
	ops := []Op{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []Change{{Field: "title", Add: "A"}}, Apply: func() (string, any, error) {
			applied++
			cancel()
			return "applied", nil, nil
		}},
		{ID: "op2", Key: "K2", Kind: "test", Changes: []Change{{Field: "title", Add: "B"}}, Apply: func() (string, any, error) {
			applied++
			return "applied", nil, nil
		}},
	}
	env, err := Run(Options{Yes: true, MaxChanges: -1, Context: ctx}, "test", ops)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) = false, err=%v", err)
	}
	if applied != 1 {
		t.Fatalf("applied %d operation(s) after cancellation, want 1", applied)
	}
	if env.Result.Summary.Applied != 1 || env.Result.Summary.NotAttempted != 1 {
		t.Fatalf("summary = %+v", env.Result.Summary)
	}
	if got := env.Result.Items[1].Status; got != "not_attempted" {
		t.Fatalf("second status = %q, want not_attempted", got)
	}
	if len(env.Warnings) != 1 || !strings.Contains(env.Warnings[0], "canceled") {
		t.Fatalf("warnings = %v, want a cancellation warning", env.Warnings)
	}
}

// An op whose write left with an earlier op's request already has an outcome.
// Cancelling must collect it, with its key, while an op that was never sent
// stays not_attempted.
func TestRunApplyCollectsDispatchedOutcomeAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sent := false
	ops := []Op{
		{ID: "op1", Kind: "test", Changes: []Change{{Field: "title", Add: "A"}}, Apply: func() (string, any, error) {
			sent = true
			cancel()
			return "applied", map[string]any{"key": "KEY1"}, nil
		}},
		{ID: "op2", Kind: "test", Changes: []Change{{Field: "title", Add: "B"}},
			Dispatched: func() bool { return sent },
			Apply:      func() (string, any, error) { return "applied", map[string]any{"key": "KEY2"}, nil }},
		{ID: "op3", Kind: "test", Changes: []Change{{Field: "title", Add: "C"}},
			Dispatched: func() bool { return false },
			Apply: func() (string, any, error) {
				t.Error("an op that was never dispatched ran after cancellation")
				return "applied", nil, nil
			}},
	}
	env, err := Run(Options{Yes: true, MaxChanges: -1, ContinueOnError: true, Context: ctx}, "test", ops)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want cancellation", err)
	}
	items := env.Result.Items
	if items[1].Status != "applied" || items[1].Key != "KEY2" || items[2].Status != "not_attempted" {
		t.Fatalf("items = %+v, want the dispatched op applied with its key and the unsent op not_attempted", items)
	}
	if env.Result.Summary.Applied != 2 || env.Result.Summary.NotAttempted != 1 {
		t.Fatalf("summary = %+v, want 2 applied and 1 not attempted", env.Result.Summary)
	}
}

// A nil Context must never cancel: most callers do not set one.
func TestRunApplyNilContextNeverCancels(t *testing.T) {
	ops := []Op{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []Change{{Field: "title", Add: "A"}}, Apply: func() (string, any, error) { return "applied", nil, nil }},
		{ID: "op2", Key: "K2", Kind: "test", Changes: []Change{{Field: "title", Add: "B"}}, Apply: func() (string, any, error) { return "applied", nil, nil }},
	}
	env, err := Run(Options{Yes: true, MaxChanges: -1}, "test", ops)
	if err != nil {
		t.Fatalf("Run err = %v, want nil", err)
	}
	if env.Result.Summary.Applied != 2 {
		t.Fatalf("summary = %+v, want both applied", env.Result.Summary)
	}
}

func TestRunApplyContinueOnError(t *testing.T) {
	ops := []Op{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []Change{{Field: "title", Add: "A"}}, Apply: func() (string, any, error) { return "failed", "transport", errors.New("network") }},
		{ID: "op2", Key: "K2", Kind: "test", Changes: []Change{{Field: "title", Add: "B"}}, Apply: func() (string, any, error) { return "applied", nil, nil }},
	}
	env, err := Run(Options{Yes: true, MaxChanges: -1, ContinueOnError: true}, "test", ops)
	if err == nil {
		t.Fatal("Run continue-on-error err = nil, want non-nil")
	}
	if env.OK || env.Result == nil {
		t.Fatalf("continue envelope = %+v", env)
	}
	if env.Result.Summary.Failed != 1 || env.Result.Summary.Applied != 1 || env.Result.Summary.NotAttempted != 0 {
		t.Fatalf("summary = %+v", env.Result.Summary)
	}
}

func TestRunApplyContinueOnErrorMaxFailures(t *testing.T) {
	executed := 0
	ops := []Op{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []Change{{Field: "title", Add: "A"}}, Apply: func() (string, any, error) {
			executed++
			return "failed", "first failure", errors.New("first failure")
		}},
		{ID: "op2", Key: "K2", Kind: "test", Changes: []Change{{Field: "title", Add: "B"}}, Apply: func() (string, any, error) {
			executed++
			return "failed", "second failure", errors.New("second failure")
		}},
		{ID: "op3", Key: "K3", Kind: "test", Changes: []Change{{Field: "title", Add: "C"}}, Apply: func() (string, any, error) {
			executed++
			return "applied", nil, nil
		}},
		{ID: "op4", Key: "K4", Kind: "test", Changes: []Change{{Field: "title", Add: "D"}}, Apply: func() (string, any, error) {
			executed++
			return "applied", nil, nil
		}},
	}

	env, err := Run(Options{Yes: true, MaxChanges: -1, ContinueOnError: true, MaxFailures: 2}, "test", ops)
	if err == nil {
		t.Fatal("Run max-failures err = nil, want non-nil")
	}
	if executed != 2 {
		t.Fatalf("executed = %d, want exactly 2 operations before the failure ceiling", executed)
	}
	if env.OK || env.Result == nil {
		t.Fatalf("max-failures envelope = %+v, want an incomplete result", env)
	}
	if env.Result.Summary.Failed != 2 || env.Result.Summary.Applied != 0 || env.Result.Summary.NotAttempted != 2 {
		t.Fatalf("summary = %+v, want 2 failed and 2 not attempted", env.Result.Summary)
	}
	for i := 2; i < 4; i++ {
		if got := env.Result.Items[i].Status; got != "not_attempted" {
			t.Errorf("item %d status = %q, want not_attempted", i, got)
		}
	}
}

func TestRunApplyAppliedWithErrorIsFailed(t *testing.T) {
	op := Op{ID: "x", Key: "K1", Kind: "test", Changes: []Change{{Field: "title", Add: "A"}}, Apply: func() (string, any, error) {
		return "applied", nil, errors.New("post-condition write failed")
	}}
	env, err := Run(Options{Yes: true, MaxChanges: -1}, "test", []Op{op})
	if err == nil {
		t.Fatal("Run err = nil, want non-nil for applied-with-error")
	}
	if env.OK {
		t.Fatal("env.OK = true, want false when applied carries an error")
	}
	if env.Result == nil {
		t.Fatal("Result is nil")
	}
	if got := env.Result.Items[0].Status; got != "failed" {
		t.Fatalf("status = %q, want %q", got, "failed")
	}
	if env.Result.Summary.Failed != 1 || env.Result.Summary.Applied != 0 {
		t.Fatalf("summary = %+v, want Failed=1 Applied=0", env.Result.Summary)
	}
}

func TestRunApplyAppliedWithReasonRemainsApplied(t *testing.T) {
	reason := map[string]any{"message": "created but filing failed", "key": "NEWKEY"}
	op := Op{ID: "y", Key: "journalArticle", Kind: "item_create", Changes: []Change{{Field: "item", Add: map[string]any{"itemType": "journalArticle"}}}, Apply: func() (string, any, error) {
		return "applied", reason, nil
	}}
	env, err := Run(Options{Yes: true, MaxChanges: -1}, "test", []Op{op})
	if err != nil {
		t.Fatalf("Run err = %v, want nil for applied-with-reason (no error)", err)
	}
	if !env.OK {
		t.Fatalf("env.OK = false, want true for partial-success with nil error; envelope=%+v", env)
	}
	if got := env.Result.Items[0].Status; got != "applied" {
		t.Fatalf("status = %q, want applied", got)
	}
	if env.Result.Summary.Applied != 1 {
		t.Fatalf("summary = %+v, want Applied=1", env.Result.Summary)
	}
	if env.Result.Items[0].Reason == nil {
		t.Fatal("reason is nil, want partial-success reason preserved")
	}
}

// A caller whose durable record of a landed write failed stops the run before
// the next op's side effect. --continue-on-error does not soften that: the flag
// tolerates a failed write, not an unrecorded one that landed.
func TestRunObservedObserverErrorStopsBeforeNextOp(t *testing.T) {
	var applied []string
	apply := func(key string) func() (string, any, error) {
		return func() (string, any, error) {
			applied = append(applied, key)
			return "applied", nil, nil
		}
	}
	ops := []Op{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []Change{{Field: "title", Add: "A"}}, Apply: apply("K1")},
		{ID: "op2", Key: "K2", Kind: "test", Changes: []Change{{Field: "title", Add: "B"}}, Apply: apply("K2")},
		{ID: "op3", Key: "K3", Kind: "test", Changes: []Change{{Field: "title", Add: "C"}}, Apply: apply("K3")},
	}
	recordErr := errors.New("journal disk full")
	calls := 0
	env, err := RunObserved(Options{Yes: true, MaxChanges: -1, ContinueOnError: true}, "test", ops, func(op Op, item ResultItem) error {
		calls++
		return recordErr
	})
	if !errors.Is(err, recordErr) {
		t.Fatalf("err = %v, want the observer's own error", err)
	}
	if len(applied) != 1 || applied[0] != "K1" {
		t.Fatalf("applied %v, want only the first op's write", applied)
	}
	if calls != 1 {
		t.Fatalf("observer called %d time(s), want 1 (the run stops at the first failure)", calls)
	}
	if env.OK {
		t.Fatalf("env.OK = true, want false for a run stopped with an unrecorded write")
	}
	if env.Result.Summary.Applied != 1 || env.Result.Summary.NotAttempted != 2 {
		t.Fatalf("summary = %+v, want 1 applied and 2 not attempted", env.Result.Summary)
	}
	if env.Result.Items[0].Key != "K1" || env.Result.Items[0].Status != "applied" {
		t.Fatalf("first item = %+v, want the landed write named", env.Result.Items[0])
	}
	if len(env.Warnings) != 1 || !strings.Contains(env.Warnings[0], "op1") || !strings.Contains(env.Warnings[0], "1 operation(s) landed") {
		t.Fatalf("warnings = %v, want one naming the op it stopped after and what landed", env.Warnings)
	}
}

// An observer failure with no later write left to hold back stopped nothing:
// the run's outcome stands and the failure is a warning. That covers the last
// write, one followed only by planned no-ops (which write nothing and still
// run), and a batch whose later writes were dispatched with the first, so they
// landed and are still collected and observed.
func TestRunObservedObserverErrorWithNothingToHoldBackIsAWarning(t *testing.T) {
	applied := func() (string, any, error) { return "applied", nil, nil }
	dispatched := func() bool { return true }
	cases := []struct {
		name         string
		ops          []Op
		wantApplied  int
		wantNoOp     int
		wantObserved int
	}{
		{name: "last write", wantApplied: 1, wantObserved: 1, ops: []Op{
			{ID: "op1", Key: "K1", Kind: "test", Changes: []Change{{Field: "title", Add: "A"}}, Apply: applied},
		}},
		{name: "followed by a planned no-op", wantApplied: 1, wantNoOp: 1, wantObserved: 1, ops: []Op{
			{ID: "op1", Key: "K1", Kind: "test", Changes: []Change{{Field: "title", Add: "A"}}, Apply: applied},
			{ID: "op2", Key: "K2", Kind: "test"},
		}},
		{name: "later writes already dispatched", wantApplied: 2, wantObserved: 2, ops: []Op{
			{ID: "op1", Key: "K1", Kind: "test", Changes: []Change{{Field: "title", Add: "A"}}, Apply: applied},
			{ID: "op2", Key: "K2", Kind: "test", Changes: []Change{{Field: "title", Add: "B"}}, Apply: applied, Dispatched: dispatched},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var observed []string
			env, err := RunObserved(Options{Yes: true, MaxChanges: -1}, "test", tc.ops, func(op Op, _ ResultItem) error {
				observed = append(observed, op.ID)
				if op.ID == "op1" {
					return errors.New("journal disk full")
				}
				return nil
			})
			if err != nil || !env.OK {
				t.Fatalf("env.OK = %t, err = %v, want the run's own outcome (ok, no error)", env.OK, err)
			}
			if s := env.Result.Summary; s.Applied != tc.wantApplied || s.NoOp != tc.wantNoOp || s.NotAttempted != 0 {
				t.Fatalf("summary = %+v, want %d applied, %d no-op, none held back", s, tc.wantApplied, tc.wantNoOp)
			}
			if len(observed) != tc.wantObserved {
				t.Fatalf("observed %v, want %d landed op(s) observed", observed, tc.wantObserved)
			}
			if len(env.Warnings) != 1 || !strings.Contains(env.Warnings[0], "op1") || !strings.Contains(env.Warnings[0], "journal disk full") || !strings.Contains(env.Warnings[0], "no later write was held back") {
				t.Fatalf("warnings = %v, want one naming the op and the record failure", env.Warnings)
			}
		})
	}
}

// Cancellation before the first write leaves nothing half-done, so the engine
// must not borrow the partial run's "mutation incomplete" wording: callers read
// that as "part of the run applied" and go looking for writes to reconcile.
func TestRunApplyCanceledBeforeFirstWriteSaysNothingLanded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	applied := 0
	ops := []Op{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []Change{{Field: "title", Add: "A"}}, Apply: func() (string, any, error) {
			applied++
			return "applied", nil, nil
		}},
		{ID: "op2", Key: "K2", Kind: "test", Changes: []Change{{Field: "title", Add: "B"}}, Apply: func() (string, any, error) {
			applied++
			return "applied", nil, nil
		}},
	}
	env, err := Run(Options{Yes: true, MaxChanges: -1, Context: ctx}, "test", ops)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if applied != 0 || env.Result.Summary.Applied != 0 || env.Result.Summary.NotAttempted != 2 {
		t.Fatalf("applied=%d summary=%+v, want nothing applied and both ops not attempted", applied, env.Result.Summary)
	}
	if strings.Contains(err.Error(), "mutation incomplete") {
		t.Fatalf("err = %q, want it not to claim an incomplete (part-applied) run", err)
	}
	if !strings.Contains(err.Error(), "before any write landed") {
		t.Fatalf("err = %q, want it to say no write landed", err)
	}
}
