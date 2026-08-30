// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"strings"
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

// TestRunMutationKeepsEngineErrorWhenJournalingFails pins error precedence. A
// journal failure downgrades a SUCCESSFUL run to degraded (exit 13). It must not
// overwrite a real engine failure: doing so replaced "mutation incomplete" with
// a generic degraded error naming neither the conflict nor the journal problem,
// and CI and MCP callers branch on that distinction.
func TestRunMutationKeepsEngineErrorWhenJournalingFails(t *testing.T) {
	mutationJournalRecorder = func(*mutation.Envelope) error { return errors.New("journal disk full") }
	t.Cleanup(func() { mutationJournalRecorder = nil })

	ops := []mutation.Op{
		{ID: "op1", Key: "K1", Kind: "test", Changes: []mutation.Change{{Field: "title", Add: "A"}}, Apply: func() (string, any, error) {
			return "applied", nil, nil
		}},
		{ID: "op2", Key: "K2", Kind: "test", Changes: []mutation.Change{{Field: "title", Add: "B"}}, Apply: func() (string, any, error) {
			return "conflict", "stale version", errors.New("precondition failed")
		}},
	}
	env, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "test", ops)
	if err == nil {
		t.Fatal("runMutation err = nil, want the engine's conflict error")
	}
	var cliErr *cliError
	if errors.As(err, &cliErr) {
		t.Fatalf("engine error was replaced by a cliError (code %d): %v", cliErr.code, err)
	}
	if err.Error() != "mutation incomplete" {
		t.Fatalf("err = %q, want the engine's own %q", err.Error(), "mutation incomplete")
	}
	// The journal failure is still reported, as a warning on the envelope.
	if len(env.Warnings) == 0 || !strings.Contains(env.Warnings[len(env.Warnings)-1], "not journaled") {
		t.Fatalf("warnings = %v, want the journal failure recorded", env.Warnings)
	}
}

// A journal failure on an otherwise clean run still degrades to exit 13.
func TestRunMutationDegradesCleanRunWhenJournalingFails(t *testing.T) {
	mutationJournalRecorder = func(*mutation.Envelope) error { return errors.New("journal disk full") }
	t.Cleanup(func() { mutationJournalRecorder = nil })

	ops := []mutation.Op{{ID: "op", Key: "K", Kind: "test", Changes: []mutation.Change{{Field: "title", Add: "A"}}, Apply: func() (string, any, error) {
		return "applied", nil, nil
	}}}
	if _, err := runMutation(context.Background(), &rootFlags{yes: true, maxChanges: -1}, "test", ops); err == nil {
		t.Fatal("runMutation err = nil, want a degraded error")
	} else {
		var cliErr *cliError
		if !errors.As(err, &cliErr) || cliErr.code != 13 {
			t.Fatalf("err = %v, want a cliError with code 13", err)
		}
	}
}
