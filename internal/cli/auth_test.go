// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/cliutil"
	"zotio/internal/config"

	"github.com/spf13/cobra"
)

// Obvious fixtures, never real-looking secrets. The two legacy values are
// distinct so a failure message says which slot leaked.
const (
	authTestLegacyConfigHeader = "FIXTURE-legacy-config-auth-header"
	authTestLegacyCredHeader   = "FIXTURE-legacy-credentials-auth-header"
	authTestNewToken           = "FIXTURE-new-api-token"
	authTestRejectedToken      = "FIXTURE-token-that-must-not-persist"
	authTestBaseURL            = "http://127.0.0.1:1/api/users/0"
)

// isolateAuthHome points config and credential resolution at a private HOME.
// ResolveKindDir consults per-kind and XDG env vars before the platform default
// under HOME, so a developer environment that sets any of them would drag the
// test onto the real store. Clearing them forces the platform-default rung.
func isolateAuthHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{
		"ZOTERO_CONFIG",
		"ZOTERO_API_KEY",
		"ZOTERO_BASE_URL",
		"ZOTERO_HOME",
		"ZOTERO_CONFIG_DIR",
		"ZOTERO_DATA_DIR",
		"ZOTERO_STATE_DIR",
		"ZOTERO_CACHE_DIR",
		"XDG_CONFIG_HOME",
		"XDG_DATA_HOME",
		"XDG_STATE_HOME",
		"XDG_CACHE_HOME",
		"ZOTIO_DEMO",
	} {
		t.Setenv(key, "")
	}
	return home
}

