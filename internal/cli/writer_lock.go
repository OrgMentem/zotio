// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"zotio/internal/cliutil"

	"github.com/spf13/cobra"
)

type writerLockContextKey struct{}

type writerLockOwnership struct {
	path     string
	lock     *cliutil.WriterLock
	owner    *cobra.Command
	previous context.Context
}

// installationWriterLockPath returns the host-user installation lock path.
//
// Profiles are persisted at ~/.zotio/profiles.json regardless of --config or
// ZOTIO_DATA_DIR. Locking there keeps profile writers serialized with every
// other installation writer that can also update shared credentials, cursors,
// journals, or configuration. A distinct user home is an independent
// installation scope; selecting a config file or data directory is not.
func installationWriterLockPath(_ *rootFlags) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory for writer lock: %w", err)
	}
	absHome, err := filepath.Abs(filepath.Clean(home))
	if err != nil {
		return "", fmt.Errorf("canonicalizing writer lock home %q: %w", home, err)
	}
	return filepath.Join(absHome, ".zotio", ".writer.lock"), nil
}

// withInstallationWriterLock serializes a complete installation-state writer
// transaction. Callers decide whether their command's current mode can mutate.
func withInstallationWriterLock(cmd *cobra.Command, flags *rootFlags, operation string, fn func() error) error {
	lockPath, err := installationWriterLockPath(flags)
	if err != nil {
		return err
	}
	return withPathWriterLock(cmd, lockPath, operation, fn)
}

// withPathWriterLock serializes a complete transaction for one canonical output
// path. A nested RootCmd inherits the command context, so it may reuse only the
// exact lock already held by its caller; distinct output paths remain isolated.
func withPathWriterLock(cmd *cobra.Command, lockPath, operation string, fn func() error) error {
	if cmd == nil {
		return fmt.Errorf("acquiring writer lock for %s: nil command", operation)
	}
	if fn == nil {
		return fmt.Errorf("acquiring writer lock for %s: nil transaction", operation)
	}

	canonicalPath, err := canonicalWriterLockPath(lockPath)
	if err != nil {
		return fmt.Errorf("canonicalizing writer lock for %s: %w", operation, err)
	}
	if ownership := writerLockOwner(cmd); ownership != nil && ownership.path == canonicalPath {
		if ownership.owner != cmd {
			return fn()
		}
		return finishWriterLockOwnership(cmd, ownership, fn())
	}

	ownership, err := acquireWriterLockOwnership(cmd, canonicalPath, operation)
	if err != nil {
		return err
	}
	return finishWriterLockOwnership(cmd, ownership, fn())
}

func acquireWriterLockOwnership(cmd *cobra.Command, lockPath, operation string) (*writerLockOwnership, error) {
	lock, err := cliutil.AcquireWriterLock(lockPath, operation)
	if err != nil {
		var busy *cliutil.WriterLockBusyError
		if errors.As(err, &busy) {
			return nil, preconditionErr(fmt.Errorf("%s cannot acquire writer lock %q because another writer is active; retry after it completes: %w", operation, lockPath, err))
		}
		return nil, fmt.Errorf("acquiring writer lock for %s at %q: %w", operation, lockPath, err)
	}
	ownership := &writerLockOwnership{
		path:     lockPath,
		lock:     lock,
		owner:    cmd,
		previous: commandContext(cmd),
	}
	cmd.SetContext(context.WithValue(ownership.previous, writerLockContextKey{}, ownership))
	return ownership, nil
}

func writerLockOwner(cmd *cobra.Command) *writerLockOwnership {
	ownership, _ := commandContext(cmd).Value(writerLockContextKey{}).(*writerLockOwnership)
	return ownership
}

func commandContext(cmd *cobra.Command) context.Context {
	if cmd != nil && cmd.Context() != nil {
		return cmd.Context()
	}
	return context.Background()
}

func finishWriterLockOwnership(cmd *cobra.Command, ownership *writerLockOwnership, transactionErr error) error {
	cmd.SetContext(ownership.previous)
	if releaseErr := ownership.lock.Release(); releaseErr != nil {
		if transactionErr != nil {
			return errors.Join(transactionErr, fmt.Errorf("releasing writer lock at %q: %w", ownership.path, releaseErr))
		}
		return fmt.Errorf("releasing writer lock at %q: %w", ownership.path, releaseErr)
	}
	return transactionErr
}

func canonicalWriterLockPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("empty lock path")
	}
	return filepath.Abs(filepath.Clean(path))
}

type writerLockMode uint8

const (
	writerLockAlways writerLockMode = iota
	writerLockOnApply
	writerLockOnNotDryRun
	writerLockOnCommandNotDryRun
	writerLockOnORCID
)

// explicitInstallationWriterCommands covers installation writers whose Cobra
// annotations intentionally do not expose them as capability writes, and
// capability writers that do not use the shared --yes mutation gate. Keep this
// narrow so normal readers stay free.
var explicitInstallationWriterCommands = map[string]writerLockMode{
	"auth set-token": writerLockAlways,
	"auth logout":    writerLockAlways,
	"profile save":   writerLockAlways,
	"profile delete": writerLockAlways,
	"init":           writerLockAlways,
	"demo":           writerLockAlways,
	"tail":           writerLockAlways,
	"workflow run":   writerLockOnApply,

	// These commands post or publish by default and preview only with --dry-run.
	"items new":    writerLockOnNotDryRun,
	"import url":   writerLockOnNotDryRun,
	"import file":  writerLockOnNotDryRun,
	"import pmid":  writerLockOnNotDryRun,
	"import arxiv": writerLockOnNotDryRun,
	"import isbn":  writerLockOnNotDryRun,
	"vault push":   writerLockOnNotDryRun,
	"vault pull":   writerLockOnNotDryRun,
	"vault sync":   writerLockOnNotDryRun,

	// Generic import has its own command-local --dry-run flag and is marked
	// capability "other" despite POSTing one record per input line.
	"import": writerLockOnCommandNotDryRun,

	// Resolving always publishes either the vault or Zotero state; it has no
	// dry-run mode. --orcid opens the local store for write, while plain audit
	// uses a read-only handle.
	"vault resolve":  writerLockAlways,
	"creators audit": writerLockOnORCID,
}

