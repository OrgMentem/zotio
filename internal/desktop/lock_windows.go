// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
)

// lockFileName is the file Mozilla's nsProfileLock opens on Windows with
// share mode 0 and FILE_FLAG_DELETE_ON_CLOSE: nobody else can open it while
// Zotero runs, and it disappears when the last handle closes, crash included.
const lockFileName = "parent.lock"

// errorSharingViolation is ERROR_SHARING_VIOLATION: another handle's share
// mode refuses this open.
const errorSharingViolation syscall.Errno = 32

func probeLock(profileDir string) LockProbe {
	path := filepath.Join(profileDir, lockFileName)
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return LockProbe{State: LockError, Err: err}
	}
	// GENERIC_READ, not zero access: an open requesting no data access is not
	// checked against share modes and would succeed even while Zotero holds
	// the file exclusively. OPEN_EXISTING so the probe never creates it.
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		switch {
		case errors.Is(err, syscall.ERROR_FILE_NOT_FOUND), errors.Is(err, syscall.ERROR_PATH_NOT_FOUND):
			return LockProbe{State: LockAbsent}
		case errors.Is(err, errorSharingViolation):
			return LockProbe{State: LockHeld}
		default:
			return LockProbe{State: LockError, Err: fmt.Errorf("opening %s: %w", path, err)}
		}
	}
	// A leftover file nobody holds (possible only if the delete-on-close was
	// lost, e.g. to power loss). The handle is closed at once: while it is
	// open, a Zotero starting at that instant would fail its exclusive open.
	_ = syscall.CloseHandle(h)
	return LockProbe{State: LockFree}
}
