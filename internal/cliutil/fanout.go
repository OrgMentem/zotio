// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

// Package cliutil contains shared infrastructure helpers used across the
// CLI. Helpers live in their own package (not in package cli) to avoid
// symbol collisions with commands in package cli. Callers import as
// `cliutil` and invoke `cliutil.FanoutRun(...)`, `cliutil.CleanText(...)`,
// etc.
package cliutil

import (
	"context"
	"fmt"
	"sync"
)

// FanoutError represents one source's failure from a FanoutRun call.
// Source identifies which input produced the error; Err is the error returned
// by the caller's fn.
type FanoutError struct {
	Source string
	Err    error
}

// FanoutResult pairs a successful fn return value with its source name so
// callers can iterate results without a separate source lookup.
type FanoutResult[T any] struct {
	Source string
	Value  T
}

// fanoutConcurrency is the worker count. 4 matches the existing sync.go
// worker-pool idiom and is safe for scraping CLIs where per-host 429 pressure
// is real.
//
// Deliberately a constant, not an option. FanoutRun used to take
// `opts ...FanoutOption`, where FanoutOption wrapped an unexported struct and
// no With* constructor was ever written, so no caller inside or outside this
// package could build one: a functional-options signature that accepted
// nothing (zotio-caaace76). Give it back only when a caller needs per-host
// tuning, and export the constructor in the same change.
const fanoutConcurrency = 4

// FanoutRun invokes fn concurrently for each source and collects successful
// results plus per-source errors. It never returns a top-level error and
// recovers panics from fn as per-source FanoutErrors — partial failures
// surface via the returned errors slice, which should be piped to
// FanoutReportErrors so no source is silently dropped.
//
// Contract:
//   - Workers respect ctx: on ctx.Done() they stop pulling new jobs, and
//     in-flight fn calls receive the cancelled ctx.
//   - Unpulled sources produce a FanoutError{Err: ctx.Err()} so reporting
//     stays complete — cancel never silently drops a source.
//   - Errors are collected by source index and returned in source order,
//     not completion order, so FanoutReportErrors output is deterministic
//     across runs.
//   - The jobs channel is bounded at 2*concurrency so large source lists
//     don't buffer one goroutine per source.
//
// Per-source rate limiting is the caller's responsibility. Wrap fn with a
// limiter (e.g., golang.org/x/time/rate) if you're fanning out to sites
// that enforce per-host throttles; naïve scrape fan-out triggers 429s.
func FanoutRun[S, T any](
	ctx context.Context,
	sources []S,
	name func(S) string,
	fn func(context.Context, S) (T, error),
) ([]FanoutResult[T], []FanoutError) {
	// Parallel slices indexed by source position so output stays in source
	// order regardless of completion order. Using pointers lets us detect
	// "no result and no error" (shouldn't happen but is a defensive signal).
	type slot struct {
		result *FanoutResult[T]
		err    *FanoutError
	}
	slots := make([]slot, len(sources))

	type job struct{ idx int }
	jobs := make(chan job, fanoutConcurrency*2)

	var wg sync.WaitGroup
	for w := 0; w < fanoutConcurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				idx := j.idx
				func() {
					// Recover panics from fn so one bad source doesn't kill
					// the whole process. The panic becomes a per-source
					// FanoutError alongside regular errors.
					defer func() {
						if r := recover(); r != nil {
							slots[idx].err = &FanoutError{
								Source: name(sources[idx]),
								Err:    fmt.Errorf("panic in fanout fn: %v", r),
							}
						}
					}()
					// Respect cancellation: if ctx is already done, record
					// the cancel error rather than running fn with a
					// useless context.
					if err := ctx.Err(); err != nil {
						slots[idx].err = &FanoutError{Source: name(sources[idx]), Err: err}
						return
					}
					val, err := fn(ctx, sources[idx])
					if err != nil {
						slots[idx].err = &FanoutError{Source: name(sources[idx]), Err: err}
					} else {
						v := val
						slots[idx].result = &FanoutResult[T]{Source: name(sources[idx]), Value: v}
					}
				}()
			}
		}()
	}

	// Feed jobs, but stop feeding if ctx cancels so unpulled sources get a
	// ctx.Err() FanoutError rather than being silently dropped.
	func() {
		defer close(jobs)
		for i := range sources {
			select {
			case <-ctx.Done():
				// Mark this and all remaining sources as cancelled, then stop.
				for j := i; j < len(sources); j++ {
					slots[j].err = &FanoutError{Source: name(sources[j]), Err: ctx.Err()}
				}
				return
			case jobs <- job{idx: i}:
			}
		}
	}()

	wg.Wait()

	results := make([]FanoutResult[T], 0, len(sources))
	errs := make([]FanoutError, 0, len(slots))
	for _, s := range slots {
		if s.result != nil {
			results = append(results, *s.result)
		}
		if s.err != nil {
			errs = append(errs, *s.err)
		}
	}
	return results, errs
}
