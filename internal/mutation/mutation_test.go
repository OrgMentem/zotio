// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase3): engine tests for the promoted mutation package
// (state machine + gates), relocated from internal/cli/mutate_test.go.

package mutation

import (
	"errors"
	"strings"
	"testing"
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
