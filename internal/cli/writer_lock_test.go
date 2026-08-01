// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zotio/internal/cliutil"

	"github.com/spf13/cobra"
)

func useWriterLockTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func saveWriterLockTestProfile(t *testing.T, values map[string]string) {
	t.Helper()
	if err := saveProfileStore(&profileStore{Profiles: map[string]Profile{
		"writer-lock": {Name: "writer-lock", Values: values},
	}}); err != nil {
		t.Fatalf("saving profile: %v", err)
	}
}

func newProfileWriterLockTestRoot(flags *rootFlags, path []string, run func(*cobra.Command, []string) error) *cobra.Command {
	root := &cobra.Command{
		Use: "zotio",
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return applySelectedProfile(cmd, flags)
		},
	}
	root.PersistentFlags().BoolVar(&flags.dryRun, "dry-run", false, "")
	root.PersistentFlags().BoolVar(&flags.yes, "yes", false, "")
	root.PersistentFlags().StringVar(&flags.profileName, "profile", "", "")

	leaf := &cobra.Command{Use: path[len(path)-1], RunE: run}
	parent := &cobra.Command{Use: path[0]}
	if path[0] == "creators" {
		leaf.Flags().Bool("orcid", false, "")
	}
	parent.AddCommand(leaf)
	root.AddCommand(parent)
	installInstallationWriterLocks(root, flags)
	return root
}

