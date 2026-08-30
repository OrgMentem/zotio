// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package client

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zotio/internal/config"
)

// This file pins the meaning of the int in Client's (body, int, error) returns.
//
// The type reuses that shape for two unrelated numbers: an HTTP status code on
// the mutating verbs, and a Zotero object version on GetWithVersion and
// GetFromWriteBaseWithVersion. Nothing in the signatures distinguishes them, so
// a caller that confuses the two compiles cleanly and then misreads the value.
// Doc comments now say which is which; these tests keep the prose honest by
// making the two meanings observable, with values chosen so a swap cannot pass:
// the status is never a plausible version and the version is never a plausible
// status.

func contractClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c := New(&config.Config{BaseURL: baseURL}, 5*time.Second, 0)
	// The version-bearing reads bypass the cache by construction; disabling it
	// here keeps the status assertions independent of any on-disk state.
	c.NoCache = true
	return c
}

// TestMutatingVerbsReturnHTTPStatus asserts the int from the write family is
// the server's status code. The server answers 409, which is not a version any
// of these tests could produce, so a version leaking into this position fails.
func TestMutatingVerbsReturnHTTPStatus(t *testing.T) {
	const wantStatus = http.StatusConflict
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A version header is present and deliberately different from the
		// status, so a method reading the wrong source is caught.
		w.Header().Set("Last-Modified-Version", "9182")
		w.WriteHeader(wantStatus)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c := contractClient(t, srv.URL)

	for _, tc := range []struct {
		name string
		call func() (int, error)
	}{
		{"Post", func() (int, error) { _, s, err := c.Post("/items", map[string]string{}); return s, err }},
		{"PostWithHeaders", func() (int, error) {
			_, s, err := c.PostWithHeaders("/items", map[string]string{}, nil)
			return s, err
		}},
		{"Put", func() (int, error) { _, s, err := c.Put("/items/AAAAAAAA", map[string]string{}); return s, err }},
		{"PutWithHeaders", func() (int, error) {
			_, s, err := c.PutWithHeaders("/items/AAAAAAAA", map[string]string{}, nil)
			return s, err
		}},
		{"Patch", func() (int, error) { _, s, err := c.Patch("/items/AAAAAAAA", map[string]string{}); return s, err }},
		{"PatchWithHeaders", func() (int, error) {
			_, s, err := c.PatchWithHeaders("/items/AAAAAAAA", map[string]string{}, nil)
			return s, err
		}},
		{"Delete", func() (int, error) { _, s, err := c.Delete("/items/AAAAAAAA"); return s, err }},
		{"DeleteWithHeaders", func() (int, error) {
			_, s, err := c.DeleteWithHeaders("/items/AAAAAAAA", nil)
			return s, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := tc.call()
			if got != wantStatus {
				t.Errorf("%s int = %d, want the HTTP status %d (a 9182 here means the version header leaked into the status position)",
					tc.name, got, wantStatus)
			}
		})
	}
}

// TestVersionReadsReturnObjectVersion asserts the int from the version-bearing
// reads is the Last-Modified-Version header, not the status. The server answers
// 200 with version 9182: were the status returned instead, the value would be
// 200, which is also a legal version, so the test uses a version far outside
// the status range to keep the two distinguishable.
func TestVersionReadsReturnObjectVersion(t *testing.T) {
	const wantVersion = 9182
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "9182")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"key":"AAAAAAAA"}`))
	}))
	t.Cleanup(srv.Close)
	c := contractClient(t, srv.URL)

	if _, got, err := c.GetWithVersion("/items/AAAAAAAA", nil); err != nil || got != wantVersion {
		t.Errorf("GetWithVersion int = %d (err %v), want the object version %d", got, err, wantVersion)
	}

	c.WriteBaseURL = srv.URL
	if _, got, err := c.GetFromWriteBaseWithVersion("/items/AAAAAAAA", nil); err != nil || got != wantVersion {
		t.Errorf("GetFromWriteBaseWithVersion int = %d (err %v), want the object version %d", got, err, wantVersion)
	}
}

// TestVersionReadsReportZeroForAMissingHeader pins the documented coercion: an
// absent or unparseable version becomes 0, which callers guarding a write must
// treat as "no precondition available" rather than as a valid version. Zotero
// rejects a guarded write with no precondition, so a caller that passed 0
// through would turn a missing header into a confusing server-side refusal.
func TestVersionReadsReportZeroForAMissingHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "not-a-number")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c := contractClient(t, srv.URL)

	if _, got, err := c.GetWithVersion("/items/AAAAAAAA", nil); err != nil || got != 0 {
		t.Errorf("GetWithVersion int = %d (err %v), want 0 for an unparseable version header", got, err)
	}
}

// TestProbeGetReturnsStatus pins that ProbeGet's single int is the status. It
// is the one method whose shape gives no hint: (int, error) with no body.
func TestProbeGetReturnsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "9182")
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := contractClient(t, srv.URL)

	got, _ := c.ProbeGet("/nonexistent")
	if got != http.StatusNotFound {
		t.Errorf("ProbeGet = %d, want the HTTP status 404", got)
	}
}
