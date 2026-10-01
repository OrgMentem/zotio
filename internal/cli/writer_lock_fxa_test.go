package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestFxaAuthProfileDryRunTakesNoWriterLock pins that the auth and profile
// --dry-run previews neither create ~/.zotio/.writer.lock nor refuse (exit 9)
// while another writer holds it.
func TestFxaAuthProfileDryRunTakesNoWriterLock(t *testing.T) {
	home := useWriterLockTestHome(t)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	saveWriterLockTestProfile(t, map[string]string{"limit": "5"})
	lockPath := filepath.Join(home, ".zotio", ".writer.lock")

	argsList := [][]string{
		{"--config", configPath, "--dry-run", "auth", "set-token", "--stdin"},
		{"--config", configPath, "--dry-run", "auth", "logout"},
		{"--config", configPath, "--dry-run", "--compact", "profile", "save", "fxa-preview"},
		{"--config", configPath, "--dry-run", "profile", "delete", "writer-lock"},
	}
	run := func(args []string) error {
		root := newRootCmd(&rootFlags{})
		root.SilenceErrors, root.SilenceUsage = true, true
		root.SetIn(strings.NewReader("fxa-token\n"))
		root.SetOut(&strings.Builder{})
		root.SetErr(&strings.Builder{})
		root.SetArgs(args)
		return root.ExecuteContext(context.Background())
	}

	for _, args := range argsList {
		if err := run(args); err != nil {
			t.Fatalf("%s: %v", strings.Join(args[3:], " "), err)
		}
		if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
			t.Fatalf("%s created the writer lock %s (lstat err = %v)", strings.Join(args[3:], " "), lockPath, err)
		}
	}

	holder := &cobra.Command{Use: "holder"}
	started := make(chan struct{})
	release := make(chan struct{})
	holderErr := make(chan error, 1)
	go func() {
		holderErr <- withInstallationWriterLock(holder, &rootFlags{configPath: configPath}, "holder", func() error {
			close(started)
			<-release
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("holder did not acquire installation writer lock")
	}
	for _, args := range argsList {
		if err := run(args); err != nil {
			t.Errorf("%s while writer active: exit %d (err = %v), want success", strings.Join(args[3:], " "), ExitCode(err), err)
		}
	}
	close(release)
	if err := <-holderErr; err != nil {
		t.Fatalf("holder: %v", err)
	}
}
