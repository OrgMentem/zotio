// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cliutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const appName = "zotio"

// AppName returns the current per-user directory name used for config, data,
// state, and cache. Callers deriving app-scoped paths MUST use this rather than
// hardcoding the string, so the name has a single source of truth.
func AppName() string { return appName }

const envPrefix = "ZOTERO"

type PathKind int

const (
	PathKindConfig PathKind = iota
	PathKindData
	PathKindState
	PathKindCache
)

// pathHomeOverride holds the --home flag value. SetHomeOverride writes
// it once at command init; the MCP server's request handlers resolve
// paths concurrently through ResolveKindDir, so access is guarded.
var (
	pathHomeOverrideMu sync.RWMutex
	pathHomeOverride   string
	pathWarnedMu       sync.Mutex
	pathWarned         = map[string]struct{}{}
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

func SetHomeOverride(path string) (func(), error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return setHomeOverride(""), nil
	}
	clean, ok := CleanPathOverride(path)
	if !ok {
		return nil, fmt.Errorf("invalid --home %q: path must be absolute", path)
	}
	if info, err := os.Stat(clean); err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("--home %q: not a directory", clean)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("checking --home %q: %w", clean, err)
	}
	return setHomeOverride(clean), nil
}

// setHomeOverride swaps in value under lock and returns a restore
// function that reinstates the prior value (also under lock).
func setHomeOverride(value string) func() {
	pathHomeOverrideMu.Lock()
	previous := pathHomeOverride
	pathHomeOverride = value
	pathHomeOverrideMu.Unlock()
	return func() {
		pathHomeOverrideMu.Lock()
		pathHomeOverride = previous
		pathHomeOverrideMu.Unlock()
	}
}

func homeOverride() string {
	pathHomeOverrideMu.RLock()
	defer pathHomeOverrideMu.RUnlock()
	return pathHomeOverride
}

func HomeOverrideActive() bool {
	return homeOverride() != ""
}

func ConfigDir() (string, error) {
	return KindDir(PathKindConfig)
}

func DataDir() (string, error) {
	return KindDir(PathKindData)
}

func StateDir() (string, error) {
	return KindDir(PathKindState)
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
		if dirFile, err := os.Open(dir); err == nil {
			_ = dirFile.Sync()
			_ = dirFile.Close()
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
	if override := homeOverride(); override != "" {
		return pathResolution(kind, filepath.Join(override, kindName(kind)), "--home", "--home", ignored), nil
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

func AllPathResolutions() ([]PathResolution, error) {
	kinds := []PathKind{PathKindConfig, PathKindData, PathKindState, PathKindCache}
	resolutions := make([]PathResolution, 0, len(kinds))
	for _, kind := range kinds {
		resolution, err := ResolveKindDir(kind)
		if err != nil {
			return nil, err
		}
		resolutions = append(resolutions, resolution)
	}
	return resolutions, nil
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
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
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
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home dir: %w", err)
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
