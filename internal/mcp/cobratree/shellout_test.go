// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestAcquireMirroredMuRespectsCancelledContext(t *testing.T) {
	mirroredCommandMu.Lock()
	defer mirroredCommandMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := acquireMirroredMu(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("acquireMirroredMu with cancelled context should return error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("acquireMirroredMu error = %v, want context.Canceled", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("acquireMirroredMu took %v, should return promptly when context already cancelled", elapsed)
	}
}

func TestRunMirroredInProcessRespectsCancelledContextWhileLocked(t *testing.T) {
	mirroredCommandMu.Lock()
	defer mirroredCommandMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	result := runMirroredInProcess(ctx, func() *cobra.Command { return nil }, nil, nil)
	elapsed := time.Since(start)

	if result == nil || !result.IsError {
		t.Fatalf("runMirroredInProcess with cancelled context should return error result")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("runMirroredInProcess took %v, should return promptly when context cancelled", elapsed)
	}
}

func TestOrchestrationRootWithContextRespectsCancellation(t *testing.T) {
	mirroredCommandMu.Lock()
	defer mirroredCommandMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	root, err := orchestrationRootWithContext(ctx, func() *cobra.Command { return &cobra.Command{Use: "zotio"} })
	if err == nil {
		t.Fatalf("orchestrationRootWithContext with cancelled context should return error")
	}
	if root != nil {
		t.Fatalf("orchestrationRootWithContext should return nil root on cancel, got %v", root)
	}
}

func TestCancelledContextDoesNotWaitForLock(t *testing.T) {
	mirroredCommandMu.Lock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- acquireMirroredMu(ctx)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("acquireMirroredMu should return context error when waiting and context expires")
		}
		mirroredCommandMu.Unlock()
		time.Sleep(50 * time.Millisecond)
		if !mirroredCommandMu.TryLock() {
			t.Fatalf("mirroredCommandMu should be unlocked after cancelled acquisition")
		}
		mirroredCommandMu.Unlock()
	case <-time.After(2 * time.Second):
		mirroredCommandMu.Unlock()
		t.Fatalf("acquireMirroredMu did not return within timeout - still blocking on mutex")
	}
}

func TestCLIArgsFromMCPRepeatsArrayFlagsAndPreservesFalse(t *testing.T) {
	args := map[string]any{
		"enabled": false,
		"tag":     []any{"comma,value", "two words", ""},
	}

	want := []string{
		"--enabled=false",
		"--tag", "comma,value",
		"--tag", "two words",
		"--tag", "",
	}
	if got := cliArgsFromMCP(args); !reflect.DeepEqual(got, want) {
		t.Fatalf("cliArgsFromMCP() = %#v, want %#v", got, want)
	}
}

func TestSplitShellArgsSupportsMixedQuotingAndEscapes(t *testing.T) {
	input := `plain "double quoted" 'single quoted' a\ b pre" two"post "mix\"quote" 'literal\slash' "" ''`
	want := []string{
		"plain",
		"double quoted",
		"single quoted",
		"a b",
		"pre twopost",
		`mix"quote`,
		`literal\slash`,
		"",
		"",
	}
	if got := splitShellArgs(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("splitShellArgs(%q) = %#v, want %#v", input, got, want)
	}
}
