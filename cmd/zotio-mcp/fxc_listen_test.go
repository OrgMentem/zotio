package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// The generated token file is named by the bind address. A second server that
// cannot bind that address must exit without replacing the live server's token,
// or every client of the running server loses its credential.
func TestFxcBusyAddressKeepsLiveServerToken(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	t.Setenv("ZOTERO_STATE_DIR", stateDir)

	first, err := listenMCPHTTP(&bytes.Buffer{}, "127.0.0.1:0", "", "", false)
	if err != nil {
		t.Fatalf("bind first server: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	addr := first.Addr().String()
	const liveToken = "live-token-0123456789"
	if err := announceMCPAuth(&bytes.Buffer{}, addr, liveToken, "generated", true); err != nil {
		t.Fatalf("announce first server token: %v", err)
	}
	tokenPath := filepath.Join(stateDir, "mcp-http-token-"+strings.ReplaceAll(addr, ":", "_"))

	var log bytes.Buffer
	second, err := listenMCPHTTP(&log, addr, "second-token-0123456789", "generated", true)
	if err == nil {
		_ = second.Close()
		t.Fatalf("second server bound busy address %s", addr)
	}
	if got, _, _, rerr := resolveMCPAuthToken("", tokenPath, false); rerr != nil || got != liveToken {
		t.Fatalf("live token after failed second start = (%q, %v), want %q", got, rerr, liveToken)
	}
	if log.Len() != 0 {
		t.Fatalf("failed start announced a token: %q", log.String())
	}
}
