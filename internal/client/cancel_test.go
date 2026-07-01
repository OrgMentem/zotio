// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean write-safety): verifies client wrapper contexts cancel in-flight HTTP work.

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
	c.ctx = ctx
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

func TestGetPreCanceledContextReturnsPromptly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := New(&config.Config{BaseURL: server.URL}, 200*time.Millisecond, 0)
	c.ctx = ctx
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
