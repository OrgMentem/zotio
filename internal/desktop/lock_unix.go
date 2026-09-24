// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package desktop

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// lockFileName is the file Mozilla's nsProfileLock locks with
// fcntl(F_SETLK, F_WRLCK) over its whole length on macOS and Linux. Zotero
// never deletes it, so only the lock, not the file, says whether Zotero runs.
const lockFileName = ".parentlock"

func probeLock(profileDir string) LockProbe {
	path := filepath.Join(profileDir, lockFileName)
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LockProbe{State: LockAbsent}
		}
		return LockProbe{State: LockError, Err: err}
	}
	if !info.Mode().IsRegular() {
		return LockProbe{State: LockError, Err: fmt.Errorf("%s is not a regular file (%s)", path, info.Mode().Type())}
	}
	// Read-only: F_GETLK only asks, and a probe must never create, truncate
	// or lock the file Zotero owns. O_NONBLOCK and O_NOFOLLOW keep a file
	// swapped for a FIFO or a symlink after the Lstat from blocking open(2)
	// or redirecting it.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return LockProbe{State: LockAbsent}
		}
		return LockProbe{State: LockError, Err: fmt.Errorf("opening %s: %w", path, err)}
	}
	// Closing any descriptor drops every fcntl lock THIS process holds on the
	// file. This process holds none, so the close cannot release Zotero's.
	defer syscall.Close(fd)

	// F_GETLK reports a conflicting lock held by ANOTHER process; a lock this
	// process held would read as unlocked, which is why tests hold it from a
	// child process.
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}
	if err := syscall.FcntlFlock(uintptr(fd), syscall.F_GETLK, &lk); err != nil {
		return LockProbe{State: LockError, Err: fmt.Errorf("querying the lock on %s: %w", path, err)}
	}
	if lk.Type == syscall.F_UNLCK {
		return LockProbe{State: LockFree}
	}
	return LockProbe{State: LockHeld, PID: int(lk.Pid)}
}
