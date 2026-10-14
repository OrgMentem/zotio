// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestStoreStateGuidance pins the consumer-visible store-state contract in
// one place: a missing store is a precondition error (exit 9) that guides the
// operator to sync, while a corrupt
// store reports a contextual open failure and must never look missing. Every
// local command that opens the store owes the same two behaviors.
func TestStoreStateGuidance(t *testing.T) {
	commands := map[string]func(t *testing.T) *cobra.Command{
		"items summarize": func(t *testing.T) *cobra.Command {
			t.Helper()
			cmd := newItemsSummarizeCmd(&rootFlags{})
			cmd.SetArgs([]string{"ABCD2345"})
			return cmd
		},
	}
	for name, newCmd := range commands {
		t.Run("missing store guides sync/"+name, func(t *testing.T) {
			isolateDemoEnv(t, "0")
			cmd := newCmd(t)
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var out bytes.Buffer
			cmd.SetOut(&out)
			err := cmd.Execute()
			if ExitCode(err) != 9 || err == nil || !strings.Contains(err.Error(), "zotio sync") {
				t.Fatalf("%s with missing store: err = %v (exit %d), want precondition (9) guiding to 'zotio sync'", name, err, ExitCode(err))
			}
			if out.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing", out.String())
			}
		})
		t.Run("corrupt store does not look missing/"+name, func(t *testing.T) {
			isolateDemoEnv(t, "0")
			dbPath := helpersTestDefaultDBPath(t, "zotio")
			if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
				t.Fatalf("mkdir db dir: %v", err)
			}
			if err := os.WriteFile(dbPath, []byte("not a SQLite database"), 0o600); err != nil {
				t.Fatalf("write corrupt db: %v", err)
			}
			cmd := newCmd(t)
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			var out bytes.Buffer
			cmd.SetOut(&out)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "opening local database") {
				t.Fatalf("%s error = %v, want contextual store-open failure", name, err)
			}
			if strings.Contains(out.String(), "Run 'zotio sync' first.") {
				t.Fatalf("stdout = %q, must not misclassify corrupt store as missing", out.String())
			}
		})
	}
}
