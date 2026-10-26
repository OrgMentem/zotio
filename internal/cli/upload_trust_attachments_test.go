// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"zotio/internal/client"
)

func TestUploadURLTrustedAttachments(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"https://s3.amazonaws.com/x", true},
		{"http://localhost:1234/x", true},
		{"http://127.0.0.1/x", true},
		{"http://[::1]/x", true},
		{"http://storage.example/x", false},
		{"http://10.0.0.1/x", false},
		{"http://localhost.evil.example/x", false},
		{"ftp://storage.example/x", false},
		{"file:///etc/passwd", false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := uploadURLTrusted(u); got != tc.want {
				t.Fatalf("trusted=%v, want %v", got, tc.want)
			}
		})
	}
	if uploadURLTrusted(nil) {
		t.Fatal("nil URL trusted")
	}
}

func TestPostUploadPayloadRefusesRemoteHTTPAttachments(t *testing.T) {
	requests := 0
	c := &client.Client{HTTPClient: &http.Client{Transport: externalHTTPRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, fmt.Errorf("unexpected request")
	})}}
	err := postUploadPayload(context.Background(), c, "http://storage.example/private?signature=secret", "", "", "", uploadRequestFor(t, []byte("payload")))
	if err == nil || !strings.Contains(err.Error(), "http://storage.example") || !strings.Contains(err.Error(), "untrusted storage") {
		t.Fatalf("error=%v", err)
	}
	if requests != 0 {
		t.Fatalf("sent %d requests to untrusted storage", requests)
	}
}
