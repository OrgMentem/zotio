package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func authConfigHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"ZOTIO_DEMO", "ZOTERO_CONFIG", "ZOTERO_API_KEY", "ZOTERO_USER_ID", "ZOTERO_BASE_URL", "ZOTERO_HOME", "ZOTERO_STATE_DIR", "ZOTERO_CACHE_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, "")
	}
	t.Setenv("ZOTERO_CONFIG_DIR", filepath.Join(home, "primary"))
	t.Setenv("ZOTERO_DATA_DIR", filepath.Join(home, "data"))
}

func TestLoadDefaultPathParseErrorsAuthConfig(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "primary", true: "legacy"}[legacy], func(t *testing.T) {
			authConfigHome(t)
			path, _, err := resolveConfigPath("")
			if err != nil {
				t.Fatal(err)
			}
			owner := "config-kind path"
			if legacy {
				path, err = LegacyConfigPath()
				owner = "legacy config path"
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("base_url = "), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load("")
			if err == nil || cfg != nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), owner) {
				t.Fatalf("Load = %v, %v; want parse error naming %s", cfg, err, path)
			}
		})
	}
}

func TestCredentialChangesClearCachedAccountAuthConfig(t *testing.T) {
	for _, logout := range []bool{false, true} {
		t.Run(map[bool]string{false: "rotate", true: "logout"}[logout], func(t *testing.T) {
			authConfigHome(t)
			cfg, err := Load("")
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.SaveCredential("old-key"); err != nil {
				t.Fatal(err)
			}
			if err := cfg.SaveUserID("111"); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ZOTERO_USER_ID", "333")
			cfg, err = Load("")
			if err != nil {
				t.Fatal(err)
			}
			if logout {
				err = cfg.ClearTokens()
			} else {
				err = cfg.SaveCredential("new-key")
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.UserID != "" || cfg.configForSave().UserID != "" {
				t.Fatal("cached account survived credential change")
			}
			t.Setenv("ZOTERO_USER_ID", "")
			cfg, err = Load("")
			if err != nil {
				t.Fatal(err)
			}
			if cfg.UserID != "" {
				t.Fatalf("persisted user_id = %q", cfg.UserID)
			}
		})
	}
}
