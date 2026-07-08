// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// verifies adaptive limiter sleeps are context-cancellable.

package cliutil

import (
	"context"
	"testing"
	"time"
)

func TestAdaptiveLimiterWaitContextCancelsSleep(t *testing.T) {
	limiter := NewAdaptiveLimiter(1)
	limiter.Wait()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		limiter.WaitContext(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("WaitContext did not return promptly after context cancellation")
	}
}
