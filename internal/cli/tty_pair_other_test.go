//go:build !darwin && !linux

// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"os"
)

// openTTYPair reports that pseudo-terminals are unavailable on this platform.
func openTTYPair() (master, slave *os.File, err error) {
	return nil, nil, errors.New("pseudo-terminals are not supported on this platform")
}
