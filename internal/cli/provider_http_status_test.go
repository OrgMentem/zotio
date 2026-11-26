// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A provider's reply text is untrusted display data. A 5xx whose body mentions
// "HTTP 404" is an outage, not an absent record; only the typed status of a
// 404 or 410 answer means the registry has no such DOI.
func TestIsRegistryRecordAbsentReadsStatusNotBody(t *testing.T) {
	fetch := func(code int, body string) error {
		transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: code,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		})
		var out map[string]any
		return getCappedProviderJSON(context.Background(), &http.Client{Transport: transport}, providerCrossRef, "https://8.8.8.8/works/x", nil, &out)
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"404", fetch(404, "Resource not found"), true},
		{"410", fetch(410, ""), true},
		{"500 body mentions HTTP 404", fetch(500, "upstream HTTP 404 while loading"), false},
		{"503 body mentions HTTP 410", fetch(503, "HTTP 410"), false},
		{"untyped text only", fmt.Errorf("HTTP 404: nope"), false},
		{"nil", nil, false},
	} {
		if got := isRegistryRecordAbsent(tc.err); got != tc.want {
			t.Errorf("%s: isRegistryRecordAbsent(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
