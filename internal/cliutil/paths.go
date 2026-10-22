// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cliutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
)

const appName = "zotio"

// AppName returns the current per-user directory name used for config, data,
// state, and cache. Callers deriving app-scoped paths MUST use this rather than
// hardcoding the string, so the name has a single source of truth.
func AppName() string { return appName }

// HomeDir returns a usable absolute home directory for per-user paths.
func HomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home dir: %w", err)
	}
	if home == "" {
		return "", fmt.Errorf("home directory is empty")
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("home directory %q is not absolute", home)
	}
	return home, nil
}

const envPrefix = "ZOTERO"

type PathKind int

const (
	PathKindConfig PathKind = iota
	PathKindData
	PathKindState
	PathKindCache
)

// The MCP server's request handlers resolve paths concurrently through
// ResolveKindDir, so the warning set is guarded.
var (
	pathWarnedMu sync.Mutex
	pathWarned   = map[string]struct{}{}
)

type PathResolution struct {
	Kind             PathKind
	KindName         string
	Dir              string
	Rung             string
	Source           string
	IgnoredOverrides []PathIgnoredOverride
}

type PathIgnoredOverride struct {
	Name  string
	Value string
}

func ConfigDir() (string, error) {
	return KindDir(PathKindConfig)
}

func DataDir() (string, error) {
	return KindDir(PathKindData)
}

func CacheDir() (string, error) {
	return KindDir(PathKindCache)
}

func ReadFileWithLegacyFallback(primary, legacy string) ([]byte, string, error) {
	data, err := os.ReadFile(primary)
	if err == nil {
		return data, primary, nil
	}
	if !errors.Is(err, os.ErrNotExist) || legacy == "" || legacy == primary {
		return nil, primary, err
	}
	data, legacyErr := os.ReadFile(legacy)
	if legacyErr != nil {
		return nil, legacy, legacyErr
	}
	return data, legacy, nil
}

// AtomicWriteFile replaces path with data via a temp file and rename, so a
// reader never observes a torn file. It deliberately does NOT fsync: an fsync
// pair costs ~9ms versus ~0.2ms for the rename alone, which is the wrong price
// for regenerable data such as the API response cache written on every GET.
// Use AtomicWriteDurableFile for state whose loss is not recoverable.
func AtomicWriteFile(path string, data []byte, fileMode, dirMode os.FileMode) error {
	return atomicWrite(path, data, fileMode, dirMode, false)
}

// AtomicWriteDurableFile additionally fsyncs the data and the parent directory,
// so both the contents and the rename survive sudden power loss. Reserve it for
// state that cannot be regenerated -- credentials, config, profiles -- and pay
// its fsync cost knowingly.
func AtomicWriteDurableFile(path string, data []byte, fileMode, dirMode os.FileMode) error {
	return atomicWrite(path, data, fileMode, dirMode, true)
}

func atomicWrite(path string, data []byte, fileMode, dirMode os.FileMode, durable bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("creating file dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := tmp.Chmod(fileMode); err != nil {
		cleanup()
		return fmt.Errorf("securing temporary file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("writing temporary file: %w", err)
	}
	if durable {
		if err := tmp.Sync(); err != nil {
			cleanup()
			return fmt.Errorf("syncing temporary file: %w", err)
		}
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("closing temporary file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("publishing file: %w", err)
	}
	if durable {
		// Some platforms cannot sync directories, but the published file remains
		// usable, so do not make a completed write fail for that limitation.
		_ = syncDir(dir)
	}
	return nil
}

// syncDir flushes directory entry changes (a rename or an unlink) in dir to
// stable storage. Windows cannot open a directory for syncing and journals
// metadata itself, so it is a no-op there; a filesystem that reports it cannot
// sync a directory (EINVAL, ENOTSUP, ENOSYS) is also accepted, because retrying
// cannot change that answer. Any other failure is returned. It is a variable so
// tests can observe the barrier, which no unit test can otherwise detect.
var syncDir = func(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil && !errors.Is(syncErr, syscall.EINVAL) && !errors.Is(syncErr, syscall.ENOTSUP) && !errors.Is(syncErr, syscall.ENOSYS) {
		return syncErr
	}
	return closeErr
}

// atomicTempSiblings lists the temporary files an interrupted atomicWrite of
// path can leave behind: ".<base>.<digits>.tmp", the exact shape os.CreateTemp
// produces for atomicWrite's pattern. Matching the digits (not a bare
// wildcard) keeps an unrelated user file such as ".config.toml.bak.tmp" out of
// the sweep.
func atomicTempSiblings(path string) ([]string, error) {
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	prefix := "." + filepath.Base(path) + "."
	var found []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".tmp") {
			continue
		}
		middle := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".tmp")
		if middle == "" || strings.Trim(middle, "0123456789") != "" {
			continue
		}
		found = append(found, filepath.Join(dir, name))
	}
	return found, nil
}

