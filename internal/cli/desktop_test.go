// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"zotio/internal/connector"
	"zotio/internal/desktop"
	"zotio/internal/zoteroprefs"
)

// desktopTestEnv isolates discovery and config: profiles come only from the
// returned pinned directory (or from nowhere when pin is false), and the
// connector answers as up says. Nothing here can reach a real Zotero.
func desktopTestEnv(t *testing.T, pin bool, up bool) (flags *rootFlags, profile string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))
	t.Setenv("ZOTERO_BASE_URL", "")
	t.Setenv("ZOTERO_CONFIG", "")
	t.Setenv(zoteroprefs.ProfileDirEnv, "")
	if pin {
		profile = t.TempDir()
		t.Setenv(zoteroprefs.ProfileDirEnv, profile)
	}
	oldPing := connectorPing
	t.Cleanup(func() { connectorPing = oldPing })
	connectorPing = func(context.Context, *connector.Client) error {
		if up {
			return nil
		}
		return errors.New("dial tcp 127.0.0.1:23119: connect: connection refused")
	}
	return &rootFlags{asJSON: true, agent: true, configPath: testConfigFile(t, "http://localhost:23119/api/users/0")}, profile
}

func runDesktopCmd(t *testing.T, flags *rootFlags, stdin string, args ...string) (map[string]any, string, error) {
	t.Helper()
	cmd := newDesktopCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	err := cmd.ExecuteContext(t.Context())
	if out.Len() == 0 {
		return nil, "", err
	}
	var got map[string]any
	if jerr := json.Unmarshal(out.Bytes(), &got); jerr != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", jerr, out.String())
	}
	return got, out.String(), err
}

func desktopJSONKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// papio parses this object; its field names and meanings are the contract.
func TestDesktopStatusAgentJSONContract(t *testing.T) {
	t.Run("stopped", func(t *testing.T) {
		flags, profile := desktopTestEnv(t, true, false)
		got, _, err := runDesktopCmd(t, flags, "", "status")
		if err != nil {
			t.Fatalf("desktop status: %v", err)
		}
		want := []string{"checked_at", "connector_error", "connector_reachable", "connector_url", "data_dir", "evidence", "profiles", "running", "state"}
		if keys := desktopJSONKeys(got); !slices.Equal(keys, want) {
			t.Fatalf("status keys = %v, want %v", keys, want)
		}
		if got["running"] != false || got["connector_reachable"] != false || got["state"] != "stopped" || got["evidence"] != "none" {
			t.Fatalf("status = %v, want stopped", got)
		}
		profiles, _ := got["profiles"].([]any)
		if len(profiles) != 1 || profiles[0].(map[string]any)["path"] != profile || profiles[0].(map[string]any)["lock"] != "absent" {
			t.Fatalf("profiles = %v, want the pinned profile with an absent lock", got["profiles"])
		}
		if got["connector_url"] != "http://localhost:23119/connector" {
			t.Fatalf("connector_url = %v", got["connector_url"])
		}
	})

	t.Run("ready", func(t *testing.T) {
		flags, _ := desktopTestEnv(t, true, true)
		got, _, err := runDesktopCmd(t, flags, "", "status")
		if err != nil {
			t.Fatalf("desktop status: %v", err)
		}
		if got["running"] != true || got["connector_reachable"] != true || got["state"] != "ready" || got["evidence"] != "connector" {
			t.Fatalf("status = %v, want ready on connector evidence", got)
		}
		if _, ok := got["connector_error"]; ok {
			t.Fatalf("status = %v, want no connector_error when reachable", got)
		}
	})
}

// A Web-API-only configuration has no connector, but the lock still says
// whether Zotero runs, so status reports rather than fails.
func TestDesktopStatusWithANonLocalBaseURLStillReports(t *testing.T) {
	flags, _ := desktopTestEnv(t, true, true)
	flags.configPath = testConfigFile(t, "https://api.zotero.org/users/123")
	got, _, err := runDesktopCmd(t, flags, "", "status")
	if err != nil {
		t.Fatalf("desktop status: %v", err)
	}
	if got["connector_reachable"] != false || !strings.Contains(got["connector_error"].(string), "local Zotero base URL") {
		t.Fatalf("status = %v, want the missing connector explained", got)
	}
}

