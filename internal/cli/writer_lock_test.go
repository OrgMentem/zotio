// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/cliutil"

	"github.com/spf13/cobra"
)

func TestInstallationWriterLockSerializesWritersBeforeHandlers(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	started := make(chan struct{})
	release := make(chan struct{})
	firstFlags := &rootFlags{configPath: configPath}
	first := newWriterLockTestRoot(firstFlags, newWriterLockTestSyncCmd(func(*cobra.Command, []string) error {
		close(started)
		<-release
		return nil
	}))
	first.SetArgs([]string{"sync"})
	firstErr := make(chan error, 1)
	go func() { firstErr <- first.ExecuteContext(context.Background()) }()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first writer did not enter its handler")
	}

	secondRan := false
	secondFlags := &rootFlags{configPath: configPath}
	second := newWriterLockTestRoot(secondFlags, newWriterLockTestSyncCmd(func(*cobra.Command, []string) error {
		secondRan = true
		return nil
	}))
	second.SetArgs([]string{"sync"})
	err := second.ExecuteContext(context.Background())
	if ExitCode(err) != 9 {
		t.Fatalf("second writer exit code = %d, want 9 (err = %v)", ExitCode(err), err)
	}
	if secondRan {
		t.Fatal("busy writer entered its handler")
	}
	var busy *cliutil.WriterLockBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("busy writer error does not preserve WriterLockBusyError: %v", err)
	}
	if !strings.Contains(err.Error(), "retry") || !strings.Contains(err.Error(), "sync") {
		t.Fatalf("busy writer error lacks operation/retry guidance: %v", err)
	}

	close(release)
	if err := <-firstErr; err != nil {
		t.Fatalf("first writer: %v", err)
	}
}

func TestInstallationWriterLockLeavesPreviewsAndReadsConcurrent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	started := make(chan struct{})
	release := make(chan struct{})
	holderFlags := &rootFlags{configPath: configPath}
	holder := newWriterLockTestRoot(holderFlags, newWriterLockTestSyncCmd(func(*cobra.Command, []string) error {
		close(started)
		<-release
		return nil
	}))
	holder.SetArgs([]string{"sync"})
	holderErr := make(chan error, 1)
	go func() { holderErr <- holder.ExecuteContext(context.Background()) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not enter its handler")
	}

	previewRan := false
	previewFlags := &rootFlags{configPath: configPath}
	previewRoot := &cobra.Command{Use: "zotio"}
	items := &cobra.Command{Use: "items"}
	items.AddCommand(&cobra.Command{Use: "create", RunE: func(*cobra.Command, []string) error {
		previewRan = true
		return nil
	}})
	previewRoot.AddCommand(items)
	installInstallationWriterLocks(previewRoot, previewFlags)
	previewRoot.SetArgs([]string{"items", "create"})
	if err := previewRoot.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("preview while writer active: %v", err)
	}
	if !previewRan {
		t.Fatal("preview handler did not run while writer lock was held")
	}

	readRan := false
	readRoot := &cobra.Command{Use: "zotio"}
	readRoot.AddCommand(&cobra.Command{
		Use:         "read-probe",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(*cobra.Command, []string) error {
			readRan = true
			return nil
		},
	})
	installInstallationWriterLocks(readRoot, &rootFlags{configPath: configPath})
	readRoot.SetArgs([]string{"read-probe"})
	if err := readRoot.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("read while writer active: %v", err)
	}
	if !readRan {
		t.Fatal("read handler did not run while writer lock was held")
	}

	close(release)
	if err := <-holderErr; err != nil {
		t.Fatalf("writer: %v", err)
	}
}

func TestInstallationWriterLockIsReentrantForInheritedRootContext(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	nestedRan := false
	nestedPreRun := false
	nestedFlags := &rootFlags{configPath: configPath}
	nested := &cobra.Command{
		Use: "zotio",
		PersistentPreRunE: func(*cobra.Command, []string) error {
			nestedPreRun = true
			return nil
		},
	}
	nested.AddCommand(newWriterLockTestSyncCmd(func(*cobra.Command, []string) error {
		nestedRan = true
		return nil
	}))
	installInstallationWriterLocks(nested, nestedFlags)
	nested.SetArgs([]string{"sync"})

	outer := &cobra.Command{Use: "outer"}
	if err := withInstallationWriterLock(outer, &rootFlags{configPath: configPath}, "workflow run", func() error {
		return nested.ExecuteContext(outer.Context())
	}); err != nil {
		t.Fatalf("nested writer with inherited context: %v", err)
	}
	if !nestedPreRun || !nestedRan {
		t.Fatalf("nested writer did not complete persistent pre-run and handler: pre-run=%t handler=%t", nestedPreRun, nestedRan)
	}
	if outer.Context().Value(writerLockContextKey{}) != nil {
		t.Fatal("outer command context retained a released writer-lock ownership marker")
	}
}

