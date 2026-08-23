// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// holdMirroredSlot takes the mirrored-command slot for the duration of the
// test, the way a long-running import holds it in production.
func holdMirroredSlot(t *testing.T) {
	t.Helper()
	if err := acquireMirroredSlot(context.Background()); err != nil {
		t.Fatalf("acquireMirroredSlot: %v", err)
	}
	t.Cleanup(releaseMirroredSlot)
}

func TestAcquireMirroredSlotRespectsCancelledContext(t *testing.T) {
	holdMirroredSlot(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := acquireMirroredSlot(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("acquireMirroredSlot with cancelled context should return error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("acquireMirroredSlot error = %v, want context.Canceled", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("acquireMirroredSlot took %v, should return promptly when context already cancelled", elapsed)
	}
}

func TestRunMirroredInProcessRespectsCancelledContextWhileLocked(t *testing.T) {
	holdMirroredSlot(t)

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
	holdMirroredSlot(t)

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

func TestCancelledContextDoesNotWaitForSlot(t *testing.T) {
	holdMirroredSlot(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- acquireMirroredSlot(ctx)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("acquireMirroredSlot should return context error when waiting and context expires")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("acquireMirroredSlot did not return within timeout - still blocking on the slot")
	}
}

// TestCancelledAcquisitionsLeaveNoGoroutines pins the property the mutex
// version could not have: a caller that gives up while the slot is held must
// leave nothing behind. The mutex version parked a waiter goroutine per caller
// plus a hand-off goroutine per cancellation, all of which survived until the
// holder released — minutes, for a large import or sync.
func TestCancelledAcquisitionsLeaveNoGoroutines(t *testing.T) {
	holdMirroredSlot(t)

	const callers = 100
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
			defer cancel()
			if err := acquireMirroredSlot(ctx); err == nil {
				t.Error("acquireMirroredSlot succeeded while the slot was held")
				releaseMirroredSlot()
			}
		}()
	}
	wg.Wait()

	// The slot is still held, so a leaked waiter would still be parked on it.
	// Poll briefly: the callers' own goroutines are torn down asynchronously.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 0 {
		t.Fatalf("%d goroutine(s) still parked after %d cancelled acquisitions", leaked, callers)
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
