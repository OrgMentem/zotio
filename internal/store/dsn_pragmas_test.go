// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package store

import (
	"path/filepath"
	"testing"
)

// livePragma reads a setting back off the connection instead of trusting the
// DSN that was supposed to install it.
func livePragma(t *testing.T, s *Store, name string) string {
	t.Helper()
	var value string
	if err := s.db.QueryRow("PRAGMA " + name).Scan(&value); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return value
}

// The DSNs once carried mattn/go-sqlite3 shorthand (_journal_mode,
// _busy_timeout, ...) that the pinned driver does not implement. It ignores
// unknown underscore keys rather than rejecting them, and SQLite drops them
// again as unrecognized URI params, so every pragma was silently lost: the
// store ran in rollback-journal mode with busy_timeout 0, contradicting the
// WAL reader/writer concurrency the two-connection pool is built on and
// leaving contention to fail instantly instead of waiting.
//
// Asserting the DSN string would not have caught it. These read live state.
func TestOpenInstallsTheDSNPragmas(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "pragmas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, tc := range []struct{ pragma, want string }{
		{"journal_mode", "wal"},
		{"busy_timeout", "5000"},
		{"synchronous", "1"}, // NORMAL
		{"foreign_keys", "1"},
	} {
		if got := livePragma(t, s, tc.pragma); got != tc.want {
			t.Errorf("PRAGMA %s = %s, want %s", tc.pragma, got, tc.want)
		}
	}
}

// The read-only connection needs its own busy_timeout: it is the one that runs
// concurrently with a writer, so a dropped timeout turns every overlap into an
// immediate SQLITE_BUSY.
func TestOpenReadOnlyInstallsTheDSNPragmas(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pragmas.db")
	writable, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if got := livePragma(t, s, "busy_timeout"); got != "10000" {
		t.Errorf("PRAGMA busy_timeout = %s, want 10000", got)
	}
	if got := livePragma(t, s, "journal_mode"); got != "wal" {
		t.Errorf("PRAGMA journal_mode = %s, want wal", got)
	}
	// mode=ro is the whole point of this constructor and is a real SQLite URI
	// parameter, unlike the pragmas above; a regression here opens read-write.
	if _, err := s.db.Exec(`CREATE TABLE writes_should_fail (id INTEGER)`); err == nil {
		t.Error("read-only connection accepted a write")
	}
}
