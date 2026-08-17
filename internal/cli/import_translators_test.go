// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchTranslatorHTML_NormalSizeSucceeds(t *testing.T) {
	old := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = old })

	body := "<html><head><title>ok</title></head><body>hello</body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	flags := &rootFlags{timeout: 5 * time.Second}
	got, err := fetchTranslatorHTML(context.Background(), srv.URL+"/page", flags)
	if err != nil {
		t.Fatalf("fetchTranslatorHTML normal size: %v", err)
	}
	if got != body {
		t.Fatalf("html = %q, want %q", got, body)
	}
}

func TestFetchTranslatorHTML_ExceedsLimitErrors(t *testing.T) {
	old := allowPrivateOutboundForTests
	allowPrivateOutboundForTests = true
	t.Cleanup(func() { allowPrivateOutboundForTests = old })

	over := strings.Repeat("a", 4<<20+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(over))
	}))
	defer srv.Close()

	flags := &rootFlags{timeout: 5 * time.Second}
	_, err := fetchTranslatorHTML(context.Background(), srv.URL+"/page", flags)
	if err == nil {
		t.Fatalf("fetchTranslatorHTML over limit succeeded, want exceeded error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exceeded") {
		t.Fatalf("error = %q, want 'exceeded'", msg)
	}
	if !strings.Contains(msg, "4194304") {
		t.Fatalf("error = %q, want limit 4194304", msg)
	}
}
