// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.
// cover the config seam that isolates the ZOTIO_DEMO sandbox.
// The safety contract is that with ZOTIO_DEMO active, Load returns a pristine,
// key-less config BEFORE it ever reads a config file or a ZOTERO_* override —
// and that with demo mode off, those same overrides are honored exactly.

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Poisoned values threaded through every credential channel Load consults. A
// correctly isolated demo Load must surface none of them; a normal Load must
// surface all of them.
const (
	poisonKey  = "POISON-API-KEY-must-never-surface"
	poisonBase = "https://evil.example.test/api/users/13"
	poisonUser = "66666"
)

// poisonEnvironment plants a config file carrying a real api_key (pointed at by
// ZOTERO_CONFIG) plus ZOTERO_API_KEY / ZOTERO_BASE_URL / ZOTERO_USER_ID
// overrides, all under an isolated temp HOME. t.Setenv restores every var (and
// the pre-test ZOTIO_DEMO) at cleanup, so these tests are full-suite safe.
func poisonEnvironment(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(home, "poison.toml")
	body := "base_url = \"" + poisonBase + "\"\napi_key = \"" + poisonKey + "\"\nuser_id = \"" + poisonUser + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write poison config: %v", err)
	}
	t.Setenv("ZOTERO_CONFIG", cfgPath)
	t.Setenv("ZOTERO_API_KEY", poisonKey)
	t.Setenv("ZOTERO_BASE_URL", poisonBase)
	t.Setenv("ZOTERO_USER_ID", poisonUser)
}

// TestLoadInDemoModeReturnsKeylessConfigDespitePoisonedEnvAndConfig defends the
// core safety invariant: with ZOTIO_DEMO=1, no api key, user id, or base URL
// from the user's real config file or ZOTERO_* env overrides may leak into the
// returned config. Load must short-circuit to the sandbox config.
func TestLoadInDemoModeReturnsKeylessConfigDespitePoisonedEnvAndConfig(t *testing.T) {
	poisonEnvironment(t)
	t.Setenv("ZOTIO_DEMO", "1")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load in demo mode: %v", err)
	}

	if cfg.ZoteroApiKey != "" {
		t.Errorf("demo config leaked an API key: ZoteroApiKey = %q, want empty", cfg.ZoteroApiKey)
	}
	if got := cfg.AuthHeader(); got != "" {
		t.Errorf("demo config leaked an auth header: AuthHeader() = %q, want empty", got)
	}
	if cfg.UserID != "" {
		t.Errorf("demo config leaked a user id: UserID = %q, want empty", cfg.UserID)
	}
	// The exact base URL literal is a default we do not pin; what matters is
	// that the poisoned override did NOT survive.
	if cfg.BaseURL == poisonBase {
		t.Errorf("demo config leaked poisoned base URL %q", cfg.BaseURL)
	}
	// AuthSource is the discriminator the rest of the tool branches on to know
	// it is running the sandbox rather than a real, credentialed library.
	if cfg.AuthSource != "demo" {
		t.Errorf("demo config AuthSource = %q, want %q", cfg.AuthSource, "demo")
	}
}

// TestLoadWithoutDemoModeHonorsPoisonedEnvAndConfig is the mirror: with demo
// mode off ("0" and empty both mean off — the parsing boundary of
// demoModeFromEnv), the very same overrides Load ignored above must now be
// applied. Together the two tests prove the demo guard is the *only* thing
// suppressing real credentials, in both directions.
func TestLoadWithoutDemoModeHonorsPoisonedEnvAndConfig(t *testing.T) {
	for _, demoVal := range []string{"0", ""} {
		name := "explicit-zero"
		if demoVal == "" {
			name = "empty-unset"
		}
		t.Run(name, func(t *testing.T) {
			poisonEnvironment(t)
			t.Setenv("ZOTIO_DEMO", demoVal)

			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load with demo off: %v", err)
			}

			if cfg.ZoteroApiKey != poisonKey {
				t.Errorf("ZoteroApiKey = %q, want the ZOTERO_API_KEY override %q", cfg.ZoteroApiKey, poisonKey)
			}
			if cfg.BaseURL != poisonBase {
				t.Errorf("BaseURL = %q, want the ZOTERO_BASE_URL override %q", cfg.BaseURL, poisonBase)
			}
			if cfg.UserID != poisonUser {
				t.Errorf("UserID = %q, want the ZOTERO_USER_ID override %q", cfg.UserID, poisonUser)
			}
			if cfg.AuthSource != "env:ZOTERO_API_KEY" {
				t.Errorf("AuthSource = %q, want %q (env override, not the demo sentinel)", cfg.AuthSource, "env:ZOTERO_API_KEY")
			}
		})
	}
}
