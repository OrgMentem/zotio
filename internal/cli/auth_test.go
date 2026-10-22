// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
//
// The command itself no longer touches the legacy slot: Config.SaveCredential
// is the single owner of that clear. So every assertion below deliberately
// reads the persisted state (a reload, then the two files) rather than the
// in-memory Config the command handed to SaveCredential, which is the only
// layer that still fails when that owner stops clearing.
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
	// Not vacuous despite config.Load zeroing AuthHeaderVal at parse time:
	// LoadCredentials is applied afterwards, so a credentials.toml that still
	// declared auth_header would hand back a live legacy slot here — which is
	// exactly how the fixture above seeds one.
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
// Each arm must fail with a typed exit class, persist no secret, and leave the
// pre-existing legacy credential exactly as it found it.
func TestAuthSetTokenRejectsUnsafeInput(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		stdin    string
		wantErr  string
		wantCode int
	}{
		{
			name:     "stdin flag omitted",
			args:     nil,
			stdin:    authTestRejectedToken + "\n",
			wantErr:  "refusing token on command line",
			wantCode: 2,
		},
		{
			name:     "empty stdin",
			args:     []string{"--stdin"},
			stdin:    "",
			wantErr:  "empty token on stdin",
			wantCode: 4,
		},
		{
			name:     "whitespace only stdin",
			args:     []string{"--stdin"},
			stdin:    "  \t\r\n  \n",
			wantErr:  "empty token on stdin",
			wantCode: 4,
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
			if !errors.As(err, &cErr) || cErr.code != tc.wantCode {
				t.Fatalf("error = %#v, want *cliError with code %d", err, tc.wantCode)
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

// TestInitAPIKeyStepClearsLegacyHeader covers the `zotio init` prompt, which
// stores the pasted secret through the same Config.SaveCredential path as
// `auth set-token` and, like it, clears nothing itself. The step must therefore
// leave the legacy auth_header slot just as empty as `auth set-token` does,
// proven the same way: reload from disk and read the persisted files.
func TestInitAPIKeyStepClearsLegacyHeader(t *testing.T) {
	home := isolateAuthHome(t)
	// No auth_header in config.toml: config.Load would migrate it into api_key
	// and AuthHeader() would resolve, short-circuiting the prompt. Only a
	// credentials.toml auth_header presents a live legacy slot to this step.
	configPath := writeAuthTestConfigFile(t, "base_url = \""+authTestBaseURL+"\"\n")
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

	ok, report := runInitAPIKeyStep(cmd, &rootFlags{}, bufio.NewReader(cmd.InOrStdin()), cfg)
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
	// The reload is the assertion that catches a SaveCredential which stops
	// clearing: with that clear removed, the save rewrites credentials.toml
	// with the legacy auth_header still in it and this reload hands it back
	// (measured — the mutant fails right here).
	if reloaded.AuthHeaderVal != "" {
		t.Fatalf("reloaded AuthHeaderVal = %q, want empty legacy slot", reloaded.AuthHeaderVal)
	}
	// Saving routes every credential to credentials.toml and strips it from
	// config.toml, so the legacy key must not appear in the plain config store
	// either — not even as an empty leftover key.
	if got := readFileString(t, configPath); strings.Contains(got, "auth_header") {
		t.Fatalf("config file now declares auth_header:\n%s", got)
	}
	if got := readFileString(t, credentialsPath); strings.Contains(got, "auth_header") {
		t.Fatalf("credentials file still declares auth_header:\n%s", got)
	}
	assertAbsentUnderHome(t, home, authTestLegacyCredHeader)
}

// firstReadSignal closes started on the first Read so a test knows the command
// is blocked waiting for input rather than still starting up.
type firstReadSignal struct {
	r       io.Reader
	started chan struct{}
	once    sync.Once
}

func (s *firstReadSignal) Read(p []byte) (int, error) {
	s.once.Do(func() { close(s.started) })
	return s.r.Read(p)
}

// A stalled token producer must not wedge the installation. The command holds
// the writer lock while it waits for stdin; cancellation has to end the wait so
// the lock is released, and a token that arrives afterwards must change
// nothing.
func TestAuthSetTokenCancellationStopsWaitingReleasesWriterLockAndSavesNothing(t *testing.T) {
	isolateAuthHome(t)
	configBody := "base_url = \"" + authTestBaseURL + "\"\n"
	configPath := writeAuthTestConfigFile(t, configBody)
	credentialsPath, err := cliutil.CredentialsFilePath()
	if err != nil {
		t.Fatalf("CredentialsFilePath: %v", err)
	}

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	in := &firstReadSignal{r: pr, started: make(chan struct{})}

	root := newRootCmd(&rootFlags{})
	if root.Annotations == nil {
		root.Annotations = map[string]string{}
	}
	root.Annotations[preflightAnnotationKey] = preflightAnnotationSkip
	root.SilenceErrors, root.SilenceUsage = true, true
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(in)
	root.SetArgs([]string{"auth", "set-token", "--stdin"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ExecuteRootInProcess(ctx, root) }()

	select {
	case <-in.started:
	case err := <-done:
		t.Fatalf("auth set-token returned before reading stdin: %v (output %q)", err, out.String())
	case <-time.After(5 * time.Second):
		t.Fatal("auth set-token never started reading stdin")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("auth set-token error = %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("auth set-token kept waiting for stdin after cancellation")
	}

	probe := &cobra.Command{Use: "probe"}
	if err := withInstallationWriterLock(probe, &rootFlags{}, "probe", func() error { return nil }); err != nil {
		t.Fatalf("installation writer lock still held after cancelled auth set-token: %v", err)
	}

	// The producer finally delivers a token and closes: too late.
	if _, err := io.WriteString(pw, authTestRejectedToken+"\n"); err != nil {
		t.Fatalf("late token write: %v", err)
	}
	_ = pw.Close()
	if _, statErr := os.Stat(credentialsPath); !os.IsNotExist(statErr) {
		t.Fatalf("os.Stat(%s) = %v, want no credentials file after a cancelled set-token", credentialsPath, statErr)
	}
	if got := readFileString(t, configPath); got != configBody {
		t.Fatalf("config rewritten after cancelled set-token:\ngot  %q\nwant %q", got, configBody)
	}
	if strings.Contains(out.String(), "Token saved") {
		t.Fatalf("output = %q, want no saved confirmation after cancellation", out.String())
	}
}

// A token that is already waiting on stdin when the context is already
// cancelled must not be persisted: the operator interrupted the command, so a
// durable credential replacement afterwards is a surprise.
func TestAuthSetTokenCancelledContextNeverPersistsAvailableToken(t *testing.T) {
	isolateAuthHome(t)
	configBody := "base_url = \"" + authTestBaseURL + "\"\n"
	configPath := writeAuthTestConfigFile(t, configBody)
	credentialsPath, err := cliutil.CredentialsFilePath()
	if err != nil {
		t.Fatalf("CredentialsFilePath: %v", err)
	}

	cmd := newAuthSetTokenCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetIn(strings.NewReader(authTestRejectedToken + "\n"))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--stdin"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cmd.ExecuteContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation (output %q)", err, out.String())
	}
	if _, statErr := os.Stat(credentialsPath); !os.IsNotExist(statErr) {
		t.Fatalf("os.Stat(%s) = %v, want no credentials file", credentialsPath, statErr)
	}
	if got := readFileString(t, configPath); got != configBody {
		t.Fatalf("config rewritten:\n%s", got)
	}
}
