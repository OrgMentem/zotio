// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveCredentialPersistsAPIKeyWhereAuthHeaderReadsAndScrubsConfigFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(home, "config.toml")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.AuthHeaderVal = "legacy-header"
	cfg.AccessToken = "legacy-bearer"
	if err := cfg.SaveCredential("secret-token"); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	reloaded, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.AuthHeader(); got != "secret-token" {
		t.Fatalf("AuthHeader() after reload = %q, want saved API key", got)
	}
	if reloaded.AccessToken != "" || reloaded.AuthHeaderVal != "" {
		t.Fatalf("legacy credential slots survived reload: auth_header=%q access_token=%q", reloaded.AuthHeaderVal, reloaded.AccessToken)
	}
	hasCreds, err := FileHasCredentialFields(cfgPath)
	if err != nil {
		t.Fatalf("probe config file: %v", err)
	}
	if hasCreds {
		data, _ := os.ReadFile(cfgPath)
		t.Fatalf("config file still contains credential fields after SaveCredential:\n%s", data)
	}
}

func TestSaveScrubsLegacyCredentialsWhenConfigRelocates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacyPath := filepath.Join(home, "legacy.toml")
	newPath := filepath.Join(home, "new", "config.toml")
	legacy := "base_url = \"http://localhost:23119/api/users/0\"\napi_key = \"old-secret\"\naccess_token = \"old-bearer\"\nclient_secret = \"old-client-secret\"\n"
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	cfg := &Config{
		BaseURL:          "http://localhost:23119/api/users/0",
		Path:             newPath,
		legacySourcePath: legacyPath,
		envOverrides:     map[string]bool{},
	}
	if err := cfg.save(); err != nil {
		t.Fatalf("save relocated config: %v", err)
	}

	hasCreds, err := FileHasCredentialFields(legacyPath)
	if err != nil {
		t.Fatalf("probe legacy config: %v", err)
	}
	if hasCreds {
		data, _ := os.ReadFile(legacyPath)
		t.Fatalf("legacy config still contains credential fields after scrub:\n%s", data)
	}
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("read scrubbed legacy config: %v", err)
	}
	if !strings.Contains(string(data), "base_url") {
		t.Fatalf("legacy scrub removed non-credential config data:\n%s", data)
	}
}

func TestConfigJSONOmitsCredentialBearingFields(t *testing.T) {
	cfg := &Config{
		BaseURL:       "https://api.example.test/users/1",
		AuthHeaderVal: "legacy-auth-secret",
		Headers:       map[string]string{"Authorization": "header-secret"},
		AccessToken:   "access-secret",
		RefreshToken:  "refresh-secret",
		TokenExpiry:   time.Unix(1_700_000_000, 0).UTC(),
		ClientID:      "client-identifier",
		ClientSecret:  "client-secret",
		ZoteroApiKey:  "zotero-api-secret",
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	for _, field := range []string{
		"AuthHeaderVal",
		"Headers",
		"AccessToken",
		"RefreshToken",
		"TokenExpiry",
		"ClientID",
		"ClientSecret",
		"ZoteroApiKey",
	} {
		if _, ok := got[field]; ok {
			t.Errorf("json output contains credential-bearing field %q: %s", field, data)
		}
	}
	for _, secret := range []string{
		"legacy-auth-secret",
		"header-secret",
		"access-secret",
		"refresh-secret",
		"client-identifier",
		"client-secret",
		"zotero-api-secret",
	} {
		if strings.Contains(string(data), secret) {
			t.Errorf("json output contains credential-bearing value %q: %s", secret, data)
		}
	}
	if got["BaseURL"] != cfg.BaseURL {
		t.Errorf("json output BaseURL = %#v, want %q", got["BaseURL"], cfg.BaseURL)
	}
}
