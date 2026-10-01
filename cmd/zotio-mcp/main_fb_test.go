package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The generated bearer token must reach the operator without touching the
// server log, which supervisors and log collectors capture. It goes to a
// private file whose path is logged, and that file works as
// --mcp-auth-token-file.
func TestFbGeneratedMCPTokenStaysOutOfLogs(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("ZOTERO_STATE_DIR", stateDir)
	t.Setenv("ZOTIO_MCP_TOKEN", "")

	token, source, generated, err := resolveMCPAuthToken("", "", false)
	if err != nil || !generated {
		t.Fatalf("resolveMCPAuthToken = (%q, %q, %v, %v), want a generated token", token, source, generated, err)
	}
	var log bytes.Buffer
	if err := announceMCPAuth(&log, "127.0.0.1:7777", token, source, generated); err != nil {
		t.Fatalf("announceMCPAuth: %v", err)
	}
	if strings.Contains(log.String(), token) {
		t.Fatalf("server log contains the bearer token: %q", log.String())
	}

	tokenPath := filepath.Join(stateDir, "mcp-http-token-127.0.0.1_7777")
	if !strings.Contains(log.String(), tokenPath) {
		t.Fatalf("server log = %q, want it to name the token file %s", log.String(), tokenPath)
	}
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file mode = %#o, want 0600", perm)
	}
	reread, rereadSource, rereadGenerated, err := resolveMCPAuthToken("", tokenPath, false)
	if err != nil || reread != token || rereadGenerated {
		t.Fatalf("token file as --mcp-auth-token-file = (%q, %q, %v, %v), want the generated token", reread, rereadSource, rereadGenerated, err)
	}

	// A second server on another port keeps its own token file.
	if err := announceMCPAuth(&bytes.Buffer{}, "127.0.0.1:7778", "other-token-0123456789", "generated", true); err != nil {
		t.Fatalf("announceMCPAuth second server: %v", err)
	}
	if again, _, _, err := resolveMCPAuthToken("", tokenPath, false); err != nil || again != token {
		t.Fatalf("first server token after second start = (%q, %v), want it unchanged", again, err)
	}
}
