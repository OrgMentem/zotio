// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// openTTYPair returns the master and slave ends of a new pseudo-terminal.
// The slave end reports as a terminal to isTerminal; output written to it is
// readable from the master end.
func openTTYPair() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	fd := master.Fd()
	ioctl := func(req uintptr, arg unsafe.Pointer) error {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); errno != 0 {
			return errno
		}
		return nil
	}
	var name [128]byte
	for _, step := range []struct {
		req uintptr
		arg unsafe.Pointer
	}{
		{syscall.TIOCPTYGRANT, nil},
		{syscall.TIOCPTYUNLK, nil},
		{syscall.TIOCPTYGNAME, unsafe.Pointer(&name[0])},
	} {
		if err := ioctl(step.req, step.arg); err != nil {
			master.Close()
			return nil, nil, fmt.Errorf("pty ioctl %#x: %w", step.req, err)
		}
	}
	path := string(name[:bytes.IndexByte(name[:], 0)])
	slave, err = os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}