func TestDesktopWaitExitCodesAndOutcomes(t *testing.T) {
	t.Run("already ready", func(t *testing.T) {
		flags, _ := desktopTestEnv(t, true, true)
		got, _, err := runDesktopCmd(t, flags, "", "wait", "--timeout", "1h0m0s")
		if err != nil {
			t.Fatalf("desktop wait: %v", err)
		}
		if got["outcome"] != "ready" || got["connector_reachable"] != true || got["running"] != true {
			t.Fatalf("wait = %v, want outcome ready", got)
		}
		if _, ok := got["waited_ms"].(float64); !ok {
			t.Fatalf("wait = %v, want a numeric waited_ms", got)
		}
	})

	t.Run("timeout exits 14", func(t *testing.T) {
		flags, _ := desktopTestEnv(t, true, false)
		start := time.Now()
		got, _, err := runDesktopCmd(t, flags, "", "wait", "--timeout", "200ms")
		if code := ExitCode(err); code != 14 {
			t.Fatalf("exit code = %d (err %v), want 14", code, err)
		}
		if got["outcome"] != "timeout" || got["state"] != "stopped" {
			t.Fatalf("wait = %v, want outcome timeout in state stopped", got)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("a 200ms timeout took %v", elapsed)
		}
	})

	// Zotero open past its startup window with a connector that cannot take
	// requests: the caller must learn that, not wait silently or be told to
	// open Zotero.
	for _, state := range []desktop.State{desktop.StateUnresponsive, desktop.StateConnectorOff} {
		t.Run(string(state)+" exits 15", func(t *testing.T) {
			flags, _ := desktopTestEnv(t, true, false)
			old := desktopWait
			t.Cleanup(func() { desktopWait = old })
			desktopWait = func(context.Context, desktop.WaitOptions) (desktop.Status, error) {
				return desktop.Status{Running: true, State: state, Evidence: desktop.EvidenceProfileLock, Profiles: []desktop.ProfileStatus{}}, desktop.ErrStuck
			}
			got, _, err := runDesktopCmd(t, flags, "", "wait", "--timeout", "1h")
			if code := ExitCode(err); code != 15 {
				t.Fatalf("exit code = %d (err %v), want 15", code, err)
			}
			if got["outcome"] != string(state) || got["state"] != string(state) || got["running"] != true {
				t.Fatalf("wait = %v, want outcome and state %q with running true", got, state)
			}
		})
	}

	t.Run("no profile exits 9", func(t *testing.T) {
		flags, _ := desktopTestEnv(t, false, false)
		got, _, err := runDesktopCmd(t, flags, "", "wait", "--timeout", "5s")
		if code := ExitCode(err); code != 9 {
			t.Fatalf("exit code = %d (err %v), want 9", code, err)
		}
		if got["outcome"] != "no_profile" {
			t.Fatalf("wait = %v, want outcome no_profile", got)
		}
	})

	t.Run("non-local base URL exits 10", func(t *testing.T) {
		flags, _ := desktopTestEnv(t, true, false)
		flags.configPath = testConfigFile(t, "https://api.zotero.org/users/123")
		_, out, err := runDesktopCmd(t, flags, "", "wait")
		if code := ExitCode(err); code != 10 || out != "" {
			t.Fatalf("exit code = %d, stdout %q (err %v); want 10 and no output", code, out, err)
		}
	})

	// A supervisor that dies closes its end of the pipe; the waiter must not
	// outlive it, and must not print an answer nobody reads.
	t.Run("stdin closed under --watch-stdin", func(t *testing.T) {
		flags, _ := desktopTestEnv(t, true, false)
		_, out, err := runDesktopCmd(t, flags, "", "wait", "--watch-stdin")
		if err == nil || ExitCode(err) != 1 || out != "" {
			t.Fatalf("exit code = %d, stdout %q (err %v); want 1 and no output", ExitCode(err), out, err)
		}
	})

	t.Run("negative timeout is a usage error", func(t *testing.T) {
		flags, _ := desktopTestEnv(t, true, false)
		_, _, err := runDesktopCmd(t, flags, "", "wait", "--timeout", "-1s")
		if code := ExitCode(err); code != 2 {
			t.Fatalf("exit code = %d (err %v), want 2", code, err)
		}
	})
}

// Without --watch-stdin an empty stdin (Go's exec default is /dev/null) must
// not end the wait.
func TestDesktopWaitIgnoresStdinUnlessAsked(t *testing.T) {
	flags, _ := desktopTestEnv(t, true, false)
	got, _, err := runDesktopCmd(t, flags, "", "wait", "--timeout", "300ms")
	if ExitCode(err) != 14 || got["outcome"] != "timeout" {
		t.Fatalf("wait with an empty stdin = %v, exit %d; want it to run to the timeout", got, ExitCode(err))
	}
}

func TestDesktopStatusHumanOutputNamesTheState(t *testing.T) {
	flags, profile := desktopTestEnv(t, true, false)
	flags.asJSON, flags.agent = false, false
	cmd := newDesktopCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("desktop status: %v", err)
	}
	for _, want := range []string{"Zotero desktop: stopped", "Profile: " + profile + " (lock absent)", "connection refused"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(profile, ".parentlock")); err == nil {
		t.Fatal("status created a lock file in the profile")
	}
}

// A Zotero that is open but hung, or open with its connector off, must end
// the wait with its own outcome and exit code, so a caller tells the user
// "Zotero is open but not responding" instead of "open Zotero".
func TestDesktopWaitStuckExits15WithTheState(t *testing.T) {
	for _, state := range []desktop.State{desktop.StateUnresponsive, desktop.StateConnectorOff} {
		t.Run(string(state), func(t *testing.T) {
			flags, _ := desktopTestEnv(t, true, false)
			old := desktopWait
			t.Cleanup(func() { desktopWait = old })
			desktopWait = func(context.Context, desktop.WaitOptions) (desktop.Status, error) {
				return desktop.Status{Running: true, State: state, Evidence: desktop.EvidenceProfileLock, Profiles: []desktop.ProfileStatus{}}, desktop.ErrStuck
			}
			got, _, err := runDesktopCmd(t, flags, "", "wait", "--timeout", "1h")
			if code := ExitCode(err); code != 15 {
				t.Fatalf("exit code = %d (err %v), want 15", code, err)
			}
			if got["outcome"] != string(state) || got["state"] != string(state) || got["running"] != true {
				t.Fatalf("wait = %v, want outcome and state %s", got, state)
			}
		})
	}
}