func TestInstallationWriterLockSerializesWritersBeforeHandlers(t *testing.T) {
	useWriterLockTestHome(t)
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
	useWriterLockTestHome(t)
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
	useWriterLockTestHome(t)
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

func TestInstallationWriterLockIgnoresConfigAndDataDirSelection(t *testing.T) {
	home := useWriterLockTestHome(t)
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
	err := second.ExecuteContext(context.Background())
	if ExitCode(err) != 9 {
		t.Fatalf("different --config writer exit code = %d, want 9 (err = %v)", ExitCode(err), err)
	}
	if secondRan {
		t.Fatal("different --config writer entered its handler")
	}

	t.Setenv("ZOTIO_DATA_DIR", filepath.Join(t.TempDir(), "other-data"))
	pathWithOtherDataDir, err := installationWriterLockPath(&rootFlags{})
	if err != nil {
		t.Fatalf("lock path with ZOTIO_DATA_DIR: %v", err)
	}
	wantPath := filepath.Join(home, ".zotio", ".writer.lock")
	if pathWithOtherDataDir != wantPath {
		t.Fatalf("lock path with ZOTIO_DATA_DIR = %q, want shared profile scope %q", pathWithOtherDataDir, wantPath)
	}

	close(release)
	if err := <-firstErr; err != nil {
		t.Fatalf("first writer: %v", err)
	}
}

func TestAuthAndProfileWritersShareInstallationLockScope(t *testing.T) {
	useWriterLockTestHome(t)
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

func TestInstallationWriterLockExceptionsResolveAndClassify(t *testing.T) {
	useWriterLockTestHome(t)
	tests := []struct {
		path            string
		flags           rootFlags
		flag            string
		flagValue       string
		wantMode        writerLockMode
		wantAcquireLock bool
	}{
		{path: "items new", wantMode: writerLockOnNotDryRun, wantAcquireLock: true},
		{path: "items new", flags: rootFlags{dryRun: true}, wantMode: writerLockOnNotDryRun},
		{path: "import", wantMode: writerLockOnCommandNotDryRun, wantAcquireLock: true},
		{path: "import", flag: "dry-run", flagValue: "true", wantMode: writerLockOnCommandNotDryRun},
		{path: "import url", wantMode: writerLockOnNotDryRun, wantAcquireLock: true},
		{path: "import url", flags: rootFlags{dryRun: true}, wantMode: writerLockOnNotDryRun},
		{path: "import file", wantMode: writerLockOnNotDryRun, wantAcquireLock: true},
		{path: "import file", flags: rootFlags{dryRun: true}, wantMode: writerLockOnNotDryRun},
		{path: "import pmid", wantMode: writerLockOnNotDryRun, wantAcquireLock: true},
		{path: "import pmid", flag: "dry-run", flagValue: "true", wantMode: writerLockOnNotDryRun},
		{path: "import arxiv", wantMode: writerLockOnNotDryRun, wantAcquireLock: true},
		{path: "import arxiv", flag: "dry-run", flagValue: "true", wantMode: writerLockOnNotDryRun},
		{path: "import isbn", wantMode: writerLockOnNotDryRun, wantAcquireLock: true},
		{path: "import isbn", flag: "dry-run", flagValue: "true", wantMode: writerLockOnNotDryRun},
		{path: "vault push", wantMode: writerLockOnNotDryRun, wantAcquireLock: true},
		{path: "vault push", flags: rootFlags{dryRun: true}, wantMode: writerLockOnNotDryRun},
		{path: "vault pull", wantMode: writerLockOnNotDryRun, wantAcquireLock: true},
		{path: "vault pull", flags: rootFlags{dryRun: true}, wantMode: writerLockOnNotDryRun},
		{path: "vault sync", wantMode: writerLockOnNotDryRun, wantAcquireLock: true},
		{path: "vault sync", flags: rootFlags{dryRun: true}, wantMode: writerLockOnNotDryRun},
		{path: "vault resolve", wantMode: writerLockAlways, wantAcquireLock: true},
		{path: "creators audit", wantMode: writerLockOnORCID},
		{path: "creators audit", flag: "orcid", flagValue: "true", wantMode: writerLockOnORCID, wantAcquireLock: true},
	}
	for _, tt := range tests {
		t.Run(tt.path+"/"+tt.flagValue, func(t *testing.T) {
			flags := tt.flags
			root := newRootCmd(&flags)
			flags.dryRun = tt.flags.dryRun
			cmd, _, err := root.Find(strings.Fields(tt.path))
			if err != nil || cmd == nil || !cmd.Runnable() {
				t.Fatalf("exception path %q does not resolve to a runnable command: cmd=%v err=%v", tt.path, cmd, err)
			}
			if tt.flag != "" {
				if err := cmd.Flags().Set(tt.flag, tt.flagValue); err != nil {
					t.Fatalf("setting %s on %s: %v", tt.flag, tt.path, err)
				}
			}
			mode, ok := installationWriterLockModeForCommand(root, cmd)
			if !ok {
				t.Fatalf("exception path %q was not installed", tt.path)
			}
			if mode != tt.wantMode {
				t.Fatalf("%s mode = %d, want %d", tt.path, mode, tt.wantMode)
			}
			if got := shouldAcquireInstallationWriterLock(cmd, mode, &flags); got != tt.wantAcquireLock {
				t.Fatalf("%s lock decision = %t, want %t", tt.path, got, tt.wantAcquireLock)
			}
		})
	}
}

func TestProfileValuesDetermineWriterLockEligibility(t *testing.T) {
	tests := []struct {
		name     string
		path     []string
		profile  map[string]string
		wantBusy bool
		wantRun  bool
	}{
		{name: "dry run preview", path: []string{"items", "new"}, profile: map[string]string{"dry-run": "true"}, wantRun: true},
		{name: "live writer", path: []string{"items", "new"}, profile: map[string]string{}, wantBusy: true},
		{name: "yes gated apply", path: []string{"items", "create"}, profile: map[string]string{"yes": "true"}, wantBusy: true},
		{name: "orcid sidecar", path: []string{"creators", "audit"}, profile: map[string]string{"orcid": "true"}, wantBusy: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useWriterLockTestHome(t)
			saveWriterLockTestProfile(t, tt.profile)

			holder := &cobra.Command{Use: "holder"}
			started := make(chan struct{})
			release := make(chan struct{})
			holderErr := make(chan error, 1)
			go func() {
				holderErr <- withInstallationWriterLock(holder, &rootFlags{}, "holder", func() error {
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

			ran := false
			flags := &rootFlags{}
			root := newProfileWriterLockTestRoot(flags, tt.path, func(*cobra.Command, []string) error {
				ran = true
				return nil
			})
			root.SilenceErrors, root.SilenceUsage = true, true
			root.SetArgs(append([]string{"--profile", "writer-lock"}, tt.path...))
			err := root.ExecuteContext(context.Background())
			if tt.wantBusy {
				if ExitCode(err) != 9 {
					t.Fatalf("profile %s exit code = %d, want 9 (err = %v)", tt.name, ExitCode(err), err)
				}
			} else if err != nil {
				t.Fatalf("profile %s execution: %v", tt.name, err)
			}
			if ran != tt.wantRun {
				t.Fatalf("profile %s handler ran = %t, want %t", tt.name, ran, tt.wantRun)
			}
			if !flags.profileApplied {
				t.Fatalf("profile %s was not applied before lock eligibility", tt.name)
			}

			close(release)
			if err := <-holderErr; err != nil {
				t.Fatalf("holder: %v", err)
			}
		})
	}
}

func TestProfileWriterLockReleasesAfterHandlerError(t *testing.T) {
	for _, profile := range []map[string]string{
		{},
		{"dry-run": "true"},
	} {
		t.Run(fmt.Sprint(profile), func(t *testing.T) {
			useWriterLockTestHome(t)
			saveWriterLockTestProfile(t, profile)
			handlerErr := errors.New("handler failed")
			flags := &rootFlags{}
			root := newProfileWriterLockTestRoot(flags, []string{"items", "new"}, func(*cobra.Command, []string) error {
				return handlerErr
			})
			root.SilenceErrors, root.SilenceUsage = true, true
			root.SetArgs([]string{"--profile", "writer-lock", "items", "new"})
			if err := root.ExecuteContext(context.Background()); !errors.Is(err, handlerErr) {
				t.Fatalf("writer handler error = %v, want %v", err, handlerErr)
			}

			probe := &cobra.Command{Use: "probe"}
			if err := withInstallationWriterLock(probe, &rootFlags{}, "probe", func() error { return nil }); err != nil {
				t.Fatalf("writer lock leaked after handler error: %v", err)
			}
		})
	}
}

func TestInstallationWriterLockCandidatesResolve(t *testing.T) {

	home := useWriterLockTestHome(t)
	flags := &rootFlags{}
	root := newRootCmd(flags)
	modes := installationWriterLockModes(root)
	for _, entry := range buildCapabilityRegistry(root) {
		if entry.Operation != "write" && entry.Operation != "sync" {
			continue
		}
		mode, ok := modes[entry.Path]
		if !ok {
			t.Errorf("capability writer %q (%s) was not installed", entry.Path, entry.Operation)
			continue
		}
		wantMode := writerLockOnApply
		if entry.Operation == "sync" {
			wantMode = writerLockAlways
		}
		if explicit, ok := explicitInstallationWriterCommands[entry.Path]; ok {
			wantMode = explicit
		}
		if mode != wantMode {
			t.Errorf("capability writer %q mode = %d, want %d", entry.Path, mode, wantMode)
		}
		if mode == writerLockOnApply {
			cmd, _, err := root.Find(strings.Fields(entry.Path))
			if err != nil || cmd == nil {
				t.Errorf("finding capability writer %q: cmd=%v err=%v", entry.Path, cmd, err)
				continue
			}
			if shouldAcquireInstallationWriterLock(cmd, mode, &rootFlags{}) {
				t.Errorf("confirmation-gated writer %q locked without --yes", entry.Path)
			}
			if !shouldAcquireInstallationWriterLock(cmd, mode, &rootFlags{yes: true}) {
				t.Errorf("confirmation-gated writer %q did not lock with --yes", entry.Path)
			}
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

	configPath := filepath.Join(t.TempDir(), "named.toml")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "env.toml"))
	t.Setenv("ZOTIO_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	fromFlag, err := installationWriterLockPath(&rootFlags{configPath: configPath})
	if err != nil {
		t.Fatalf("lock path from --config: %v", err)
	}
	fromEnv, err := installationWriterLockPath(&rootFlags{})
	if err != nil {
		t.Fatalf("lock path from env: %v", err)
	}
	wantPath := filepath.Join(home, ".zotio", ".writer.lock")
	if fromFlag != wantPath || fromEnv != wantPath {
		t.Fatalf("installation lock paths = --config %q, env %q; want shared profile scope %q", fromFlag, fromEnv, wantPath)
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
