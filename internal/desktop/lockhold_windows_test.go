// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build windows

package desktop

import "syscall"

// fileFlagDeleteOnClose is FILE_FLAG_DELETE_ON_CLOSE.
const fileFlagDeleteOnClose = 0x04000000

// holdLock takes the lock as Mozilla's nsProfileLock does on Windows: an
// exclusive (share mode 0) handle that deletes the file when it closes.
func holdLock(path string) (func(), error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_ALWAYS, fileFlagDeleteOnClose, 0)
	if err != nil {
		return nil, err
	}
	return func() { _ = syscall.CloseHandle(h) }, nil
}
