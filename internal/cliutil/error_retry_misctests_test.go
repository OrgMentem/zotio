// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cliutil

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRetryAfterOrFallbackDateCapsMiscTests(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := retryAfterNow
	retryAfterNow = func() time.Time { return base }
	t.Cleanup(func() { retryAfterNow = old })
	future := base.Add(time.Hour)
	for _, tt := range []struct {
		name, header string
		wait         time.Duration
		fallback     bool
	}{
		{"http_date", future.Format(http.TimeFormat), MaxRetryWait, false},
		{"epoch_seconds", fmt.Sprint(future.Unix()), MaxRetryWait, false},
		{"epoch_milliseconds", fmt.Sprint(future.UnixMilli()), MaxRetryWait, false},
		{"past_http_date", base.Add(-time.Hour).Format(http.TimeFormat), 5 * time.Second, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			resp.Header.Set("Retry-After", tt.header)
			wait, fallback := RetryAfterOrFallback(resp)
			if wait != tt.wait || fallback != tt.fallback {
				t.Fatalf("RetryAfterOrFallback = (%v, %v), want (%v, %v)", wait, fallback, tt.wait, tt.fallback)
			}
		})
	}
}

func TestSanitizeErrorPreservesObjectKeysMiscTests(t *testing.T) {
	for _, name := range []string{"itemKey", "collectionKey", "searchKey", "otherKey", "ITEMKEY", "item%4Bey"} {
		msg := "GET /items?" + name + "=ABCD2345,EFGH6789&limit=1"
		if got := SanitizeErrorBodyWithSecrets(msg); got != msg {
			t.Errorf("SanitizeErrorBodyWithSecrets(%q) = %q", msg, got)
		}
	}
}

func TestSanitizeErrorCredentialBoundariesMiscTests(t *testing.T) {
	for _, name := range []string{"key", "api_key", "Key", "API_KEY", "%6Bey", "%4B%45%59", "%61pi%5fkey", "%41%50%49%5F%4B%45%59"} {
		msg := "GET /items?itemKey=ABCD2345&" + name + "=SECRET123&limit=1"
		got := SanitizeErrorBodyWithSecrets(msg)
		if strings.Contains(got, "SECRET123") || !strings.Contains(got, "[REDACTED]") || !strings.Contains(got, "itemKey=ABCD2345") || !strings.Contains(got, "&limit=1") {
			t.Errorf("credential %q: sanitized = %q", name, got)
		}
	}
	const fake = "SECRET123"
	for _, msg := range []string{"key=" + fake, "api_key=" + fake, "Zotero-API-Key: " + fake, "zOtErO-aPi-kEy: " + fake} {
		if got := SanitizeErrorBodyWithSecrets(msg); strings.Contains(got, "SECRET123") || !strings.Contains(got, "[REDACTED]") {
			t.Errorf("SanitizeErrorBodyWithSecrets(%q) = %q", msg, got)
		}
	}
	if got := SanitizeErrorBodyWithSecrets("itemKey=SECRET123", "SECRET123"); strings.Contains(got, "SECRET123") {
		t.Fatalf("literal secret survived: %q", got)
	}
}
