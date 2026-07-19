// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cliutil

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AdaptiveLimiter paces outbound requests with adaptive ceiling discovery.
// Starts at a floor rate, ramps up after consecutive successes, halves on 429
// and records a ceiling. Per-session only — not persisted. Methods are safe
// to call on a nil receiver.
type adaptiveLimiterWaiter struct {
	at   time.Time
	wake chan struct{}
}

type AdaptiveLimiter struct {
	mu          sync.Mutex
	rate        float64
	floor       float64
	ceiling     float64
	successes   int
	rampAfter   int
	lastRequest time.Time // zero-value: first Wait() returns immediately
	waiters     []*adaptiveLimiterWaiter
}

// NewAdaptiveLimiter returns a limiter starting at ratePerSec, or nil when
// rate-limiting should be disabled. Methods on the nil limiter no-op.
func NewAdaptiveLimiter(ratePerSec float64) *AdaptiveLimiter {
	if ratePerSec <= 0 {
		return nil
	}
	return &AdaptiveLimiter{
		rate:      ratePerSec,
		floor:     ratePerSec,
		rampAfter: 10,
	}
}

func (l *AdaptiveLimiter) Wait() {
	// preserve the existing no-arg API while routing
	// through the cancellable implementation used by HTTP requests.
	l.WaitContext(context.Background())
}

func (l *AdaptiveLimiter) WaitContext(ctx context.Context) {
	if l == nil {
		return
	}
	// callers may pass a request context so a
	// rate-limit sleep exits promptly on Ctrl-C/SIGTERM cancellation.
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	// Reserve the next send slot while holding the lock, then sleep outside the
	// lock. Each reservation remains cancellable: cancelling a waiter compacts
	// later reservations and wakes them to recalculate their deadline.
	l.mu.Lock()
	delay := time.Duration(float64(time.Second) / l.rate)
	last := l.lastRequest
	if n := len(l.waiters); n > 0 && l.waiters[n-1].at.After(last) {
		last = l.waiters[n-1].at
	}
	next := last.Add(delay)
	if now := time.Now(); next.Before(now) {
		next = now
	}
	waiter := &adaptiveLimiterWaiter{at: next, wake: make(chan struct{}, 1)}
	l.waiters = append(l.waiters, waiter)
	l.mu.Unlock()

	for {
		l.mu.Lock()
		deadline := waiter.at
		l.mu.Unlock()
		if d := time.Until(deadline); d > 0 {
			timer := time.NewTimer(d)
			select {
			case <-timer.C:
			case <-waiter.wake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				continue
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				l.mu.Lock()
				l.refundWaiter(waiter, delay)
				l.mu.Unlock()
				return
			}
		}

		l.mu.Lock()
		if ctx.Err() != nil {
			l.refundWaiter(waiter, delay)
			l.mu.Unlock()
			return
		}
		l.removeWaiter(waiter)
		if waiter.at.After(l.lastRequest) {
			l.lastRequest = waiter.at
		}
		l.mu.Unlock()
		return
	}
}

func (l *AdaptiveLimiter) removeWaiter(waiter *adaptiveLimiterWaiter) int {
	for i, queued := range l.waiters {
		if queued == waiter {
			l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
			return i
		}
	}
	return -1
}

func (l *AdaptiveLimiter) refundWaiter(waiter *adaptiveLimiterWaiter, delay time.Duration) {
	index := l.removeWaiter(waiter)
	if index < 0 {
		return
	}
	for _, queued := range l.waiters[index:] {
		queued.at = queued.at.Add(-delay)
		select {
		case queued.wake <- struct{}{}:
		default:
		}
	}
}

func (l *AdaptiveLimiter) OnSuccess() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.successes++
	if l.successes >= l.rampAfter {
		newRate := l.rate * 1.25
		if l.ceiling > 0 && newRate > l.ceiling*0.9 {
			newRate = l.ceiling * 0.9
		}
		l.rate = newRate
		l.successes = 0
	}
}

func (l *AdaptiveLimiter) OnRateLimit() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ceiling = l.rate
	l.rate = l.rate / 2
	if l.rate < 0.5 {
		l.rate = 0.5
	}
	l.successes = 0
}

func (l *AdaptiveLimiter) Rate() float64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rate
}

// rateLimitError signals an upstream returned 429 after retries were
// exhausted. Callers must surface this as a hard error rather than empty
// results — empty-on-throttle is indistinguishable from "no data exists"
// and silently corrupts downstream queries.
// unexported — only exercised by package tests today.
type rateLimitError struct {
	URL        string
	RetryAfter time.Duration
	Body       string
}

func (e *rateLimitError) Error() string {
	msg := fmt.Sprintf("rate limited: HTTP 429 for %s", e.URL)
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf("; retry after %s", e.RetryAfter)
	}
	if body := strings.TrimSpace(e.Body); body != "" {
		msg += ": " + body
	}
	return msg
}

// MaxRetryWait caps the wait derived from a Retry-After header so a buggy
// or hostile upstream cannot pin a CLI for hours.
const MaxRetryWait = 60 * time.Second

const (
	defaultRetryWait               = 5 * time.Second
	unixEpochSecondsThreshold      = 1_000_000_000
	unixEpochMillisecondsThreshold = 1_000_000_000_000
)

// retryAfterNow is the clock RetryAfter reads. It defaults to time.Now (so
// production behavior is unchanged) and is overridable in tests for exact,
// non-flaky assertions on HTTP-date / epoch Retry-After parsing.
// test seam for deterministic time-dependent tests.
var retryAfterNow = time.Now

// RetryAfter parses an HTTP Retry-After header (RFC 7231: delta-seconds or
// HTTP-date), plus common Unix epoch seconds/milliseconds variants emitted by
// some APIs. Waits are capped at MaxRetryWait. Returns 5s when missing or
// unparseable.
func RetryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return defaultRetryWait
	}
	header := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if header == "" {
		return defaultRetryWait
	}
	if value, err := strconv.ParseInt(header, 10, 64); err == nil {
		return retryAfterFromNumber(value)
	}
	if t, err := http.ParseTime(header); err == nil {
		wait := t.Sub(retryAfterNow())
		if wait > MaxRetryWait {
			return MaxRetryWait
		}
		if wait > 0 {
			return wait
		}
	}
	return defaultRetryWait
}

func retryAfterFromNumber(value int64) time.Duration {
	if value <= 0 {
		return defaultRetryWait
	}
	if value > int64(MaxRetryWait/time.Second) {
		if wait := retryAfterEpochWait(value); wait > 0 {
			if wait > MaxRetryWait {
				return MaxRetryWait
			}
			return wait
		}
		return MaxRetryWait
	}
	return time.Duration(value) * time.Second
}

func retryAfterEpochWait(value int64) time.Duration {
	switch {
	case value >= unixEpochMillisecondsThreshold:
		return time.UnixMilli(value).Sub(retryAfterNow())
	case value >= unixEpochSecondsThreshold:
		return time.Unix(value, 0).Sub(retryAfterNow())
	default:
		return 0
	}
}

// MaxBackoff caps Backoff so tests stay bounded. Callers needing jitter
// add their own; the bare exponential keeps the contract deterministic.
const MaxBackoff = 30 * time.Second

// Backoff returns 2^attempt seconds capped at MaxBackoff.
func Backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	wait := time.Duration(math.Pow(2, float64(attempt))) * time.Second
	if wait > MaxBackoff {
		return MaxBackoff
	}
	return wait
}
