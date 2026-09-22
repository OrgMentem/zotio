// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// verifies mutating request failures preserve uncertain write outcomes.

package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
)

func TestMutatingRequestCanceledBeforeDispatchIsNotAmbiguous(t *testing.T) {
	var calls int32
	transport := clientRoundTripFunc(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("unexpected dispatch")
	})
	c := clientTestNewClient(t, "https://example.test")
	c.NoCache = true
	c.HTTPClient = &http.Client{Transport: transport}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := c.doRequest(ctx, http.MethodPost, "/items", nil, map[string]any{"title": "updated"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if IsAmbiguousWriteError(err) {
		t.Fatalf("error type = %T, want a non-ambiguous cancellation before dispatch", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("transport calls = %d, want 0", got)
	}
}

func TestMutatingRequestCanceledAfterDispatchIsAmbiguous(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int32
	transport := clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		cancel()
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	c := clientTestNewClient(t, "https://example.test")
	c.NoCache = true
	c.HTTPClient = &http.Client{Transport: transport}

	_, _, _, err := c.doRequest(ctx, http.MethodPost, "/items", nil, map[string]any{"title": "updated"}, nil)
	if !IsAmbiguousWriteError(err) {
		t.Fatalf("error type = %T, want *AmbiguousWriteError after dispatch", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want wrapped context cancellation", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("transport calls = %d, want 1", got)
	}
}

func TestMutatingTransportFailureIsAmbiguous(t *testing.T) {
	dropped := errors.New("connection dropped")
	var calls int32
	transport := clientRoundTripFunc(func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return nil, dropped
	})
	c := clientTestNewClient(t, "https://example.test")
	c.NoCache = true
	c.HTTPClient = &http.Client{Transport: transport}

	_, _, _, err := c.doRequest(context.Background(), http.MethodPost, "/items", nil, map[string]any{"title": "updated"}, nil)
	if !IsAmbiguousWriteError(err) {
		t.Fatalf("error type = %T, want *AmbiguousWriteError after transport failure", err)
	}
	if !errors.Is(err, dropped) {
		t.Fatalf("error = %v, want wrapped transport failure", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("transport calls = %d, want 1", got)
	}
}

func TestMutatingResponseReadFailureIsAmbiguous(t *testing.T) {
	readFailure := errors.New("response body dropped")
	var calls int32
	transport := clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(errorReader{err: readFailure}),
			Request:    req,
		}, nil
	})
	c := clientTestNewClient(t, "https://example.test")
	c.NoCache = true
	c.HTTPClient = &http.Client{Transport: transport}

	_, _, _, err := c.doRequest(context.Background(), http.MethodPost, "/items", nil, map[string]any{"title": "updated"}, nil)
	if !IsAmbiguousWriteError(err) {
		t.Fatalf("error type = %T, want *AmbiguousWriteError after response read failure", err)
	}
	if !errors.Is(err, readFailure) {
		t.Fatalf("error = %v, want wrapped response read failure", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("transport calls = %d, want 1", got)
	}
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}