func TestInstallationWriterLockCustomConfigScopesAreIndependent(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	first := newWriterLockTestRoot(&rootFlags{configPath: filepath.Join(t.TempDir(), "one.toml")}, newWriterLockTestSyncCmd(func(*cobra.Command, []string) error {
		close(started)
		<-release
		return nil
	}))
	first.SetArgs([]string{"sync"})
	firstErr := make(chan error, 1)
	go func() { firstErr <- first.ExecuteContext(context.Background()) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first writer did not enter its handler")
	}

	secondRan := false
	second := newWriterLockTestRoot(&rootFlags{configPath: filepath.Join(t.TempDir(), "two.toml")}, newWriterLockTestSyncCmd(func(*cobra.Command, []string) error {
		secondRan = true
		return nil
	}))
	second.SetArgs([]string{"sync"})
	if err := second.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("independent custom config writer: %v", err)
	}
	if !secondRan {
		t.Fatal("independent custom config writer did not run")
	}

	close(release)
	if err := <-firstErr; err != nil {
		t.Fatalf("first writer: %v", err)
	}
}

func TestAuthAndProfileWritersShareInstallationLockScope(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
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

	for _, args := range [][]string{
		{"--config", configPath, "auth", "logout"},
		{"--config", configPath, "profile", "save", "writer-lock-test", "--json"},
	} {
		root := newRootCmd(&rootFlags{})
		root.SilenceErrors, root.SilenceUsage = true, true
		root.SetArgs(args)
		err := root.ExecuteContext(context.Background())
		if ExitCode(err) != 9 {
			t.Fatalf("%s exit code = %d, want 9 (err = %v)", strings.Join(args[2:], " "), ExitCode(err), err)
		}
	}

	close(release)
	if err := <-holderErr; err != nil {
		t.Fatalf("holder: %v", err)
	}
}

func TestInstallationWriterLockCandidatesResolve(t *testing.T) {
	flags := &rootFlags{}
	root := newRootCmd(flags)
	modes := installationWriterLockModes(root)
	for _, entry := range buildCapabilityRegistry(root) {
		if entry.Operation != "write" && entry.Operation != "sync" {
			continue
		}
		if _, ok := modes[entry.Path]; !ok {
			t.Errorf("capability writer %q (%s) was not installed", entry.Path, entry.Operation)
		}
	}
	for path := range explicitInstallationWriterCommands {
		cmd, _, err := root.Find(strings.Fields(path))
		if err != nil || cmd == nil || !cmd.Runnable() {
			t.Errorf("explicit writer path %q does not resolve to a runnable command: cmd=%v err=%v", path, cmd, err)
		}
		if _, ok := modes[path]; !ok {
			t.Errorf("explicit writer path %q was not installed", path)
		}
	}
	for _, path := range []string{"auth set-token", "auth logout", "profile save", "profile delete"} {
		if modes[path] != writerLockAlways {
			t.Errorf("%s writer mode = %d, want unconditional installation lock", path, modes[path])
		}
	}

	envConfigPath := filepath.Join(t.TempDir(), "env.toml")
	t.Setenv("ZOTERO_CONFIG", envConfigPath)
	configPath := filepath.Join(t.TempDir(), "named.toml")
	fromFlag, err := installationWriterLockPath(&rootFlags{configPath: configPath})
	if err != nil {
		t.Fatalf("lock path from --config: %v", err)
	}
	fromEnv, err := installationWriterLockPath(&rootFlags{})
	if err != nil {
		t.Fatalf("lock path from env: %v", err)
	}
	wantEnvPath, err := filepath.Abs(filepath.Join(filepath.Dir(envConfigPath), ".writer.lock"))
	if err != nil {
		t.Fatalf("canonicalizing expected env lock path: %v", err)
	}
	if fromEnv != wantEnvPath {
		t.Fatalf("env config lock path = %q, want %q", fromEnv, wantEnvPath)
	}
	if fromFlag == fromEnv {
		t.Fatal("custom --config did not select its own lock scope")
	}
}

func newWriterLockTestRoot(flags *rootFlags, command *cobra.Command) *cobra.Command {
	root := &cobra.Command{Use: "zotio"}
	root.AddCommand(command)
	installInstallationWriterLocks(root, flags)
	return root
}

func newWriterLockTestSyncCmd(run func(*cobra.Command, []string) error) *cobra.Command {
	return &cobra.Command{Use: "sync", RunE: run}
}
