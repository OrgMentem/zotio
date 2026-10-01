// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// An invalid enum value for a global selector is a mistake in the command
// line, so it must exit 2 like an invalid --group, not the generic 1 that
// retry logic treats as a possibly transient failure.
func TestRfInvalidGlobalSelectorIsUsageError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_PROFILE", "")
	t.Setenv("ZOTERO_GROUP", "")
	for _, tc := range []struct {
		flag string
		want string
	}{
		{flag: "--data-source", want: `invalid --data-source value "bogus"`},
		{flag: "--via", want: `invalid --via value "bogus"`},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			root := RootCmd()
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs([]string{"version", tc.flag, "bogus"})
			err := root.ExecuteContext(context.Background())
			if err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s bogus = %v (exit %d), want usage exit 2 naming %q", tc.flag, err, ExitCode(err), tc.want)
			}
		})
	}
}

// The installation lock is acquired in the root PersistentPreRunE and released
// by the RunE wrapper. Commands that reject their positional arguments must
// not strand it between the two: under zotio-mcp the process keeps serving, so
// a stranded lock refuses every later writer. This holds because Cobra runs
// Args validation before any PersistentPreRunE; the test exercises the real
// tree so a Cobra upgrade or a hook reordering that breaks it is caught.
func TestRfInstallationWriterLockFreeAfterArgsFailure(t *testing.T) {
	useWriterLockTestHome(t)
	t.Setenv("ZOTERO_PROFILE", "")
	t.Setenv("ZOTERO_GROUP", "")
	for _, args := range [][]string{
		{"profile", "save"},
		{"profile", "delete", "one", "two", "--yes"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := RootCmd()
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs(args)
			err := root.ExecuteContext(context.Background())
			if err == nil || !strings.Contains(err.Error(), "arg(s)") {
				t.Fatalf("%v = %v, want a positional-argument failure", args, err)
			}

			probe := &cobra.Command{Use: "probe"}
			if err := withInstallationWriterLock(probe, &rootFlags{}, "probe", func() error { return nil }); err != nil {
				t.Fatalf("installation writer lock still held after %v failed Args validation: %v", args, err)
			}
		})
	}
}
