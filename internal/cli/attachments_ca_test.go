// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"zotio/internal/client"
)

// The storage URL from upload authorization is bearer-signed: anyone holding it
// can upload into the library's file store until it expires. net/http embeds
// the request URL in every transport error, so a failed upload must not carry
// the path or query into the returned error, where it reaches stderr, MCP
// output, and the mutation journal.
func TestPostUploadPayloadTransportErrorRedactsSignedURL(t *testing.T) {
	const secret = "SIGNED-SENTINEL-7f3a9c"
	cause := errors.New("connection reset by peer")
	httpClient := &http.Client{Transport: externalHTTPRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, cause
	})}
	uploadURL := "https://storage.example.test/bucket/" + secret + "?X-Amz-Signature=" + secret + "&policy=" + secret

	err := postUploadPayload(context.Background(), &client.Client{HTTPClient: httpClient}, uploadURL, "", "", "", uploadRequestFor(t, []byte("payload")))
	if err == nil {
		t.Fatal("postUploadPayload succeeded, want the transport failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q, leaks the signed upload URL", err)
	}
	if !strings.Contains(err.Error(), "https://storage.example.test") {
		t.Fatalf("error = %q, want the storage origin kept for diagnosis", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want the transport cause kept in the chain", err)
	}
}
