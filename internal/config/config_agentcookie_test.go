// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The .agentcookie-managed marker gates a security-relevant Load divergence:
// with the marker present, credentials.toml must not be imported and the
// store must report the agentcookie source; without it, legacy behavior
// (credentials file wins) must hold. Nothing pinned this, so a marker
// regression would silently resurrect stale credentials.
func agentcookieTestIsolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{
		"ZOTERO_CONFIG",
		"ZOTERO_API_KEY",
		"ZOTERO_BASE_URL",
		"ZOTERO_USER_ID",
		"ZOTERO_HOME",
		"ZOTERO_DATA_DIR",
		"XDG_DATA_HOME",
		"ZOTIO_DEMO",
	} {
		t.Setenv(key, "")
	}
}

func TestLoadIgnoresCredentialsFileWhenAgentcookieManaged(t *testing.T) {
	agentcookieTestIsolate(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("api_key = \"config-key\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	credPath := filepath.Join(dir, "credentials.toml")
	if err := os.WriteFile(credPath, []byte("api_key = \"stale-credentials-key\"\n"), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	// Point the credentials resolver at the fixture dir: credentialsPath
	// derives from the data dir, so relocate it with the per-kind env var.
	t.Setenv("ZOTERO_DATA_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, ".agentcookie-managed"), []byte("managed\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.AuthHeader(); got != "config-key" {
		t.Fatalf("AuthHeader() = %q, want the config value (stale credentials file imported)", got)
	}
	if cfg.AuthSource != "agentcookie" {
		t.Errorf("AuthSource = %q, want agentcookie", cfg.AuthSource)
	}
	if cfg.CredentialSource != "agentcookie" {
		t.Errorf("CredentialSource = %q, want agentcookie", cfg.CredentialSource)
	}
	if !cfg.AgentcookieManagedByExternalStore() {
		t.Error("AgentcookieManagedByExternalStore = false, want true with the marker present")
	}

	// Writes must not import the stale file either: saving a new token keeps
	// serving it while the stale credentials file sits untouched on disk.
	if err := cfg.SaveCredential("rotated-key"); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	if data, err := os.ReadFile(credPath); err != nil {
		t.Fatalf("read credentials: %v", err)
	} else if !strings.Contains(string(data), "stale-credentials-key") {
		t.Fatalf("credentials file was rewritten under a managed store:\n%s", data)
	}
	reloaded, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.AuthHeader(); got != "rotated-key" {
		t.Fatalf("reloaded AuthHeader() = %q, want the rotated config value", got)
	}
}

func TestLoadPrefersCredentialsFileWithoutAgentcookieMarker(t *testing.T) {
	agentcookieTestIsolate(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("api_key = \"config-key\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.toml"), []byte("api_key = \"credentials-key\"\n"), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	t.Setenv("ZOTERO_DATA_DIR", dir)

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.AuthHeader(); got != "credentials-key" {
		t.Fatalf("AuthHeader() = %q, want the credentials file to win without a marker", got)
	}
	if cfg.AgentcookieManagedByExternalStore() {
		t.Error("AgentcookieManagedByExternalStore = true, want false without a marker")
	}
	if cfg.CredentialSource != "credentials file" {
		t.Errorf("CredentialSource = %q, want \"credentials file\"", cfg.CredentialSource)
	}
}
