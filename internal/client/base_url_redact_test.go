// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package client

import (
	"io"
	"os"
	"strings"
	"testing"
)

// A rejected base URL is echoed so the user can see which setting was ignored,
// but the userinfo and credential query values in it are secrets that must not
// reach stderr, CI logs, or MCP error captures.
func TestUntrustedBaseURLWarningRedactsCredentials(t *testing.T) {
	const raw = "https://alice:pa55word@evil.example/api?token=t0ken-secret&x=1"
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	stderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = stderr })

	got := sanitizeClientBaseURL(raw)

	os.Stderr = stderr
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	if got != defaultZoteroBaseURL {
		t.Fatalf("sanitizeClientBaseURL(%q) = %q, want the default base %q", raw, got, defaultZoteroBaseURL)
	}
	warning := string(out)
	if !strings.Contains(warning, "evil.example") {
		t.Fatalf("warning %q does not name the rejected host", warning)
	}
	for _, secret := range []string{"alice", "pa55word", "t0ken-secret"} {
		if strings.Contains(warning, secret) {
			t.Fatalf("warning leaked credential %q: %q", secret, warning)
		}
	}
}
