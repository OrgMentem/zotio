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
		{path: "items new", flags: rootFlags{yes: true, maxChanges: -1}, wantMode: writerLockOnApply, wantAcquireLock: true},
		{path: "items new", wantMode: writerLockOnApply},
		{path: "import", flags: rootFlags{yes: true, maxChanges: -1}, wantMode: writerLockOnApply, wantAcquireLock: true},
		{path: "import", wantMode: writerLockOnApply},
		{path: "import", flags: rootFlags{yes: true, dryRun: true, maxChanges: -1}, wantMode: writerLockOnApply},
		// Now on the shared --yes gate: the lock follows apply mode, not --dry-run.
		{path: "import url", flags: rootFlags{yes: true, maxChanges: -1}, wantMode: writerLockOnApply, wantAcquireLock: true},
		{path: "import url", wantMode: writerLockOnApply},
		{path: "import url", flags: rootFlags{yes: true, dryRun: true, maxChanges: -1}, wantMode: writerLockOnApply},
		{path: "import file", flags: rootFlags{yes: true, maxChanges: -1}, wantMode: writerLockOnApply, wantAcquireLock: true},
		{path: "import file", wantMode: writerLockOnApply},
		{path: "import file", flags: rootFlags{yes: true, dryRun: true, maxChanges: -1}, wantMode: writerLockOnApply},
		{path: "import pmid", flags: rootFlags{yes: true, maxChanges: -1}, wantMode: writerLockOnApply, wantAcquireLock: true},
		{path: "import pmid", wantMode: writerLockOnApply},
		{path: "import arxiv", flags: rootFlags{yes: true, maxChanges: -1}, wantMode: writerLockOnApply, wantAcquireLock: true},
		{path: "import arxiv", wantMode: writerLockOnApply},
		{path: "import isbn", flags: rootFlags{yes: true, maxChanges: -1}, wantMode: writerLockOnApply, wantAcquireLock: true},
		{path: "import isbn", wantMode: writerLockOnApply},
		// The vault commands are on the shared --yes gate now, so the lock
		// follows apply mode rather than --dry-run.
		{path: "vault push", flags: rootFlags{yes: true, maxChanges: -1}, wantMode: writerLockOnApply, wantAcquireLock: true},
		{path: "vault push", wantMode: writerLockOnApply},
		{path: "vault push", flags: rootFlags{yes: true, dryRun: true, maxChanges: -1}, wantMode: writerLockOnApply},
		{path: "vault pull", flags: rootFlags{yes: true, maxChanges: -1}, wantMode: writerLockOnApply, wantAcquireLock: true},
		{path: "vault pull", wantMode: writerLockOnApply},
		{path: "vault pull", flags: rootFlags{yes: true, dryRun: true, maxChanges: -1}, wantMode: writerLockOnApply},
		{path: "vault sync", flags: rootFlags{yes: true, maxChanges: -1}, wantMode: writerLockOnApply, wantAcquireLock: true},
		{path: "vault sync", wantMode: writerLockOnApply},
		{path: "vault sync", flags: rootFlags{yes: true, dryRun: true, maxChanges: -1}, wantMode: writerLockOnApply},
		{path: "vault resolve", flags: rootFlags{yes: true, maxChanges: -1}, wantMode: writerLockOnApply, wantAcquireLock: true},
		{path: "vault resolve", wantMode: writerLockOnApply},
		{path: "vault resolve", flags: rootFlags{yes: true, dryRun: true, maxChanges: -1}, wantMode: writerLockOnApply},
		{path: "creators audit", wantMode: writerLockOnORCID},
		{path: "creators audit", flag: "orcid", flagValue: "true", wantMode: writerLockOnORCID, wantAcquireLock: true},
	}
	for _, tt := range tests {
		t.Run(tt.path+"/"+tt.flagValue, func(t *testing.T) {
			flags := tt.flags
			root := newRootCmd(&flags)
			// newRootCmd binds these to their flag defaults; restore the case's values.
			flags.dryRun = tt.flags.dryRun
			flags.yes = tt.flags.yes
			flags.maxChanges = tt.flags.maxChanges
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
		// items new is on the shared gate now: no --yes means no write, no lock.
		{name: "ungated preview", path: []string{"items", "new"}, profile: map[string]string{}, wantRun: true},
		{name: "live writer", path: []string{"items", "new"}, profile: map[string]string{"yes": "true"}, wantBusy: true},
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
	for _, tc := range []struct {
		profile map[string]string
		// wantHeld records whether the profile should make the command acquire
		// the installation lock at all. Without it, the {} and {dry-run} cases
		// never acquire (items new is writerLockOnApply, which requires an apply
		// mode), so the post-run probe would succeed no matter what the release
		// path did and the release assertion would be vacuous.
		wantHeld bool
	}{
		{profile: map[string]string{}, wantHeld: false},
		{profile: map[string]string{"dry-run": "true"}, wantHeld: false},
		{profile: map[string]string{"yes": "true"}, wantHeld: true},
	} {
		t.Run(fmt.Sprint(tc.profile), func(t *testing.T) {
			useWriterLockTestHome(t)
			saveWriterLockTestProfile(t, tc.profile)
			handlerErr := errors.New("handler failed")
			flags := &rootFlags{}
			root := newProfileWriterLockTestRoot(flags, []string{"items", "new"}, func(*cobra.Command, []string) error {
				// Probe from inside the handler: this is what proves the lock is
				// genuinely held while the body runs, so the post-run probe below
				// is testing the release rather than an absent lock.
				inner := &cobra.Command{Use: "inner"}
				err := withInstallationWriterLock(inner, &rootFlags{}, "inner", func() error { return nil })
				if tc.wantHeld && err == nil {
					t.Error("installation lock was not held while the handler ran")
				}
				if !tc.wantHeld && err != nil {
					t.Errorf("installation lock was held for a non-applying profile: %v", err)
				}
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

// Cobra runs ValidateRequiredFlags/ValidateFlagGroups after every
// PersistentPreRunE and before RunE, so a validation failure returns from
// execute() without ever reaching the RunE wrapper that normally releases the
// installation lock. Under zotio-mcp the process keeps serving, so a leak here
// wedges every later writer command.
func TestInstallationWriterLockReleasesWhenFlagValidationFails(t *testing.T) {
	useWriterLockTestHome(t)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	flags := &rootFlags{configPath: configPath}
	root := &cobra.Command{
		Use:               "zotio",
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
	}
	ran := false
	syncCmd := newWriterLockTestSyncCmd(func(*cobra.Command, []string) error {
		ran = true
		return nil
	})
	syncCmd.Flags().String("input", "", "")
	if err := syncCmd.MarkFlagRequired("input"); err != nil {
		t.Fatalf("marking --input required: %v", err)
	}
	root.AddCommand(syncCmd)
	installInstallationWriterLocks(root, flags)
	root.SilenceErrors, root.SilenceUsage = true, true
	root.SetArgs([]string{"sync"})

	err := root.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "input") {
		t.Fatalf("execute error = %v, want a required-flag failure naming input", err)
	}
	if ran {
		t.Fatal("command body ran despite failing flag validation")
	}

	probe := &cobra.Command{Use: "probe"}
	if err := withInstallationWriterLock(probe, &rootFlags{configPath: configPath}, "probe", func() error { return nil }); err != nil {
		t.Fatalf("writer lock leaked after flag validation failure: %v", err)
	}
}

func TestWithPathWriterLockReleasesAfterPanic(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "writer.lock")
	wantPanic := "transaction panicked"
	panicking := &cobra.Command{Use: "panics"}

	func() {
		defer func() {
			recovered := recover()
			if recovered != wantPanic {
				t.Fatalf("recovered panic = %v, want %v", recovered, wantPanic)
			}
		}()
		_ = withPathWriterLock(panicking, lockPath, "panicking transaction", func() error {
			panic(wantPanic)
		})
		t.Fatal("withPathWriterLock did not propagate the panic")
	}()

	probe := &cobra.Command{Use: "probe"}
	if err := withPathWriterLock(probe, lockPath, "probe", func() error { return nil }); err != nil {
		t.Fatalf("writer lock leaked after panicking transaction: %v", err)
	}
}

// The root pre-run acquires installation ownership and hands release off to the
// RunE wrapper, so a panic inside the command body is the only path that can
// strand the lock. The in-process MCP server recovers that panic and keeps
// serving requests, so a leak here wedges every later writer command.
func TestInstallationWriterLockReleasesAfterHandlerPanic(t *testing.T) {
	useWriterLockTestHome(t)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	wantPanic := "writer handler panicked"
	flags := &rootFlags{configPath: configPath}
	root := &cobra.Command{
		Use:               "zotio",
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
	}
	root.AddCommand(newWriterLockTestSyncCmd(func(*cobra.Command, []string) error {
		panic(wantPanic)
	}))
	installInstallationWriterLocks(root, flags)
	root.SilenceErrors, root.SilenceUsage = true, true
	root.SetArgs([]string{"sync"})

	func() {
		defer func() {
			if recovered := recover(); recovered != wantPanic {
				t.Fatalf("recovered panic = %v, want %v", recovered, wantPanic)
			}
		}()
		_ = root.ExecuteContext(context.Background())
		t.Fatal("writer handler panic did not propagate out of Execute")
	}()

	probe := &cobra.Command{Use: "probe"}
	if err := withInstallationWriterLock(probe, &rootFlags{configPath: configPath}, "probe", func() error { return nil }); err != nil {
		t.Fatalf("writer lock leaked after handler panic: %v", err)
	}
}

func TestInstallationWriterLockReleasesAfterPersistentPreRunPanic(t *testing.T) {
	useWriterLockTestHome(t)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	wantPanic := "persistent pre-run panicked"
	flags := &rootFlags{configPath: configPath}
	root := &cobra.Command{
		Use: "zotio",
		PersistentPreRunE: func(*cobra.Command, []string) error {
			panic(wantPanic)
		},
	}
	root.AddCommand(newWriterLockTestSyncCmd(func(*cobra.Command, []string) error {
		return nil
	}))
	installInstallationWriterLocks(root, flags)
	root.SilenceErrors, root.SilenceUsage = true, true
	root.SetArgs([]string{"sync"})

	func() {
		defer func() {
			recovered := recover()
			if recovered != wantPanic {
				t.Fatalf("recovered panic = %v, want %v", recovered, wantPanic)
			}
		}()
		_ = root.ExecuteContext(context.Background())
		t.Fatal("persistent pre-run panic did not propagate out of Execute")
	}()

	probe := &cobra.Command{Use: "probe"}
	if err := withInstallationWriterLock(probe, &rootFlags{configPath: configPath}, "probe", func() error { return nil }); err != nil {
		t.Fatalf("writer lock leaked after persistent pre-run panic: %v", err)
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

// writerLockIsFree reports whether path's advisory lock can be acquired right
// now. flock is per open file description, so a probe from this same process
// contends with a lock the code under test holds: busy means still held.
func writerLockIsFree(t *testing.T, path string) bool {
	t.Helper()
	lock, err := cliutil.AcquireWriterLock(path, "probe")
	if err != nil {
		var busy *cliutil.WriterLockBusyError
		if errors.As(err, &busy) {
			return false
		}
		t.Fatalf("probing writer lock %q: %v", path, err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("releasing probe lock %q: %v", path, err)
	}
	return true
}

// A nested transaction on a path the same command already holds must reuse the
// lock, not release it. The reuse branch used to defer a release of the OUTER
// ownership, so the outer transaction continued unlocked while a competing
// writer could acquire the same target.
func TestWithPathWriterLockSamePathNestingKeepsLockHeld(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "target.jsonl.lock")
	cmd := &cobra.Command{Use: "nests"}
	innerRan := false

	if err := withPathWriterLock(cmd, lockPath, "outer", func() error {
		if writerLockIsFree(t, lockPath) {
			t.Fatal("outer transaction did not hold the lock")
		}
		if err := withPathWriterLock(cmd, lockPath, "inner", func() error {
			innerRan = true
			if writerLockIsFree(t, lockPath) {
				t.Fatal("lock was released while the inner transaction ran")
			}
			return nil
		}); err != nil {
			return err
		}
		if writerLockIsFree(t, lockPath) {
			t.Fatal("the nested transaction released the outer lock")
		}
		return nil
	}); err != nil {
		t.Fatalf("nested transaction: %v", err)
	}
	if !innerRan {
		t.Fatal("inner transaction never ran")
	}
	if !writerLockIsFree(t, lockPath) {
		t.Fatal("lock still held after the outer transaction returned")
	}
}

// Reuse must consider every lock the command holds, not only the innermost one:
// a command that holds the installation lock and nests an output lock still
// holds the installation lock, so re-entering it must not acquire a second time.
func TestWithPathWriterLockReusesLockHeldBelowTopOfStack(t *testing.T) {
	dir := t.TempDir()
	installationPath := filepath.Join(dir, "installation.lock")
	outputPath := filepath.Join(dir, "target.jsonl.lock")
	cmd := &cobra.Command{Use: "nests"}
	reentered := false

	if err := withPathWriterLock(cmd, installationPath, "installation", func() error {
		return withPathWriterLock(cmd, outputPath, "output", func() error {
			if err := withPathWriterLock(cmd, installationPath, "installation again", func() error {
				reentered = true
				return nil
			}); err != nil {
				return err
			}
			if writerLockIsFree(t, installationPath) {
				t.Fatal("re-entering the installation lock released it")
			}
			if writerLockIsFree(t, outputPath) {
				t.Fatal("the output lock was released early")
			}
			return nil
		})
	}); err != nil {
		t.Fatalf("nested transaction: %v", err)
	}
	if !reentered {
		t.Fatal("re-entrant transaction never ran")
	}
	for _, path := range []string{installationPath, outputPath} {
		if !writerLockIsFree(t, path) {
			t.Fatalf("lock %q still held after the transaction returned", path)
		}
	}
}

// The installation wrapper releases the ownership PersistentPreRunE handed it.
// It used to identify that handoff by owning command alone, so an output-scope
// lock the same command acquired would have been released by the wrapper.
func TestInstallationWrapperDoesNotReleaseForeignPathOwnership(t *testing.T) {
	useWriterLockTestHome(t)
	flags := &rootFlags{}
	installationPath, err := installationWriterLockPath(flags)
	if err != nil {
		t.Fatalf("resolving installation lock path: %v", err)
	}
	outputPath := filepath.Join(t.TempDir(), "target.jsonl.lock")

	ran := false
	cmd := &cobra.Command{Use: "sync", RunE: func(*cobra.Command, []string) error {
		ran = true
		return nil
	}}
	wrapInstallationWriterCommand(cmd, flags, "sync", writerLockAlways)

	ownership, err := acquireWriterLockOwnership(cmd, outputPath, "output")
	if err != nil {
		t.Fatalf("acquiring output lock: %v", err)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("wrapped command: %v", err)
	}
	if !ran {
		t.Fatal("wrapped command body never ran")
	}
	if writerLockIsFree(t, outputPath) {
		t.Fatal("the installation wrapper released an output-scope lock it did not acquire")
	}
	if !writerLockIsFree(t, installationPath) {
		t.Fatal("the installation lock was not released by the wrapper")
	}
	if err := finishWriterLockOwnership(cmd, ownership, nil); err != nil {
		t.Fatalf("releasing output lock: %v", err)
	}
}
