// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean roadmap-phase3): adapter tests — flags->mutation.Options mapping
// and delegation. The engine state machine + gates are covered in
// internal/mutation/mutation_test.go.

package cli

import (
	"context"
	"testing"

	"zotio/internal/mutation"
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
