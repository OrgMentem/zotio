// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package desktop

import (
	"io"
	"syscall"
)

// holdLock takes the lock as Mozilla's nsProfileLock::LockWithFcntl does:
// open (creating and truncating), then F_SETLK a write lock over the whole
// file.
func holdLock(path string) (func(), error) {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}
	if err := syscall.FcntlFlock(uintptr(fd), syscall.F_SETLK, &lk); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	return func() { _ = syscall.Close(fd) }, nil
}