// RemoveAtomicTemps deletes the orphaned temporary files an interrupted
// atomic write of path left beside it. SIGKILL, OOM and power loss skip
// atomicWrite's in-process cleanup, and a credential temp holds the complete
// secret, so a later logout or save must not leave it behind. Callers MUST hold
// the installation writer lock (ADR-0005): with it no other zotio writer has a
// live temporary file for the same target.
func RemoveAtomicTemps(path string) error {
	orphans, err := atomicTempSiblings(path)
	if err != nil {
		return fmt.Errorf("listing leftover temporary files for %s: %w", path, err)
	}
	for _, orphan := range orphans {
		if err := os.Remove(orphan); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing leftover temporary file %s: %w", orphan, err)
		}
	}
	return nil
}

// AtomicWritePrivateFile publishes credentials and config, whose loss forces a
// re-auth, so it is durable by default.
func AtomicWritePrivateFile(path string, data []byte, fileMode, dirMode os.FileMode) error {
	return AtomicWriteDurableFile(path, data, fileMode, dirMode)
}

func KindDir(kind PathKind) (string, error) {
	resolution, err := ResolveKindDir(kind)
	if err != nil {
		return "", err
	}
	return resolution.Dir, nil
}

func ResolveKindDir(kind PathKind) (PathResolution, error) {
	var ignored []PathIgnoredOverride
	if override, ok, skipped := envDir(kindEnvVar(kind)); ok {
		return pathResolution(kind, override, "per-kind-env", kindEnvVar(kind), ignored), nil
	} else if skipped != nil {
		ignored = append(ignored, *skipped)
	}
	if home, ok, skipped := envDir(envPrefix + "_HOME"); ok {
		return pathResolution(kind, filepath.Join(home, kindName(kind)), "home-env", envPrefix+"_HOME", ignored), nil
	} else if skipped != nil {
		ignored = append(ignored, *skipped)
	}
	if xdg, ok, skipped := envDir(xdgEnvVar(kind)); ok {
		return pathResolution(kind, filepath.Join(xdg, appName), "xdg-env", xdgEnvVar(kind), ignored), nil
	} else if skipped != nil {
		ignored = append(ignored, *skipped)
	}
	base, err := defaultBase(kind)
	if err != nil {
		return PathResolution{}, err
	}
	return pathResolution(kind, filepath.Join(base, appName), "platform-default", "platform-default", ignored), nil
}

func pathResolution(kind PathKind, dir, rung, source string, ignored []PathIgnoredOverride) PathResolution {
	return PathResolution{
		Kind:             kind,
		KindName:         kindName(kind),
		Dir:              dir,
		Rung:             rung,
		Source:           source,
		IgnoredOverrides: ignored,
	}
}

func kindName(kind PathKind) string {
	switch kind {
	case PathKindConfig:
		return "config"
	case PathKindData:
		return "data"
	case PathKindState:
		return "state"
	case PathKindCache:
		return "cache"
	default:
		return "unknown"
	}
}

func pathKindEnvSuffix(kind PathKind) string {
	switch kind {
	case PathKindConfig:
		return "CONFIG_DIR"
	case PathKindData:
		return "DATA_DIR"
	case PathKindState:
		return "STATE_DIR"
	case PathKindCache:
		return "CACHE_DIR"
	default:
		return ""
	}
}

func kindEnvVar(kind PathKind) string {
	suffix := pathKindEnvSuffix(kind)
	if suffix == "" {
		return ""
	}
	return envPrefix + "_" + suffix
}

func xdgEnvVar(kind PathKind) string {
	suffix := strings.TrimSuffix(pathKindEnvSuffix(kind), "_DIR")
	if suffix == "" {
		return ""
	}
	return "XDG_" + suffix + "_HOME"
}

func envDir(name string) (string, bool, *PathIgnoredOverride) {
	if name == "" {
		return "", false, nil
	}
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return "", false, nil
	}
	clean, ok := CleanPathOverride(raw)
	if !ok {
		warnSkippedPathOverride(name, raw)
		return "", false, &PathIgnoredOverride{Name: name, Value: raw}
	}
	return clean, true, nil
}

// CleanPathOverride trims, tilde-expands, and validates raw as an
// absolute path, returning the cleaned path and whether it is usable.
func CleanPathOverride(raw string) (string, bool) {
	expanded := expandTilde(strings.TrimSpace(raw))
	if expanded == "" || !filepath.IsAbs(expanded) {
		return "", false
	}
	return filepath.Clean(expanded), true
}

func expandTilde(path string) string {
	if path == "~" {
		if home, err := HomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := HomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func warnSkippedPathOverride(name, raw string) {
	key := name + "\x00" + raw
	pathWarnedMu.Lock()
	defer pathWarnedMu.Unlock()
	if _, ok := pathWarned[key]; ok {
		return
	}
	pathWarned[key] = struct{}{}
	fmt.Fprintf(os.Stderr, "warning: ignoring %s=%q: path must be absolute\n", name, raw)
}

func defaultBase(kind PathKind) (string, error) {
	home, err := HomeDir()
	if err != nil {
		return "", err
	}
	switch kind {
	case PathKindConfig:
		return filepath.Join(home, ".config"), nil
	case PathKindData:
		return filepath.Join(home, ".local", "share"), nil
	case PathKindState:
		return filepath.Join(home, ".local", "state"), nil
	case PathKindCache:
		return filepath.Join(home, ".cache"), nil
	default:
		return "", fmt.Errorf("unknown path kind %d", kind)
	}
}
