// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cliutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gofrs/flock"
)

// osChmod is the filesystem Chmod used by AcquireWriterLock. Overridable in
// tests to simulate Chmod failures without manipulating the real filesystem.
var osChmod = os.Chmod

// WriterLockBusyError reports that another zotio process owns the requested
// writer lock. Callers can use errors.As to classify this as a precondition
// failure without matching its user-facing text.
type WriterLockBusyError struct {
	Operation string
	Path      string
}

func (e *WriterLockBusyError) Error() string {
	return fmt.Sprintf("another zotio writer is active while %s at %q; retry after it exits", e.Operation, e.Path)
}

// WriterLock holds an exclusive advisory lock for one caller's complete
// load-mutate-publish transaction. Its lock file is intentionally retained:
// the operating system releases the advisory lock when its process exits.
type WriterLock struct {
	flock     *flock.Flock
	operation string
	path      string

	mu         sync.Mutex
	released   bool
	releaseErr error
}

// AcquireWriterLock acquires path's exclusive advisory lock without waiting.
// The caller must defer Release until its whole load-mutate-publish transaction
// is complete. A competing writer returns a *WriterLockBusyError.
func AcquireWriterLock(path string, operation string) (*WriterLock, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating writer lock directory %q: %w", dir, err)
	}

	f := flock.New(path)
	locked, err := f.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquiring writer lock for %s at %q: %w", operation, path, releaseAfterFailedAcquire(f, err))
	}
	if !locked {
		return nil, &WriterLockBusyError{Operation: operation, Path: path}
	}

	if err := osChmod(path, 0o600); err != nil {
		if os.IsPermission(err) {
			if !lockFileIsPrivate(path) {
				return nil, fmt.Errorf("securing writer lock for %s at %q: %w", operation, path, releaseAfterFailedAcquire(f, err))
			}
		} else {
			return nil, fmt.Errorf("securing writer lock for %s at %q: %w", operation, path, releaseAfterFailedAcquire(f, err))
		}
	}

	return &WriterLock{flock: f, operation: operation, path: path}, nil
}

// Release unlocks and closes the writer lock. It is safe to call more than
// once; every call returns the result of the first release attempt.
func (l *WriterLock) Release() error {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.released {
		return l.releaseErr
	}
	l.released = true
	l.releaseErr = l.flock.Unlock()
	return l.releaseErr
}

func lockFileIsPrivate(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().Perm()&0o077 == 0
}

func releaseAfterFailedAcquire(f *flock.Flock, cause error) error {
	if err := f.Unlock(); err != nil {
		return errors.Join(cause, fmt.Errorf("releasing partially acquired writer lock: %w", err))
	}
	return cause
}
