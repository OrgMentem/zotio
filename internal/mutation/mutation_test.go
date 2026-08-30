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
	if err == nil {
		t.Fatal("Run canceled err = nil, want non-nil")
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

func TestRunCanceledErrorIsWrappable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ops := []Op{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []Change{{Field: "title", Add: "A"}}, Apply: func() (string, any, error) {
			cancel()
			return "applied", nil, nil
		}},
		{ID: "op2", Key: "K2", Kind: "test", Changes: []Change{{Field: "title", Add: "B"}}, Apply: func() (string, any, error) {
			return "applied", nil, nil
		}},
	}
	_, err := Run(Options{Yes: true, MaxChanges: -1, Context: ctx}, "test", ops)
	if err == nil {
		t.Fatal("Run err = nil, want non-nil on cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) = false, err=%v", err)
	}
}
