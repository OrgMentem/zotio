// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

// Package cache provides a file-based response cache with optional embedded database backend.
// The default implementation stores JSON responses as flat files in the CLI cache directory with a TTL.
// For higher-throughput or concurrent-write scenarios, replace the file backend with an
// embedded database such as bolt (go.etcd.io/bbolt), badger (github.com/dgraph-io/badger),
// or sqlite (modernc.org/sqlite).
package cache

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"zotio/internal/cliutil"
)

// Store is a key-value cache backed by the filesystem.
type Store struct {
	Dir string
	TTL time.Duration
}

// New creates a file-based cache store.
func New(dir string, ttl time.Duration) *Store {
	return &Store{Dir: dir, TTL: ttl}
}

// expiredCleanupProbe is a test seam invoked after RemoveExpired has moved the
// entry aside and before it judges and deletes it: the window in which a
// check-then-remove cleanup would delete an entry a concurrent writer had just
// published. Tests use it to publish inside that window deterministically.
var expiredCleanupProbe func(path string)

// Get retrieves a cached value. Returns nil if not found or expired.
func (s *Store) Get(key string) (json.RawMessage, bool) {
	path := s.path(key)
	info, err := os.Stat(path)
	if err != nil {
		return nil, false
	}
	if time.Since(info.ModTime()) > s.TTL {
		RemoveExpired(path, s.TTL)
		// A concurrent Set may have published a fresh value meanwhile, and
		// cleanup leaves that in place: serve it rather than miss.
		if info, err = os.Stat(path); err != nil || time.Since(info.ModTime()) > s.TTL {
			return nil, false
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(data), true
}

// RemoveExpired deletes the cache entry at path if it is older than ttl. It
// never deletes an entry younger than ttl, nor replaces one.
//
// Writers publish entries by atomic rename (cliutil.AtomicWriteFile), so a
// fresh entry can replace path at any moment. Checking the file and then
// removing it by name cannot be made safe against that: the rename can land
// between the check and the remove, and the remove then deletes the fresh
// entry. Instead the entry is first renamed to a private name, which is
// atomic, and only the file actually moved is judged. An expired file is
// deleted. A fresh one was published after the caller saw the entry expire,
// so it is hard-linked back; the link fails rather than replace an entry
// published since, and that newer entry is kept instead. On a filesystem
// without hard links the fresh entry is dropped, which costs a refetch.
func RemoveExpired(path string, ttl time.Duration) {
	aside := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"."+rand.Text()+".expired")
	if err := os.Rename(path, aside); err != nil {
		return
	}
	if expiredCleanupProbe != nil {
		expiredCleanupProbe(path)
	}
	if info, err := os.Stat(aside); err == nil && time.Since(info.ModTime()) <= ttl {
		_ = os.Link(aside, path)
	}
	_ = os.Remove(aside)
}

// Set stores a value in the cache. It returns any filesystem error so callers
// can surface cache-write failures (disk full, permissions) instead of silently
// degrading to a perpetual cache miss.
func (s *Store) Set(key string, value json.RawMessage) error {
	return cliutil.AtomicWriteFile(s.path(key), value, 0o600, 0o700)
}

func (s *Store) path(key string) string {
	h := sha256.Sum256([]byte(key))
	return filepath.Join(s.Dir, hex.EncodeToString(h[:8])+".json")
}
