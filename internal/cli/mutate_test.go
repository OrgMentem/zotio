// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean write-safety): cover the shared mutation state machine and gates.

package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestResolveMutationMode(t *testing.T) {
	cases := []struct {
		name        string
		flags       rootFlags
		wantApply   bool
		wantMode    string
		wantPreview string
	}{
		{name: "neither", flags: rootFlags{}, wantMode: "preview", wantPreview: "default"},
		{name: "yes", flags: rootFlags{yes: true}, wantApply: true, wantMode: "apply"},
		{name: "dry run", flags: rootFlags{dryRun: true}, wantMode: "preview", wantPreview: "dry_run"},
		{name: "yes dry run", flags: rootFlags{yes: true, dryRun: true}, wantMode: "preview", wantPreview: "dry_run"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveMutationMode(&tt.flags)
			if got.Apply != tt.wantApply || got.Mode != tt.wantMode || got.PreviewReason != tt.wantPreview {
				t.Fatalf("mode = %+v, want apply=%v mode=%q preview=%q", got, tt.wantApply, tt.wantMode, tt.wantPreview)
			}
		})
	}
}

func TestEffectiveMaxChanges(t *testing.T) {
	cases := []struct {
		name  string
		flags rootFlags
		want  int
	}{
		{name: "unset", flags: rootFlags{maxChanges: -1}, want: 500},
		{name: "agent", flags: rootFlags{maxChanges: -1, agent: true}, want: 50},
		{name: "explicit", flags: rootFlags{maxChanges: 7, agent: true}, want: 7},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveMaxChanges(&tt.flags); got != tt.want {
				t.Fatalf("effectiveMaxChanges = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCheckWriteGates(t *testing.T) {
	ops := []plannedOp{
		{ID: "one", Changes: []mutationChange{{Field: "title", Add: "A"}}},
		{ID: "two", Changes: []mutationChange{{Field: "title", Add: "B"}}},
	}
	if err := checkWriteGates(&rootFlags{maxChanges: 2}, ops); err != nil {
		t.Fatalf("under cap gate = %+v, want nil", err)
	}
	if err := checkWriteGates(&rootFlags{maxChanges: 1}, ops); err == nil || err.Code != "max_changes_exceeded" || !strings.Contains(err.Message, "--max-changes") {
		t.Fatalf("over cap gate = %+v, want max_changes_exceeded mentioning --max-changes", err)
	}

	destructive := []plannedOp{{ID: "delete", Changes: []mutationChange{{Field: "collections", Remove: "C"}}, Destructive: true}}
	if err := checkWriteGates(&rootFlags{maxChanges: -1}, destructive); err == nil || err.Code != "destructive_opt_in_required" || !strings.Contains(err.Message, "--allow-destructive") {
		t.Fatalf("destructive gate = %+v, want destructive opt-in", err)
	}
	if err := checkWriteGates(&rootFlags{maxChanges: -1, allowDestructive: true}, destructive); err != nil {
		t.Fatalf("destructive with opt-in gate = %+v, want nil", err)
	}
}

func TestRunMutationPreviewDoesNotApply(t *testing.T) {
	called := 0
	ops := []plannedOp{{ID: "op", Key: "K", Kind: "test", Changes: []mutationChange{{Field: "title", Add: "T"}}, apply: func() (string, any, error) {
		called++
		return "applied", nil, nil
	}}}
	env, err := runMutation(context.Background(), &rootFlags{maxChanges: -1}, "test", ops)
	if err != nil {
		t.Fatalf("runMutation preview err = %v", err)
	}
	if !env.OK || env.Mode != "preview" || env.PreviewReason != "default" || env.Result != nil {
		t.Fatalf("preview envelope = %+v", env)
	}
	if called != 0 {
		t.Fatalf("apply called %d time(s), want 0", called)
	}
}

func TestRunMutationApplySuccess(t *testing.T) {
	ops := []plannedOp{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []mutationChange{{Field: "title", Add: "T"}}, apply: func() (string, any, error) { return "applied", nil, nil }},
		{ID: "op2", Key: "K2", Kind: "test"},
	}
	env, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "test", ops)
	if err != nil {
		t.Fatalf("runMutation apply err = %v", err)
	}
	if !env.OK || env.Result == nil {
		t.Fatalf("apply envelope = %+v", env)
	}
	if env.Result.Summary.Applied != 1 || env.Result.Summary.NoOp != 1 || env.Result.Summary.Attempted != 2 {
		t.Fatalf("summary = %+v", env.Result.Summary)
	}
}

func TestRunMutationApplyConflictFailFast(t *testing.T) {
	ops := []plannedOp{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []mutationChange{{Field: "title", Add: "A"}}, apply: func() (string, any, error) { return "applied", nil, nil }},
		{ID: "op2", Key: "K2", Kind: "test", Changes: []mutationChange{{Field: "title", Add: "B"}}, apply: func() (string, any, error) { return "conflict", "stale version", errors.New("precondition failed") }},
		{ID: "op3", Key: "K3", Kind: "test", Changes: []mutationChange{{Field: "title", Add: "C"}}, apply: func() (string, any, error) { return "applied", nil, nil }},
	}
	env, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "test", ops)
	if err == nil {
		t.Fatal("runMutation conflict err = nil, want non-nil")
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

func TestRunMutationApplyContinueOnError(t *testing.T) {
	ops := []plannedOp{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []mutationChange{{Field: "title", Add: "A"}}, apply: func() (string, any, error) { return "failed", "transport", errors.New("network") }},
		{ID: "op2", Key: "K2", Kind: "test", Changes: []mutationChange{{Field: "title", Add: "B"}}, apply: func() (string, any, error) { return "applied", nil, nil }},
	}
	env, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1, continueOnError: true}, "test", ops)
	if err == nil {
		t.Fatal("runMutation continue-on-error err = nil, want non-nil")
	}
	if env.OK || env.Result == nil {
		t.Fatalf("continue envelope = %+v", env)
	}
	if env.Result.Summary.Failed != 1 || env.Result.Summary.Applied != 1 || env.Result.Summary.NotAttempted != 0 {
		t.Fatalf("summary = %+v", env.Result.Summary)
	}
}