// installInstallationWriterLocks derives writer candidates only after the full
// command tree exists, then wraps their original handlers once at the boundary.
func installInstallationWriterLocks(rootCmd *cobra.Command, flags *rootFlags) {
	wrapRootPersistentWriterLockPreRun(rootCmd, flags)
	modes := installationWriterLockModes(rootCmd)

	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, sub := range cmd.Commands() {
			path := strings.TrimPrefix(sub.CommandPath(), rootCmd.Name()+" ")
			if mode, ok := modes[path]; ok && sub.Runnable() {
				wrapInstallationWriterCommand(sub, flags, path, mode)
			}
			walk(sub)
		}
	}
	walk(rootCmd)
}

func installationWriterLockModes(rootCmd *cobra.Command) map[string]writerLockMode {
	modes := make(map[string]writerLockMode)
	for _, entry := range buildCapabilityRegistry(rootCmd) {
		switch entry.Operation {
		case "sync":
			modes[entry.Path] = writerLockAlways
		case "write":
			modes[entry.Path] = writerLockOnApply
		}
	}
	for path, mode := range explicitInstallationWriterCommands {
		modes[path] = mode
	}
	return modes
}

func installationWriterLockModeForCommand(rootCmd, cmd *cobra.Command) (writerLockMode, bool) {
	if rootCmd == nil || cmd == nil || cmd.Root() != rootCmd {
		return 0, false
	}
	path := strings.TrimPrefix(cmd.CommandPath(), rootCmd.Name()+" ")
	mode, ok := installationWriterLockModes(rootCmd)[path]
	return mode, ok
}

func shouldAcquireInstallationWriterLock(cmd *cobra.Command, mode writerLockMode, flags *rootFlags) bool {
	switch mode {
	case writerLockAlways:
		return true
	case writerLockOnApply:
		return resolveMutationMode(flags).Apply
	case writerLockOnNotDryRun:
		if flags != nil && flags.dryRun {
			return false
		}
		dryRun, err := commandBoolFlag(cmd, "dry-run")
		return err != nil || !dryRun
	case writerLockOnCommandNotDryRun:
		dryRun, err := commandBoolFlag(cmd, "dry-run")
		return err != nil || !dryRun
	case writerLockOnORCID:
		orcid, err := commandBoolFlag(cmd, "orcid")
		return err == nil && orcid
	default:
		return false
	}
}

func commandBoolFlag(cmd *cobra.Command, name string) (bool, error) {
	if cmd == nil || cmd.Flags().Lookup(name) == nil {
		return false, fmt.Errorf("flag %q is unavailable", name)
	}
	return cmd.Flags().GetBool(name)
}

func wrapRootPersistentWriterLockPreRun(rootCmd *cobra.Command, flags *rootFlags) {
	original := rootCmd.PersistentPreRunE
	if original == nil {
		return
	}
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if err := applySelectedProfile(cmd, flags); err != nil {
			return err
		}
		mode, ok := installationWriterLockModeForCommand(rootCmd, cmd)
		if !ok || !shouldAcquireInstallationWriterLock(cmd, mode, flags) {
			return original(cmd, args)
		}
		lockPath, err := installationWriterLockPath(flags)
		if err != nil {
			return err
		}

		if ownership := writerLockOwner(cmd); ownership != nil && ownership.path == lockPath {
			return original(cmd, args)
		}
		ownership, err := acquireWriterLockOwnership(cmd, lockPath, cmd.CommandPath())
		if err != nil {
			return err
		}
		if err := original(cmd, args); err != nil {
			return finishWriterLockOwnership(cmd, ownership, err)
		}
		return nil
	}
}

func wrapInstallationWriterCommand(cmd *cobra.Command, flags *rootFlags, operation string, mode writerLockMode) {
	wrap := func(run func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			if ownership := writerLockOwner(cmd); ownership != nil && ownership.owner == cmd {
				return finishWriterLockOwnership(cmd, ownership, run(cmd, args))
			}
			if !shouldAcquireInstallationWriterLock(cmd, mode, flags) {
				return run(cmd, args)
			}
			return withInstallationWriterLock(cmd, flags, operation, func() error {
				return run(cmd, args)
			})
		}
	}
	if cmd.RunE != nil {
		cmd.RunE = wrap(cmd.RunE)
	}

	if cmd.Run != nil {
		run := cmd.Run
		// Cobra's Run callback cannot return an acquisition failure. Promote it
		// to the equivalent RunE form so a busy writer still reports exit 9;
		// command use, flags, annotations, and user-visible behavior are unchanged.
		cmd.Run = nil
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			if ownership := writerLockOwner(cmd); ownership != nil && ownership.owner == cmd {
				run(cmd, args)
				return finishWriterLockOwnership(cmd, ownership, nil)
			}
			if !shouldAcquireInstallationWriterLock(cmd, mode, flags) {
				run(cmd, args)
				return nil
			}
			return withInstallationWriterLock(cmd, flags, operation, func() error {
				run(cmd, args)
				return nil
			})
		}
	}
}
