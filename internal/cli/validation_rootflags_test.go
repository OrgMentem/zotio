// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func rootflagsIsolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("ZOTIO_DEMO", "0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(home, "missing.toml"))
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0")
	t.Setenv("ZOTERO_API_KEY", "")
	t.Setenv("ZOTERO_PROFILE", "")
	t.Setenv("ZOTERO_GROUP", "")
	savedGroup := activeGroupIDLocked()
	setActiveGroupID("")
	t.Cleanup(func() { setActiveGroupID(savedGroup) })
}

func rootflagsExecute(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := RootCmd()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	err := ExecuteRootInProcess(context.Background(), root)
	return output.String(), err
}

func TestRootflagsUsageBeforeCapabilityPreflight(t *testing.T) {
	rootflagsIsolate(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"library", "health", "--fail-on", "bogus"}, "invalid --fail-on"},
		{[]string{"searches", "materialize", "ABCD2345"}, `required flag "to"`},
		{[]string{"searches", "materialize", "ABCD2345", "--to", "abc"}, "collection"},
		{[]string{"creators", "rename"}, `required flag "from"`},
		{[]string{"creators", "rename", "--from", "Alice"}, `required flag "to"`},
		{[]string{"creators", "rename", "--from", "Alice", "--to", "Alice"}, "must differ"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			out, err := rootflagsExecute(t, tc.args...)
			if ExitCode(err) != 2 || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v (exit %d), want usage error containing %q", err, ExitCode(err), tc.want)
			}
			if strings.Contains(out, "precondition_unmet") {
				t.Fatalf("usage error emitted capability preflight output: %s", out)
			}
		})
	}
}

func TestRootflagsUnexpectedLeafArgumentNamesFlag(t *testing.T) {
	rootflagsIsolate(t)
	for _, command := range [][]string{
		{"schema", "new-item-template"},
		{"schema", "item-type-fields"},
		{"schema", "item-type-creator-types"},
		{"items", "new"},
	} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			args := append(append([]string{}, command...), "journalArticle")
			_, err := rootflagsExecute(t, args...)
			if ExitCode(err) != 2 || err == nil {
				t.Fatalf("error = %v (exit %d), want usage error", err, ExitCode(err))
			}
			for _, want := range []string{"takes no positional arguments", `"journalArticle"`, "--item-type"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "unknown command") {
				t.Errorf("leaf argument described as a subcommand: %v", err)
			}
		})
	}
	_, err := rootflagsExecute(t, "schema", "item-types", "stray")
	if ExitCode(err) != 2 || err == nil || !strings.Contains(err.Error(), `takes no positional arguments (got "stray")`) {
		t.Fatalf("generic leaf error = %v, want named unexpected argument", err)
	}
}

func TestRootflagsMutationLimitRanges(t *testing.T) {
	rootflagsIsolate(t)
	for _, tc := range []struct {
		flag  string
		value string
		want  string
	}{
		{"--max-changes", "-3", "must be -1"},
		{"--max-failures", "-1", "must be 0"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			_, err := rootflagsExecute(t, "tags", "audit", "fix", "--dry-run", tc.flag, tc.value)
			if ExitCode(err) != 2 || err == nil || !strings.Contains(err.Error(), tc.flag) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v (exit %d), want range error naming %s", err, ExitCode(err), tc.flag)
			}
		})
	}
	for _, maxChanges := range []string{"-1", "0", "1"} {
		var flags rootFlags
		root := newRootCmd(&flags)
		ran := false
		root.AddCommand(&cobra.Command{
			Use:         "rootflags-limit-probe",
			Annotations: map[string]string{"mcp:read-only": "false", preflightAnnotationKey: preflightAnnotationSkip},
			RunE: func(*cobra.Command, []string) error {
				ran = true
				if flags.maxFailures != 0 || (maxChanges == "0" && flags.maxChanges != 0) || (maxChanges == "-1" && flags.maxChanges != -1) {
					t.Errorf("sentinel meanings changed: maxChanges=%d maxFailures=%d", flags.maxChanges, flags.maxFailures)
				}
				return nil
			},
		})
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{"rootflags-limit-probe", "--max-changes", maxChanges, "--max-failures", "0"})
		if err := ExecuteRootInProcess(context.Background(), root); err != nil || !ran {
			t.Fatalf("valid limit %s: ran=%v error=%v", maxChanges, ran, err)
		}
	}
	out, err := rootflagsExecute(t, "--help")
	if err != nil || !strings.Contains(out, "0 refuses all writes") || !strings.Contains(out, "-1 = default") || !strings.Contains(out, "0 = unlimited") {
		t.Fatalf("limit help does not describe sentinels: error=%v output=%s", err, out)
	}
}

func TestRootflagsMissingExplicitConfigForEveryCommand(t *testing.T) {
	rootflagsIsolate(t)
	missing := filepath.Join(t.TempDir(), "missing.toml")
	for _, command := range [][]string{
		{"auth", "status"}, {"doctor"}, {"desktop", "status"}, {"vault", "audit"},
		{"journal", "list"}, {"profile", "list"}, {"items", "list"},
	} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			args := append(append([]string{}, command...), "--config", missing, "--agent")
			_, err := rootflagsExecute(t, args...)
			if ExitCode(err) != 10 || err == nil || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), "does not exist") {
				t.Fatalf("error = %v (exit %d), want config error naming %s", err, ExitCode(err), missing)
			}
		})
	}
	for _, command := range [][]string{{"version"}, {"--version"}, {"--help"}, {"help", "items"}, {"completion", "bash"}} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			args := append([]string{"--config=" + missing}, command...)
			if _, err := rootflagsExecute(t, args...); err != nil {
				t.Fatalf("config-independent command failed: %v", err)
			}
		})
	}
	if _, err := rootflagsExecute(t, "profile", "list"); err != nil {
		t.Fatalf("absent implicit configuration must remain valid: %v", err)
	}
}

func TestRootflagsConfigWritersAllowNewExplicitPath(t *testing.T) {
	rootflagsIsolate(t)
	missing := filepath.Join(t.TempDir(), "new.toml")
	if _, err := rootflagsExecute(t, "init", "--config", missing, "--agent"); ExitCode(err) != 9 {
		t.Fatalf("first-run init error = %v (exit %d), want setup-required exit 9", err, ExitCode(err))
	}
	for _, command := range [][]string{{"auth", "set-token", "--stdin"}, {"auth", "logout"}} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			root := RootCmd()
			root.SetIn(strings.NewReader("rootflags-test-token"))
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(append(append([]string{}, command...), "--config", missing, "--dry-run", "--agent"))
			if err := ExecuteRootInProcess(context.Background(), root); err != nil {
				t.Fatalf("configuration writer rejected a new config path: %v", err)
			}
		})
	}
}

func TestRootflagsWebReadClientUsesCommandCancellation(t *testing.T) {
	rootflagsIsolate(t)
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("style") != "apa" {
			t.Errorf("unexpected request %s", r.URL)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		started <- struct{}{}
		select {
		case <-r.Context().Done():
			canceled <- struct{}{}
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	t.Setenv("ZOTERO_BASE_URL", server.URL+"/users/1")
	t.Setenv("ZOTERO_API_KEY", "rootflags-test-key")
	root := RootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"items", "cite", "ABCD2345", "--style", "apa", "--no-cache", "--timeout", "30s"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- ExecuteRootInProcess(ctx, root) }()
	select {
	case <-started:
	case err := <-finished:
		t.Fatalf("citation returned before the request started: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("citation request never reached the server")
	}
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("canceled citation succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("citation ignored command cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("Web API handler did not observe command cancellation")
	}
}
