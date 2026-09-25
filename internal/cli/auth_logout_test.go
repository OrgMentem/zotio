// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/cliutil"
	"zotio/internal/config"
)

// Distinct fixture secrets per slot so a failure says which source leaked.
const (
	logoutTestConfigKey  = "FIXTURE-logout-config-api-key"
	logoutTestCredsKey   = "FIXTURE-logout-credentials-api-key"
	logoutTestLegacyHead = "FIXTURE-logout-legacy-auth-header"
	logoutTestClientID   = "FIXTURE-logout-client-id"
)

func runAuthLogoutCmd(t *testing.T, flags *rootFlags) (string, error) {
	t.Helper()
	cmd := newAuthLogoutCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	return out.String(), err
}

// Logout is a security boundary: after it, a fresh config.Load must find no
// credential in any persisted source. This seeds every slot (config api_key,
// legacy client pair, credentials-file api_key plus a legacy header) and
// proves nothing resurrects on reload and no secret remains on disk.
func TestAuthLogoutClearsEveryCredentialSource(t *testing.T) {
	home := isolateAuthHome(t)
	configPath := writeAuthTestConfigFile(t,
		"base_url = \""+authTestBaseURL+"\"\napi_key = \""+logoutTestConfigKey+"\"\nclient_id = \""+logoutTestClientID+"\"\n")
	credentialsPath := writeAuthTestCredentialsFile(t,
		"api_key = \""+logoutTestCredsKey+"\"\nauth_header = \""+logoutTestLegacyHead+"\"\n")

	before, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load before logout: %v", err)
	}
	if before.AuthHeader() == "" {
		t.Fatal("seeded store is unauthenticated; the test would pass vacuously")
	}

	out, err := runAuthLogoutCmd(t, &rootFlags{})
	if err != nil {
		t.Fatalf("auth logout: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "Logged out") {
		t.Fatalf("output = %q, want the logout confirmation", out)
	}

	reloaded, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load after logout: %v", err)
	}
	if got := reloaded.AuthHeader(); got != "" {
		t.Fatalf("reloaded AuthHeader() = %q, want empty (a credential resurrected)", got)
	}
	if reloaded.AuthHeaderVal != "" || reloaded.AccessToken != "" || reloaded.ClientID != "" || reloaded.ClientSecret != "" || reloaded.ZoteroApiKey != "" {
		t.Fatalf("reloaded credential slots not fully cleared: %#v", reloaded)
	}
	for _, path := range []string{configPath, credentialsPath} {
		if data, err := os.ReadFile(path); err == nil {
			for _, needle := range []string{logoutTestConfigKey, logoutTestCredsKey, logoutTestLegacyHead, logoutTestClientID} {
				if strings.Contains(string(data), needle) {
					t.Errorf("%s still contains %q after logout", path, needle)
				}
			}
		}
	}
	assertAbsentUnderHome(t, home, logoutTestConfigKey, logoutTestCredsKey, logoutTestLegacyHead, logoutTestClientID)
}

// A logout that cannot clear must fail loudly, not claim success: here the
// credentials path holds a non-empty directory, so removal fails and the
// command must return an error instead of "Logged out".
func TestAuthLogoutFailsWhenCredentialsCannotBeRemoved(t *testing.T) {
	isolateAuthHome(t)
	writeAuthTestConfigFile(t, "base_url = \""+authTestBaseURL+"\"\napi_key = \""+logoutTestConfigKey+"\"\n")
	credPath, err := cliutil.CredentialsFilePath()
	if err != nil {
		t.Fatalf("CredentialsFilePath: %v", err)
	}
	if err := os.MkdirAll(credPath, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", credPath, err)
	}
	if err := os.WriteFile(filepath.Join(credPath, "anchor"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write anchor: %v", err)
	}

	out, err := runAuthLogoutCmd(t, &rootFlags{})
	if err == nil {
		t.Fatalf("auth logout succeeded with an unremovable credentials dir (output %q)", out)
	}
	if strings.Contains(out, "Logged out") {
		t.Fatalf("output claims success despite the clear failure: %q", out)
	}
}

// Under an agentcookie-managed store, logout must still zero the config-side
// secrets and drop the stale credentials file while leaving the marker alone,
// so a later Load cannot resurrect either source.
func TestAuthLogoutUnderAgentcookieMarker(t *testing.T) {
	home := isolateAuthHome(t)
	configPath := writeAuthTestConfigFile(t, "base_url = \""+authTestBaseURL+"\"\napi_key = \""+logoutTestConfigKey+"\"\n")
	writeAuthTestCredentialsFile(t, "api_key = \""+logoutTestCredsKey+"\"\n")
	marker := filepath.Join(filepath.Dir(configPath), ".agentcookie-managed")
	if err := os.WriteFile(marker, []byte("managed\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	out, err := runAuthLogoutCmd(t, &rootFlags{})
	if err != nil {
		t.Fatalf("auth logout under marker: %v (output %q)", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker removed by logout: %v", err)
	}

	reloaded, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load after logout: %v", err)
	}
	if got := reloaded.AuthHeader(); got != "" {
		t.Fatalf("reloaded AuthHeader() = %q, want empty under a managed store", got)
	}
	assertAbsentUnderHome(t, home, logoutTestConfigKey, logoutTestCredsKey)
}
