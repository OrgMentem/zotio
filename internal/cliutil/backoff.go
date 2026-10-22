// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cliutil

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BackoffGate holds a "not before" deadline that every request through one
// client must respect. The Zotero Web API sends `Backoff: <seconds>` on any
// response, including a 2xx, when it is overloaded: the client should make no
// further requests for that long. Methods are safe to call on a nil receiver.
type BackoffGate struct {
	mu    sync.Mutex
	until time.Time
}

// NewBackoffGate returns an empty gate.
func NewBackoffGate() *BackoffGate {
	return &BackoffGate{}
}

// BackoffFromHeader parses the Zotero `Backoff` response header (whole
// seconds). A missing, unparseable, or non-positive value yields 0. The wait
// is capped at MaxRetryWait so a buggy or hostile upstream cannot pin a CLI.
func BackoffFromHeader(h http.Header) time.Duration {
	value := strings.TrimSpace(h.Get("Backoff"))
	if value == "" {
		return 0
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	if seconds > int64(MaxRetryWait/time.Second) {
		return MaxRetryWait
	}
	return time.Duration(seconds) * time.Second
}

// Extend raises the deadline to d from now when that is later than the current
// one, and reports whether it did. A shorter request never shortens a longer
// one already in force.
func (g *BackoffGate) Extend(d time.Duration) bool {
	if g == nil || d <= 0 {
		return false
	}
	until := time.Now().Add(d)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !until.After(g.until) {
		return false
	}
	g.until = until
	return true
}

// Active reports whether a backoff deadline is still in the future.
func (g *BackoffGate) Active() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}

// WaitContext blocks until the deadline passes or ctx ends. The deadline is
// re-read after each sleep because another response may extend it meanwhile.
func (g *BackoffGate) WaitContext(ctx context.Context) {
	if g == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		g.mu.Lock()
		wait := time.Until(g.until)
		g.mu.Unlock()
		if wait <= 0 {
			return
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}
