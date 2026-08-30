// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// verifies client wrapper contexts cancel in-flight HTTP work.

package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"zotio/internal/config"
)

func TestGetCancelsInFlightRequestContext(t *testing.T) {
	started := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(&config.Config{BaseURL: server.URL}, time.Second, 0)
	c.SetContext(ctx)
	c.NoCache = true

	errCh := make(chan error, 1)
	go func() {
		_, err := c.Get("/items", nil)
		errCh <- err
	}()

	select {
	case <-started:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("server did not receive request before cancellation")
	}

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Get error = %v, want context canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Get did not return promptly after context cancellation")
	}
}

// TestSetContextIsSafeWithConcurrentRequests pins the base context as shared
// mutable state. One Client is shared across sync workers and fanout goroutines
// (and CloneForRead hands the same value to further clients) while SetContext
// can rebind it, so the field must be atomic. Run under -race.
func TestSetContextIsSafeWithConcurrentRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"K1"}]`))
	}))
	defer server.Close()

	c := New(&config.Config{BaseURL: server.URL}, time.Second, 0)
	c.NoCache = true

	// Force the racing pair to overlap. Without a gate the rebinder can finish
	// all its stores before any request reaches baseCtx, and a non-atomic field
	// would pass even under -race.
	rebinding := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-rebinding
			for range 25 {
				_, _ = c.Get("/items", nil)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(rebinding)
		for range 25 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			c.SetContext(ctx)
			// Read it back the way a borrower does before restoring.
			if c.Context() == nil {
				t.Error("Context() returned nil; a borrower cannot restore it")
			}
			cancel()
			c.SetContext(context.Background())
		}
		close(release)
	}()
	// The readers keep going until the rebinder is done, so the two overlap for
	// the whole rebinding window rather than by luck.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-release:
				return
			default:
				_, _ = c.Get("/items", nil)
			}
		}
	}()
	wg.Wait()

	// A clone must not alias a torn value either.
	if clone := c.CloneForRead(server.URL); clone.Context() == nil {
		t.Error("CloneForRead produced a client with no base context")
	}
}

func TestGetWithHeadersContextCancelsInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(&config.Config{BaseURL: server.URL}, time.Second, 0)
	c.NoCache = true

	errCh := make(chan error, 1)
	go func() {
		_, err := c.GetWithHeadersContext(ctx, "/items", nil, map[string]string{"X-Request-Source": "fanout"})
		errCh <- err
	}()

	select {
	case <-started:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("server did not receive request before cancellation")
	}

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("GetWithHeadersContext error = %v, want context canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("GetWithHeadersContext did not return promptly after context cancellation")
	}
}

func TestGetPreCanceledContextReturnsPromptly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := New(&config.Config{BaseURL: server.URL}, 200*time.Millisecond, 0)
	c.SetContext(ctx)
	c.NoCache = true

	errCh := make(chan error, 1)
	go func() {
		_, err := c.Get("/items", nil)
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Get error = %v, want context canceled", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Get did not return promptly with a pre-canceled context")
	}
}

// SetContext is the path the CLI uses to seed the client base context from
// cmd.Context(); a wrapper call must honor cancellation of that context.
func TestSetContextCancelsWrapperRequest(t *testing.T) {
	started := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(&config.Config{BaseURL: server.URL}, time.Second, 0)
	c.NoCache = true
	c.SetContext(ctx)

	errCh := make(chan error, 1)
	go func() {
		_, err := c.Get("/items", nil)
		errCh <- err
	}()

	select {
	case <-started:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("server did not receive request before cancellation")
	}

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Get error = %v, want context canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Get did not return promptly after SetContext cancellation")
	}
}

// A nil ctx must be ignored so callers can pass cmd.Context() unconditionally
// without clobbering the interrupt-cancellable default base context.
func TestSetContextNilPreservesBase(t *testing.T) {
	c := New(&config.Config{BaseURL: "http://localhost:23119/api/users/0"}, time.Second, 0)
	base := c.baseCtx()
	c.SetContext(nil) //nolint:staticcheck // SA1012: intentionally passing nil to verify a nil ctx is ignored and the base context is preserved.
	if c.baseCtx() != base {
		t.Fatal("SetContext(nil) replaced the base context")
	}
}
