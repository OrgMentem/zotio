// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"path/filepath"
	"sync"
	"testing"
)

// TestApplyGroupScopeFromEnvDoesNotClobberActiveScope guards the
// check-then-set ordering inside ApplyGroupScopeFromEnv: when a scope is
// already established (by --group / PersistentPreRunE), the ZOTERO_GROUP
// fallback must not overwrite it. The check and the assignment share one
// write lock (activeGroupMu) so they cannot interleave with a concurrent
// setActiveGroupID from cobra.
func TestApplyGroupScopeFromEnvDoesNotClobberActiveScope(t *testing.T) {
	restore := SnapshotGlobals()
	defer restore()

	t.Setenv("ZOTERO_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTIO_DEMO", "")

	setActiveGroupID("99999")
	t.Setenv("ZOTERO_GROUP", "11111")
	if err := ApplyGroupScopeFromEnv(); err != nil {
		t.Fatalf("ApplyGroupScopeFromEnv() error = %v, want nil", err)
	}
	if got := ActiveGroupID(); got != "99999" {
		t.Fatalf("ActiveGroupID() = %q, want %q (must not clobber active scope)", got, "99999")
	}
	if path, err := DefaultDBPath("zotio"); err != nil {
		t.Fatalf("DefaultDBPath error = %v", err)
	} else if filepath.Base(path) != "data-group-99999.db" {
		t.Fatalf("DefaultDBPath = %q, want basename data-group-99999.db", path)
	}

	// When no scope is set, the env does apply.
	setActiveGroupID("")
	t.Setenv("ZOTERO_GROUP", "22222")
	if err := ApplyGroupScopeFromEnv(); err != nil {
		t.Fatalf("ApplyGroupScopeFromEnv() error = %v, want nil", err)
	}
	if got := ActiveGroupID(); got != "22222" {
		t.Fatalf("ActiveGroupID() = %q, want %q after unset", got, "22222")
	}

	// Empty env is a no-op and must not clear an active scope.
	setActiveGroupID("33333")
	t.Setenv("ZOTERO_GROUP", "")
	if err := ApplyGroupScopeFromEnv(); err != nil {
		t.Fatalf("ApplyGroupScopeFromEnv() error = %v, want nil", err)
	}
	if got := ActiveGroupID(); got != "33333" {
		t.Fatalf("ActiveGroupID() = %q, want %q (empty env must not clobber)", got, "33333")
	}

	// Malformed env is rejected, not silently applied, and must not
	// clobber whether a scope was already set or not.
	setActiveGroupID("")
	t.Setenv("ZOTERO_GROUP", "team-alpha")
	if err := ApplyGroupScopeFromEnv(); err == nil {
		t.Fatal("ApplyGroupScopeFromEnv() error = nil, want rejection for non-numeric ZOTERO_GROUP")
	}
	if got := ActiveGroupID(); got != "" {
		t.Fatalf("ActiveGroupID() = %q, want %q after rejected env (empty pre-state)", got, "")
	}
	setActiveGroupID("44444")
	t.Setenv("ZOTERO_GROUP", "bad!")
	if err := ApplyGroupScopeFromEnv(); err == nil {
		t.Fatal("ApplyGroupScopeFromEnv() error = nil, want rejection for non-numeric ZOTERO_GROUP")
	}
	if got := ActiveGroupID(); got != "44444" {
		t.Fatalf("ActiveGroupID() = %q, want %q after rejected env (must preserve prior scope)", got, "44444")
	}
}

// TestActiveGroupIDIsRaceFreeAgainstConcurrentGroupScopeWrites is the
// cli-package companion to the mcp regression test. It drives the same
// reader/writer pair — WRITER: setActiveGroupID (called by
// PersistentPreRunE and ApplyGroupScopeFromEnv) vs READER:
// ActiveGroupID/activeGroupIDLocked and DefaultDBPath — concurrently and
// asserts race-freedom plus the DB-path invariant. Placing the test here
// lets it reach the unexported setActiveGroupID directly. The writers cycle
// only between non-empty scopes so defaultDBPath's two-read sequence
// (check then use, each under its own RLock) cannot observe the
// intermediate "data-group-.db" that a concurrent write through "" would
// produce even when each individual access is correctly locked; the
// personal-library value "" remains an allowed initial state.
func TestActiveGroupIDIsRaceFreeAgainstConcurrentGroupScopeWrites(t *testing.T) {
	restore := SnapshotGlobals()
	defer restore()

	dataDir := t.TempDir()
	t.Setenv("ZOTERO_DATA_DIR", dataDir)
	t.Setenv("ZOTERO_GROUP", "")
	t.Setenv("ZOTIO_DEMO", "")
	t.Setenv("HOME", t.TempDir())

	allowed := map[string]bool{
		filepath.Join(dataDir, "data.db"):             true,
		filepath.Join(dataDir, "data-group-11111.db"): true,
		filepath.Join(dataDir, "data-group-22222.db"): true,
	}

	const iterations = 800

	var wg sync.WaitGroup
	errCh := make(chan string, 64)

	// Writer 1: direct setActiveGroupID cycling, the narrowest writer.
	// Alternate only between numeric scopes; "" is never a concurrent
	// writer target (see doc comment above).
	wg.Add(1)
	go func() {
		defer wg.Done()
		ids := []string{"11111", "22222"}
		for range iterations {
			for _, id := range ids {
				setActiveGroupID(id)
			}
		}
	}()

	// Writer 2: ApplyGroupScopeFromEnv's check-then-set under a single
	// write lock — exercises the "must not clobber" interleaving that the
	// no-clobber unit test covers sequentially. Also avoids "" as a
	// steady-state value by resetting to a numeric scope.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range iterations {
			t.Setenv("ZOTERO_GROUP", "11111")
			_ = ApplyGroupScopeFromEnv()
			setActiveGroupID("22222")
			t.Setenv("ZOTERO_GROUP", "22222")
			_ = ApplyGroupScopeFromEnv()
			setActiveGroupID("11111")
		}
	}()

	const readers = 4
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				id := ActiveGroupID()
				if id != "" && id != "11111" && id != "22222" {
					select {
					case errCh <- "ActiveGroupID()=" + id + " not in allowed set":
					default:
					}
				}
				p, err := DefaultDBPath("zotio")
				if err != nil {
					select {
					case errCh <- "DefaultDBPath error: " + err.Error():
					default:
					}
					continue
				}
				if !allowed[p] {
					select {
					case errCh <- "DefaultDBPath=" + p + " not in allowed set":
					default:
					}
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatalf("race invariant violated: %s", msg)
	}
}
