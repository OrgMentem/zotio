// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// atomicOutputTempPattern is deliberately a short fixed prefix rather than the
// target's own basename: prefixing a long basename can push the temporary name
// past NAME_MAX and fail a publication that would otherwise succeed. It is
// dot-prefixed so a directory listing during a long export stays uncluttered.
const atomicOutputTempPattern = ".zotio-output-*.tmp"

// withAtomicOutputFile publishes an export artifact atomically: produce writes
// into a temporary file in the target's own directory, and only a successful
// return renames that file over the target.
//
// This exists because the direct writers it replaces truncated the target
// before fetching anything, so any failure mid-export destroyed a previously
// good artifact and left either an empty file or a plausible-looking partial
// one. Measured before this change: a failed `export` truncated 11,299,615
// bytes to 0, and `collections export` against a missing key exited non-zero
// after leaving 1,086 complete, valid BibTeX entries on disk. Neither is
// distinguishable downstream from a successful export.
//
// The contract is same-directory replacement using the host filesystem's
// rename semantics, which buys process-failure publication safety, not
// power-loss durability: nothing here fsyncs, because an export is a
// regenerable projection. os.Rename is not documented atomic outside Unix; on
// Windows replacement can fail while another process holds the target open,
// and on NFS a rename may report an error after having succeeded, so
// "publication failed" does not universally imply "target untouched".
//
// Replacement installs a new filesystem object, so hardlink identity,
// ownership, ACLs, xattrs/alternate data streams and timestamps are not
// preserved. Callers choose mode explicitly: pass the mode the direct write
// would have produced, since os.WriteFile leaves an existing file's mode alone
// while an O_TRUNC open followed by Chmod does not.
//
// An existing target that is not a regular file is refused rather than
// resolved. Following a symlink would mean writing through it to keep today's
// behaviour, which reintroduces a time-of-check/time-of-use window that now
// spans the whole export instead of a single open; and renaming onto the link
// itself would silently replace the user's link with a file. Refusing is the
// only option that is both safe and honest. This also refuses FIFOs, sockets
// and devices, which these paths already fail on today.
func withAtomicOutputFile(target string, mode os.FileMode, produce func(io.Writer) error) error {
	if target == "" {
		return fmt.Errorf("atomic output requires a target path")
	}
	if err := checkAtomicOutputTarget(target); err != nil {
		return err
	}

	dir := filepath.Dir(target)
	// The temporary file must share the target's directory or the final rename
	// crosses a filesystem boundary and fails with EXDEV.
	tmp, err := os.CreateTemp(dir, atomicOutputTempPattern)
	if err != nil {
		return fmt.Errorf("creating temporary output in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()

	// Set the published mode before any bytes exist, so the artifact is never
	// briefly readable under a more permissive mode than the caller asked for.
	if err := tmp.Chmod(mode); err != nil {
		return abortAtomicOutput(tmp, tmpPath, fmt.Errorf("setting permissions on temporary output: %w", err))
	}

	if err := produce(tmp); err != nil {
		// The caller's error is the primary failure and must survive: cleanup
		// problems are not allowed to mask why the export actually failed.
		return abortAtomicOutput(tmp, tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("closing temporary output: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("publishing %s: %w", target, err)
	}
	return nil
}

// checkAtomicOutputTarget refuses to publish over anything but an absent path
// or a regular file. os.Lstat, not os.Stat, so a symlink is seen as a symlink
// instead of as whatever it points at.
func checkAtomicOutputTarget(target string) error {
	info, err := os.Lstat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspecting output path %s: %w", target, err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("refusing to publish over symlink %s: pass the file it points to instead", target)
	case info.Mode().IsRegular():
		return nil
	case info.IsDir():
		return fmt.Errorf("refusing to publish over directory %s", target)
	default:
		return fmt.Errorf("refusing to publish over non-regular file %s (mode %s)", target, info.Mode())
	}
}

// abortAtomicOutput discards the temporary file and returns primary unchanged.
// The target is never touched, so a failed publication leaves whatever was
// already there.
func abortAtomicOutput(tmp *os.File, tmpPath string, primary error) error {
	_ = tmp.Close()
	_ = os.Remove(tmpPath)
	return primary
}

// publishedOutputMode reports the mode an atomic publication should use to
// match what a direct write would have left behind. os.WriteFile does not
// change an existing file's permissions, so preserving them is the
// behaviour-compatible choice for callers that used it; fallback applies when
// the target does not exist yet.
func publishedOutputMode(target string, fallback os.FileMode) os.FileMode {
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() {
		return fallback
	}
	return info.Mode().Perm()
}
