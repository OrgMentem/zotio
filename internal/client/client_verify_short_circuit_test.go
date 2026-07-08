// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"zotio/internal/cliutil"
	"zotio/internal/config"
)

// recordingRoundTripper counts how many times its RoundTrip method is
// invoked and returns an empty 200 response. Used by the verify-mode
// short-circuit tests to assert that the transport layer never dials
// when the gate fires.
type recordingRoundTripper struct {
	calls int
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls++
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewReader([]byte("{}"))),
		Header:     http.Header{},
	}, nil
}

// newClientWithRecorder builds a minimal *Client wired to a recording
// transport. The Client is constructed through New() so the unexported
// limiter and cacheDir fields are initialized, then HTTPClient is
// swapped out for one whose Transport records every call.
func newClientWithRecorder(t *testing.T) (*Client, *recordingRoundTripper) {
	t.Helper()
	rec := &recordingRoundTripper{}
	cfg := &config.Config{BaseURL: "http://example.test"}
	c := New(cfg, time.Second, 0)
	c.HTTPClient = &http.Client{Transport: rec}
	c.NoCache = true
	return c, rec
}

// TestClient_VerifyShortCircuit_MutatingVerbs pins the transport-layer
// short-circuit in client.go. Under ZOTIO_VERIFY=1 with no
// ZOTIO_VERIFY_LIVE_HTTP opt-in, every mutating verb (DELETE/POST/PUT/PATCH)
// must return a synthetic envelope without dialing.
//
// A future edit that drops the gate, narrows the verb list, or removes either
// env-var check would silently re-open the agent-readiness gap the gate was
// added to close. This test fails on any of those drifts.
func TestClient_VerifyShortCircuit_MutatingVerbs(t *testing.T) {
	for _, verb := range []string{"DELETE", "POST", "PUT", "PATCH"} {
		verb := verb
		t.Run(verb, func(t *testing.T) {
			t.Setenv(cliutil.VerifyEnvVar, "1")
			// ZOTIO_VERIFY_LIVE_HTTP must NOT be set for the short-circuit branch.
			// Explicit unset defends against a stale value in the parent env.
			t.Setenv(cliutil.VerifyLiveHTTPEnvVar, "")
			c, rec := newClientWithRecorder(t)

			body, status, err := c.do(context.Background(), verb, "/test", nil, nil, nil)
			if err != nil {
				t.Fatalf("do(%s) returned error: %v", verb, err)
			}
			if status != http.StatusOK {
				t.Fatalf("do(%s) status = %d, want %d", verb, status, http.StatusOK)
			}
			if rec.calls != 0 {
				t.Fatalf("do(%s) attempted %d HTTP calls; want 0 (short-circuit)", verb, rec.calls)
			}

			var env map[string]any
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("envelope is not valid JSON: %v", err)
			}
			if got, _ := env["__pp_verify_synthetic__"].(bool); !got {
				t.Fatalf("envelope must include __pp_verify_synthetic__: true; got %v", env)
			}
			if got, _ := env["reason"].(string); got != "verify_short_circuit" {
				t.Fatalf("envelope reason = %q, want %q", got, "verify_short_circuit")
			}
			if got, _ := env["method"].(string); got != verb {
				t.Fatalf("envelope method = %q, want %q", got, verb)
			}
			if got, _ := env["path"].(string); got != "/test" {
				t.Fatalf("envelope path = %q, want %q", got, "/test")
			}
		})
	}
}

// TestClient_VerifyShortCircuit_LiveHTTPOptIn pins the verify sandbox
// contract: when ZOTIO_VERIFY_LIVE_HTTP=1 is set alongside ZOTIO_VERIFY=1,
// the short-circuit does NOT fire and the transport dials. Mock-server tests
// depend on this opt-in path to receive mutating requests so their pass/fail
// assertions can run against real wire-format responses.
func TestClient_VerifyShortCircuit_LiveHTTPOptIn(t *testing.T) {
	t.Setenv(cliutil.VerifyEnvVar, "1")
	t.Setenv(cliutil.VerifyLiveHTTPEnvVar, "1")
	c, rec := newClientWithRecorder(t)

	_, _, _ = c.do(context.Background(), "DELETE", "/test", nil, nil, nil)

	if rec.calls < 1 {
		t.Fatalf("LIVE_HTTP=1 should opt back in to real dial; recorder saw %d calls", rec.calls)
	}
}

// TestClient_VerifyShortCircuit_NoEnv pins the operator path: with no
// verify env vars set, mutating verbs dial normally.
func TestClient_VerifyShortCircuit_NoEnv(t *testing.T) {
	// Explicitly unset to defend against test-runner inherited values.
	t.Setenv(cliutil.VerifyEnvVar, "")
	t.Setenv(cliutil.VerifyLiveHTTPEnvVar, "")
	c, rec := newClientWithRecorder(t)

	_, _, _ = c.do(context.Background(), "DELETE", "/test", nil, nil, nil)

	if rec.calls < 1 {
		t.Fatalf("no verify env should dial normally; recorder saw %d calls", rec.calls)
	}
}

// TestClient_VerifyShortCircuit_GETControl pins that the gate is
// verb-specific: GET requests are never short-circuited, even under
// ZOTIO_VERIFY=1, because they cannot mutate remote state. A regression that
// broadens isMutatingVerb to include GET would break zotio's cached-fallback
// and list/show paths under verify mode.
func TestClient_VerifyShortCircuit_GETControl(t *testing.T) {
	t.Setenv(cliutil.VerifyEnvVar, "1")
	t.Setenv(cliutil.VerifyLiveHTTPEnvVar, "")
	c, rec := newClientWithRecorder(t)

	_, _, _ = c.do(context.Background(), "GET", "/test", nil, nil, nil)

	if rec.calls < 1 {
		t.Fatalf("GET must never short-circuit; recorder saw %d calls", rec.calls)
	}
}

// TestClient_VerifyShortCircuit_ReadOnlyPOST pins the doRead bypass: a POST
// routed through doRead (the path PostQuery* takes for GraphQL queries,
// JSON-RPC reads, and POST-based search) must NOT short-circuit under
// ZOTIO_VERIFY=1, because the operation does not mutate remote state. Without
// this, an inherited verify env silently breaks every read on a shared-endpoint
// API.
func TestClient_VerifyShortCircuit_ReadOnlyPOST(t *testing.T) {
	for _, verb := range []string{"POST", "PUT", "PATCH"} {
		verb := verb
		t.Run(verb, func(t *testing.T) {
			t.Setenv(cliutil.VerifyEnvVar, "1")
			t.Setenv(cliutil.VerifyLiveHTTPEnvVar, "")
			c, rec := newClientWithRecorder(t)

			_, _, _ = c.doRead(context.Background(), verb, "/test", nil, nil, nil)

			if rec.calls < 1 {
				t.Fatalf("doRead(%s) must dial through; recorder saw %d calls", verb, rec.calls)
			}
		})
	}
}
