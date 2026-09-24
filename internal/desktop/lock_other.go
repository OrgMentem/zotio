// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package desktop

import (
	"fmt"
	"runtime"
)

func probeLock(string) LockProbe {
	return LockProbe{State: LockError, Err: fmt.Errorf("profile lock probing is not supported on %s", runtime.GOOS)}
}
