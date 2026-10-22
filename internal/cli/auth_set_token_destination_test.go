// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const destinationTestToken = "FIXTURE-destination-api-token"

func runAuthSetTokenWithFlags(t *testing.T, flags *rootFlags, stdin string) string {
	t.Helper()
	cmd := newAuthSetTokenCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetIn(strings.NewReader(stdin))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--stdin"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("auth set-token --stdin: %v (output %q)", err, out.String())
	}
	return out.String()
}

func decodeJSONObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("output %q is not a JSON object: %v", raw, err)
	}
	return got
}

// The token lands in the credentials file, not in the config file the command
// used to name. An operator who backs up, mounts or deletes the named file
// expecting the secret must find it there, so preview and success must report
// the file that really receives the key, for a relocated data directory and an
// explicit --config path alike.
func TestAuthSetTokenReportsTheFileThatReceivesTheToken(t *testing.T) {
	setup := func(t *testing.T) (configPath, credentialsPath string) {
		t.Helper()
		home := isolateAuthHome(t)
		t.Setenv("ZOTERO_DATA_DIR", filepath.Join(home, "relocated-data"))
		configPath = filepath.Join(home, "elsewhere", "explicit.toml")
		credentialsPath = filepath.Join(home, "relocated-data", "credentials.toml")
		return configPath, credentialsPath
	}

	t.Run("success json", func(t *testing.T) {
		configPath, credentialsPath := setup(t)
		got := decodeJSONObject(t, runAuthSetTokenWithFlags(t, &rootFlags{asJSON: true, configPath: configPath}, destinationTestToken+"\n"))
		if got["saved"] != true || got["config_path"] != configPath || got["credentials_path"] != credentialsPath {
			t.Fatalf("envelope = %v, want saved with config_path %q and credentials_path %q", got, configPath, credentialsPath)
		}
		if !strings.Contains(readFileString(t, credentialsPath), destinationTestToken) {
			t.Fatalf("credentials_path %s does not hold the token it was reported as receiving", credentialsPath)
		}
		if strings.Contains(readFileString(t, configPath), destinationTestToken) {
			t.Fatalf("config file %s holds the token", configPath)
		}
	})

	t.Run("success text", func(t *testing.T) {
		configPath, credentialsPath := setup(t)
		out := runAuthSetTokenWithFlags(t, &rootFlags{configPath: configPath}, destinationTestToken+"\n")
		if !strings.Contains(out, "Token saved to "+credentialsPath) {
			t.Fatalf("output = %q, want it to say the token was saved to %s", out, credentialsPath)
		}
		if strings.Contains(out, "Token saved to "+configPath) {
			t.Fatalf("output = %q names the config file as the destination", out)
		}
	})

	t.Run("dry run previews the real destination and writes nothing", func(t *testing.T) {
		configPath, credentialsPath := setup(t)
		got := decodeJSONObject(t, runAuthSetTokenWithFlags(t, &rootFlags{asJSON: true, dryRun: true, configPath: configPath}, destinationTestToken+"\n"))
		if got["saved"] != false || got["dry_run"] != true || got["credentials_path"] != credentialsPath || got["config_path"] != configPath {
			t.Fatalf("envelope = %v, want a dry run naming credentials_path %q", got, credentialsPath)
		}
		text := runAuthSetTokenWithFlags(t, &rootFlags{dryRun: true, configPath: configPath}, destinationTestToken+"\n")
		if !strings.Contains(text, "would save the token to "+credentialsPath) {
			t.Fatalf("preview = %q, want it to name %s", text, credentialsPath)
		}
		for _, p := range []string{credentialsPath, configPath} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Fatalf("dry run wrote %s (stat err = %v)", p, err)
			}
		}
	})
}

// Under an agentcookie-managed store the token is written to the config file
// and no credentials file is created, so the config path is the destination and
// there is no separate credentials_path to report.
func TestAuthSetTokenReportsConfigFileForAgentcookieManagedStore(t *testing.T) {
	home := isolateAuthHome(t)
	configPath := writeAuthTestConfigFile(t, "base_url = \""+authTestBaseURL+"\"\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(configPath), ".agentcookie-managed"), []byte("managed\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	got := decodeJSONObject(t, runAuthSetTokenWithFlags(t, &rootFlags{asJSON: true}, destinationTestToken+"\n"))
	if got["saved"] != true || got["config_path"] != configPath {
		t.Fatalf("envelope = %v, want saved with config_path %q", got, configPath)
	}
	if _, present := got["credentials_path"]; present {
		t.Fatalf("envelope = %v, want no credentials_path when the config file holds the token", got)
	}
	if !strings.Contains(readFileString(t, configPath), destinationTestToken) {
		t.Fatalf("managed config %s does not hold the token it was reported as receiving", configPath)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "zotio", "credentials.toml")); !os.IsNotExist(err) {
		t.Fatalf("a credentials file was written under an agentcookie-managed store (stat err = %v)", err)
	}

	out := runAuthSetTokenWithFlags(t, &rootFlags{}, destinationTestToken+"\n")
	if !strings.Contains(out, "Token saved to "+configPath) || !strings.Contains(out, "agentcookie-managed") {
		t.Fatalf("output = %q, want the config path named as an agentcookie-managed destination", out)
	}
}