// writeAuthTestConfigFile seeds the real config.toml that config.Load reads.
func writeAuthTestConfigFile(t *testing.T, body string) string {
	t.Helper()
	dir, err := cliutil.ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// writeAuthTestCredentialsFile seeds the real credentials.toml that
// cliutil.LoadCredentials reads, which is the only way to hand config.Load a
// non-empty AuthHeaderVal: Load migrates a config-file auth_header into api_key
// and then zeroes the legacy field.
func writeAuthTestCredentialsFile(t *testing.T, body string) string {
	t.Helper()
	path, err := cliutil.CredentialsFilePath()
	if err != nil {
		t.Fatalf("CredentialsFilePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func runAuthSetTokenCmd(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := newAuthSetTokenCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetIn(strings.NewReader(stdin))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// assertAbsentUnderHome fails if any file under home still holds a value that
// the command was supposed to drop or refuse. It reads the whole isolated HOME
// rather than one file, so a secret written to an unexpected location (a
// forgotten legacy copy, the credentials file, a temp leftover) is still caught.
func assertAbsentUnderHome(t *testing.T, home string, needles ...string) {
	t.Helper()
	err := filepath.WalkDir(home, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, needle := range needles {
			if bytes.Contains(data, []byte(needle)) {
				t.Errorf("%s contains %q, want that value absent from disk", path, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", home, err)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestAuthSetTokenClearsLegacyHeaderAndActivatesSavedToken drives the real
// `auth set-token --stdin` command against an on-disk store that carries a
// legacy auth_header in both credential locations, then reloads from disk. The
// operator must end up authenticated as the new token, with no legacy value
// left anywhere that config.Load could resurrect it from.
func TestAuthSetTokenClearsLegacyHeaderAndActivatesSavedToken(t *testing.T) {
	home := isolateAuthHome(t)
	configPath := writeAuthTestConfigFile(t, "base_url = \""+authTestBaseURL+"\"\nauth_header = \""+authTestLegacyConfigHeader+"\"\n")
	credentialsPath := writeAuthTestCredentialsFile(t, "auth_header = \""+authTestLegacyCredHeader+"\"\n")

	// Precondition: the store authenticates through the legacy slot only, so the
	// new token has something real to displace.
	before, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load before set-token: %v", err)
	}
	if before.AuthHeaderVal != authTestLegacyCredHeader {
		t.Fatalf("seeded AuthHeaderVal = %q, want %q", before.AuthHeaderVal, authTestLegacyCredHeader)
	}

	out, err := runAuthSetTokenCmd(t, authTestNewToken+"\n", "--stdin")
	if err != nil {
		t.Fatalf("auth set-token --stdin: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "Token saved to") {
		t.Fatalf("output = %q, want a saved confirmation", out)
	}

	reloaded, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load after set-token: %v", err)
	}
	if got := reloaded.AuthHeader(); got != authTestNewToken {
		t.Fatalf("reloaded AuthHeader() = %q, want %q", got, authTestNewToken)
	}
	if reloaded.AuthHeaderVal != "" {
		t.Fatalf("reloaded AuthHeaderVal = %q, want empty legacy slot", reloaded.AuthHeaderVal)
	}
	if got := readFileString(t, configPath); strings.Contains(got, "auth_header") {
		t.Fatalf("config file still declares auth_header:\n%s", got)
	}
	if got := readFileString(t, credentialsPath); strings.Contains(got, "auth_header") {
		t.Fatalf("credentials file still declares auth_header:\n%s", got)
	}
	assertAbsentUnderHome(t, home, authTestLegacyConfigHeader, authTestLegacyCredHeader)
}

// TestAuthSetTokenRejectsUnsafeInput covers every refusal arm of the command.
// Each arm must fail with the auth exit class, persist no secret, and leave the
// pre-existing legacy credential exactly as it found it.
func TestAuthSetTokenRejectsUnsafeInput(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		stdin   string
		wantErr string
	}{
		{
			name:    "stdin flag omitted",
			args:    nil,
			stdin:   authTestRejectedToken + "\n",
			wantErr: "refusing token on command line",
		},
		{
			name:    "empty stdin",
			args:    []string{"--stdin"},
			stdin:   "",
			wantErr: "empty token on stdin",
		},
		{
			name:    "whitespace only stdin",
			args:    []string{"--stdin"},
			stdin:   "  \t\r\n  \n",
			wantErr: "empty token on stdin",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateAuthHome(t)
			configBody := "base_url = \"" + authTestBaseURL + "\"\nauth_header = \"" + authTestLegacyConfigHeader + "\"\n"
			configPath := writeAuthTestConfigFile(t, configBody)
			credentialsPath, err := cliutil.CredentialsFilePath()
			if err != nil {
				t.Fatalf("CredentialsFilePath: %v", err)
			}

			out, err := runAuthSetTokenCmd(t, tc.stdin, tc.args...)
			if err == nil {
				t.Fatalf("auth set-token succeeded for %s, want refusal (output %q)", tc.name, out)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err.Error(), tc.wantErr)
			}
			var cErr *cliError
			if !errors.As(err, &cErr) || cErr.code != 4 {
				t.Fatalf("error = %#v, want *cliError with code 4", err)
			}
			if strings.Contains(out, "Token saved to") {
				t.Fatalf("output = %q, want no saved confirmation on refusal", out)
			}

			// No credential may be written: the credentials file is the only place
			// SaveCredential puts an api_key, so its absence proves nothing was saved.
			if _, statErr := os.Stat(credentialsPath); !os.IsNotExist(statErr) {
				t.Fatalf("os.Stat(%s) = %v, want the credentials file to be absent", credentialsPath, statErr)
			}
			if got := readFileString(t, configPath); got != configBody {
				t.Fatalf("config file rewritten on refusal:\ngot  %q\nwant %q", got, configBody)
			}
			reloaded, err := config.Load("")
			if err != nil {
				t.Fatalf("config.Load after refusal: %v", err)
			}
			if got := reloaded.AuthHeader(); got != authTestLegacyConfigHeader {
				t.Fatalf("reloaded AuthHeader() = %q, want the untouched legacy value %q", got, authTestLegacyConfigHeader)
			}
			assertAbsentUnderHome(t, home, authTestRejectedToken)
		})
	}
}

// TestInitAPIKeyStepClearsLegacyHeader guards the clear-then-save pair that
// init.go duplicates from auth.go. `zotio init` prompts for the same secret and
// stores it through the same Config.SaveCredential path, so it must leave the
// legacy auth_header slot just as empty as `auth set-token` does.
func TestInitAPIKeyStepClearsLegacyHeader(t *testing.T) {
	home := isolateAuthHome(t)
	writeAuthTestConfigFile(t, "base_url = \""+authTestBaseURL+"\"\n")
	credentialsPath := writeAuthTestCredentialsFile(t, "auth_header = \""+authTestLegacyCredHeader+"\"\n")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// The step short-circuits on a working credential, so the seeded store must
	// hold the legacy slot and nothing AuthHeader() can resolve.
	if got := cfg.AuthHeader(); got != "" {
		t.Fatalf("seeded AuthHeader() = %q, want empty so the prompt runs", got)
	}
	if cfg.AuthHeaderVal != authTestLegacyCredHeader {
		t.Fatalf("seeded AuthHeaderVal = %q, want %q", cfg.AuthHeaderVal, authTestLegacyCredHeader)
	}

	cmd := &cobra.Command{Use: "init"}
	cmd.SetIn(strings.NewReader(authTestNewToken + "\n"))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	ok, report := runInitAPIKeyStep(cmd, &rootFlags{}, cfg)
	if !ok || report.Status != "saved" {
		t.Fatalf("runInitAPIKeyStep = (%t, %+v), want (true, status \"saved\")", ok, report)
	}
	if report.Step != initStepAPIKey {
		t.Fatalf("report.Step = %q, want %q", report.Step, initStepAPIKey)
	}

	reloaded, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load after init step: %v", err)
	}
	if got := reloaded.AuthHeader(); got != authTestNewToken {
		t.Fatalf("reloaded AuthHeader() = %q, want %q", got, authTestNewToken)
	}
	if reloaded.AuthHeaderVal != "" {
		t.Fatalf("reloaded AuthHeaderVal = %q, want empty legacy slot", reloaded.AuthHeaderVal)
	}
	if got := readFileString(t, credentialsPath); strings.Contains(got, "auth_header") {
		t.Fatalf("credentials file still declares auth_header:\n%s", got)
	}
	assertAbsentUnderHome(t, home, authTestLegacyCredHeader)
}
