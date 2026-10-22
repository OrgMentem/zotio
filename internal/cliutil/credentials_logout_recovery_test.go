// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cliutil_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"zotio/internal/cliutil"
	"zotio/internal/config"
)

// A logout that was killed after publishing the scrubbed primary config but
// before scrubbing the legacy file leaves exactly this state: a clean primary
// at the relocated path plus a legacy config that still holds the key. Load
// reads only the primary while the override is set, so the retry used to find
// nothing to clean, report success, and leave a key that authenticates again as
// soon as the override goes away.
func TestClearTokensRetryScrubsLegacyAfterPrimaryWasAlreadyPublished(t *testing.T) {
	home, legacyPath := resetCredentialEnv(t)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatalf("mkdir legacy config dir: %v", err)
	}
	if err := os.WriteFile(legacyPath, []byte("base_url = \"https://settings.example\"\n"+legacyCredentialTOML("legacy-secret")), 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}
	relocated := filepath.Join(home, "relocated")
	primaryPath := filepath.Join(relocated, "zotio", "config.toml")
	if err := os.MkdirAll(filepath.Dir(primaryPath), 0o700); err != nil {
		t.Fatalf("mkdir primary config dir: %v", err)
	}
	if err := os.WriteFile(primaryPath, []byte("base_url = \"https://settings.example\"\n"), 0o600); err != nil {
		t.Fatalf("write scrubbed primary config: %v", err)
	}

	loadWithOverride := func() *config.Config {
		t.Helper()
		t.Setenv("XDG_CONFIG_HOME", relocated)
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load with override: %v", err)
		}
		return cfg
	}
	loadWithoutOverride := func() *config.Config {
		t.Helper()
		t.Setenv("XDG_CONFIG_HOME", "")
		cfg, err := config.Load("")
		if err != nil {
			t.Fatalf("Load without override: %v", err)
		}
		return cfg
	}

	// The seeded state must be the dangerous one, or the test passes vacuously.
	assertConfigCredential(t, loadWithOverride(), "")
	assertConfigCredential(t, loadWithoutOverride(), "legacy-secret")

	if err := loadWithOverride().ClearTokens(); err != nil {
		t.Fatalf("retried ClearTokens: %v", err)
	}

	assertConfigCredential(t, loadWithOverride(), "")
	assertConfigCredential(t, loadWithoutOverride(), "")
	legacyData, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("read legacy config: %v", err)
	}
	if strings.Contains(string(legacyData), "legacy-secret") {
		t.Fatalf("legacy config still holds the key after the retried logout:\n%s", legacyData)
	}
	if !strings.Contains(string(legacyData), "https://settings.example") {
		t.Fatalf("legacy scrub dropped non-credential settings:\n%s", legacyData)
	}
}

// A legacy file that cannot be rewritten leaves a live key behind, so logout
// must say so instead of reporting a completed logout, must still remove the
// credentials file that is actually in use, and must finish the cleanup when
// retried after access is restored.
func TestClearTokensReportsLegacyScrubFailureAndRetryCompletes(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permission bits do not block the writer here")
	}
	home, legacyPath := resetCredentialEnv(t)
	legacyDir := filepath.Dir(legacyPath)
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatalf("mkdir legacy config dir: %v", err)
	}
	if err := os.WriteFile(legacyPath, []byte("base_url = \"https://settings.example\"\n"+legacyCredentialTOML("legacy-secret")), 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}
	if err := cliutil.SaveCredentials(testCredentials("data-secret")); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	relocated := filepath.Join(home, "relocated")
	t.Setenv("XDG_CONFIG_HOME", relocated)

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := os.Chmod(legacyDir, 0o500); err != nil {
		t.Fatalf("make legacy dir read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(legacyDir, 0o700) })

	err = cfg.ClearTokens()
	if err == nil {
		t.Fatal("ClearTokens succeeded although the legacy config still holds a key")
	}
	if !strings.Contains(err.Error(), legacyPath) {
		t.Fatalf("error = %q, want it to name the legacy config %s", err, legacyPath)
	}
	if _, ok, loadErr := cliutil.LoadCredentials(); loadErr != nil || ok {
		t.Fatalf("credentials file after the failed logout = ok %v err %v, want it removed regardless", ok, loadErr)
	}

	if err := os.Chmod(legacyDir, 0o700); err != nil {
		t.Fatalf("restore legacy dir: %v", err)
	}
	retry, err := config.Load("")
	if err != nil {
		t.Fatalf("Load for retry: %v", err)
	}
	if err := retry.ClearTokens(); err != nil {
		t.Fatalf("retried ClearTokens: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	reloaded, err := config.Load("")
	if err != nil {
		t.Fatalf("Load without override: %v", err)
	}
	assertConfigCredential(t, reloaded, "")
}

// Logout must not report success while an interrupted save's dot-temp copy of
// the secret is still on disk next to the config or credentials file.
func TestClearTokensRemovesOrphanedAtomicTemps(t *testing.T) {
	_, configPath := resetCredentialEnv(t)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("base_url = \"https://settings.example\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := cliutil.SaveCredentials(testCredentials("data-secret")); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	credsPath, err := cliutil.CredentialsFilePath()
	if err != nil {
		t.Fatalf("CredentialsFilePath: %v", err)
	}
	orphans := []string{
		filepath.Join(filepath.Dir(credsPath), ".credentials.toml.987654321.tmp"),
		filepath.Join(filepath.Dir(configPath), ".config.toml.987654321.tmp"),
	}
	for _, orphan := range orphans {
		if err := os.WriteFile(orphan, []byte(legacyCredentialTOML("data-secret")), 0o600); err != nil {
			t.Fatalf("seed orphan %s: %v", orphan, err)
		}
	}

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.ClearTokens(); err != nil {
		t.Fatalf("ClearTokens: %v", err)
	}
	for _, orphan := range orphans {
		if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("orphaned temp %s survived logout (stat err = %v)", orphan, err)
		}
	}
}
