// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cliutil

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// errBody is a ReadCloser that yields some bytes and then fails, simulating
// a transient network/proxy failure mid-body.
type errBody struct {
	remaining int
	err       error
}

func (b *errBody) Read(p []byte) (int, error) {
	if b.remaining > 0 {
		n := len(p)
		if n > b.remaining {
			n = b.remaining
		}
		b.remaining -= n
		return n, nil
	}
	return 0, b.err
}

func (b *errBody) Close() error { return nil }

// rtFunc adapts a function to http.RoundTripper.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProbeReachableBodyDrainError(t *testing.T) {
	drainErr := errors.New("connection reset by peer")
	client := &http.Client{
		Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       &errBody{remaining: 512, err: drainErr},
				Request:    r,
			}, nil
		}),
	}

	status, _, err := ProbeReachable(context.Background(), client, "http://example.test/")
	if status != ReachabilityUnreachable {
		t.Fatalf("status = %q, want %q", status, ReachabilityUnreachable)
	}
	if err == nil {
		t.Fatal("err = nil, want non-nil drain error")
	}
	if !errors.Is(err, drainErr) {
		t.Fatalf("err = %v, want wrapped %v", err, drainErr)
	}
}
