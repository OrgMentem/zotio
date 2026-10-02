// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package client

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"zotio/internal/config"
)

type rsecFailTransport struct {
	cause error
}

func (r rsecFailTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, r.cause
}

func TestRsecClientTransportErrorRedactsCredentialsAndPreservesChain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cause := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	c := New(&config.Config{BaseURL: "http://u:sekret@localhost:12345/api/users/0?token=abc"}, time.Second, 0)
	c.NoCache = true
	c.HTTPClient.Transport = rsecFailTransport{cause: cause}
	_, err := c.Get("/items", nil)
	if err == nil {
		t.Fatal("want transport failure")
	}
	for _, secret := range []string{"abc", "sekret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("client leaked %q: %v", secret, err)
		}
	}
	var urlErr *url.Error
	var netErr *net.OpError
	if !errors.As(err, &urlErr) || !errors.As(err, &netErr) || !errors.Is(err, cause) {
		t.Fatalf("transport chain lost: %v", err)
	}
	if netErr != cause {
		t.Fatalf("network error identity changed: %v", netErr)
	}
}

func TestRsecTransportRedactionPreservesCancellation(t *testing.T) {
	original := &url.Error{Op: "Get", URL: "http://u:sekret@localhost:12345/?token=abc", Err: context.Canceled}
	err := redactTransportError(original)
	var urlErr *url.Error
	if !errors.Is(err, context.Canceled) || !errors.As(err, &urlErr) || urlErr != original {
		t.Fatalf("cancellation chain lost: %v", err)
	}
	if strings.Contains(err.Error(), "abc") || strings.Contains(err.Error(), "sekret") {
		t.Fatalf("cancellation error leaked credentials: %v", err)
	}
}

func TestRsecDryRunRedactsURLCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := New(&config.Config{BaseURL: "http://u:sekret@localhost:12345/api/users/0?token=abc"}, time.Second, 0)
	c.DryRun = true
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() {
		os.Stderr = oldStderr
		_ = w.Close()
		_ = r.Close()
	})
	_, _, requestErr := c.Delete("/items/1")
	os.Stderr = oldStderr
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if requestErr != nil {
		t.Fatalf("dry-run: %v", requestErr)
	}
	stderr := string(out)
	for _, secret := range []string{"sekret", "token=abc"} {
		if strings.Contains(stderr, secret) {
			t.Fatalf("dry-run leaked %q: %s", secret, stderr)
		}
	}
	if !strings.Contains(stderr, "DELETE http://***@localhost:12345/api/users/0?token=***") {
		t.Fatalf("dry-run omitted redacted method and URL: %s", stderr)
	}
}
