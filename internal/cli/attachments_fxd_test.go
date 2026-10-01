// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"zotio/internal/client"
)

// net/http quotes a redirect Location it cannot parse inside the *url.Error
// cause, not only in its URL field. Rewriting the URL field alone left the
// signed query of that Location in the error text, which reaches stderr, MCP
// output, and the mutation journal.
func TestFxdPostUploadPayloadRedirectCauseRedactsSignedURL(t *testing.T) {
	const secret = "SIGNED-SENTINEL-c41d"
	httpClient := &http.Client{Transport: externalHTTPRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		h := http.Header{}
		h.Set("Location", "https://storage.example/bucket/%zz?X-Amz-Signature="+secret)
		return &http.Response{
			StatusCode: http.StatusFound,
			Status:     "302 Found",
			Header:     h,
			Body:       http.NoBody,
			Request:    r,
		}, nil
	})}
	uploadURL := "https://storage.example/bucket/key?X-Amz-Signature=" + secret

	err := postUploadPayload(context.Background(), &client.Client{HTTPClient: httpClient}, uploadURL, "", "", "", uploadRequestFor(t, []byte("payload")))
	if err == nil {
		t.Fatal("postUploadPayload succeeded, want the unparseable redirect reported")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q, leaks the signed redirect URL", err)
	}
	if !strings.Contains(err.Error(), "https://storage.example") {
		t.Fatalf("error = %q, want the storage origin kept for diagnosis", err)
	}
}
