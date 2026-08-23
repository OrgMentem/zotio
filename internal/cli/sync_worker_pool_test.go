// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"sync"
	"testing"
	"time"
)

// runSyncPool drives the same worker-pool shape the sync command builds: N
// workers over a buffered work channel, an enqueue loop that stops on
// cancellation, and reportUnrunSyncResources completing the set afterwards. It
// returns one result per resource, keyed by resource name.
func runSyncPool(t *testing.T, ctx context.Context, resources []string, concurrency int, syncOne func(string) syncResult) map[string][]syncResult {
	t.Helper()

	work := make(chan string, len(resources))
	results := make(chan syncResult, len(resources))

	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runSyncWorker(ctx, work, results, syncOne)
		}()
	}

	enqueued := 0
enqueue:
	for _, resource := range resources {
		select {
		case <-ctx.Done():
			break enqueue
		case work <- resource:
			enqueued++
		}
	}
	close(work)

	go func() {
		wg.Wait()
		reportUnrunSyncResources(ctx, work, resources[enqueued:], results)
		close(results)
	}()

	got := map[string][]syncResult{}
	for res := range results {
		got[res.Resource] = append(got[res.Resource], res)
	}
	return got
}

// TestSyncPoolReportsEveryResourceWhenCancelled pins the invariant a caller
// reads the result set against: every requested resource yields exactly one
// result. Before the fix, cancellation dropped the not-yet-enqueued resources
// and the ones a worker dequeued after ctx.Done, so a cancelled sync reported
// fewer resources than it was given and the missing ones were
// indistinguishable from resources nobody asked for.
func TestSyncPoolReportsEveryResourceWhenCancelled(t *testing.T) {
	resources := []string{"items", "collections", "tags", "searches", "items-trash", "groups"}
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel while the first resource is in flight, so the run stops with work
	// both queued and unsent.
	var once sync.Once
	got := runSyncPool(t, ctx, resources, 1, func(resource string) syncResult {
		once.Do(func() {
			cancel()
			// Let the enqueue loop observe the cancellation.
			time.Sleep(20 * time.Millisecond)
		})
		return syncResult{Resource: resource, Count: 1}
	})
	cancel()

	if len(got) != len(resources) {
		t.Fatalf("got results for %d resources, want %d: %v", len(got), len(resources), got)
	}
	for _, resource := range resources {
		switch n := len(got[resource]); n {
		case 1:
		case 0:
			t.Errorf("%s produced no result", resource)
		default:
			t.Errorf("%s produced %d results, want exactly 1", resource, n)
		}
	}
}

// TestSyncPoolReportsEveryResourceWhenComplete guards the other direction: an
// uncancelled run must not gain phantom cancellation results from the drain.
func TestSyncPoolReportsEveryResourceWhenComplete(t *testing.T) {
	resources := []string{"items", "collections", "tags"}
	got := runSyncPool(t, context.Background(), resources, 2, func(resource string) syncResult {
		return syncResult{Resource: resource, Count: 7}
	})

	if len(got) != len(resources) {
		t.Fatalf("got results for %d resources, want %d: %v", len(got), len(resources), got)
	}
	for _, resource := range resources {
		if len(got[resource]) != 1 {
			t.Fatalf("%s produced %d results, want exactly 1", resource, len(got[resource]))
		}
		res := got[resource][0]
		if res.Err != nil {
			t.Errorf("%s: unexpected error %v", resource, res.Err)
		}
		if res.Count != 7 {
			t.Errorf("%s: count = %d, want 7", resource, res.Count)
		}
	}
}
