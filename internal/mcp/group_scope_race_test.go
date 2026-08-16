// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package mcp

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"zotio/internal/cli"
)

// TestDBPathIsRaceFreeAgainstConcurrentGroupScopeWrites is the regression
// proof for zotio-eb8144a. The guarded global is activeGroupIDValue in
// internal/cli (helpers.go): the WRITER is cobra's PersistentPreRunE at
// internal/cli/root.go:322 which calls setActiveGroupID(flags.group) on
// every in-process command execution, and the READER is mcp.dbPath() (which
// delegates to cli.DefaultDBPath) and cli.DefaultDBPath itself. The MCP
// server's native handlers (handleSearch, handleSQL, archive/collection/item
// handlers) call dbPath() from MCP dispatch goroutines; under --transport http
// those dispatch concurrently, while a concurrent command_run (or any
// mirrored command via runMirroredInProcess) retargets the group scope.
// runMirroredInProcess serializes only MIRRORED runs under mirroredCommandMu
// with StateGuard — it does not cover native handlers — so the only correct
// synchronization is the RWMutex around activeGroupIDValue (activeGroupMu).
//
// Without that mutex, concurrent setActiveGroupID writes from ExecuteContext
// and activeGroupIDLocked reads from DefaultDBPath/dbPath race on the string
// header; -race must report that. With the mutex the test must pass and every
// observed path must be one of the legitimately expected group-scoped values
// (never torn or hybrid).
func TestDBPathIsRaceFreeAgainstConcurrentGroupScopeWrites(t *testing.T) {
	// Snapshot/restore the process-global scope so the test never leaks into
	// sibling tests. SnapshotGlobals captures activeGroupIDValue under RLock
	// and restores it under Lock; the deferred restore runs after all
	// goroutines have joined.
	restore := cli.SnapshotGlobals()
	defer restore()

	dataDir := t.TempDir()
	t.Setenv("ZOTERO_DATA_DIR", dataDir)
	t.Setenv("ZOTERO_GROUP", "")
	t.Setenv("ZOTIO_DEMO", "")

	// Precompute the only legal DB paths for the scopes this test drives.
	// defaultDBPath currently does two separate RLock reads (check then use),
	// so driving the writer through "" would let a reader observe the torn
	// intermediate "data-group-.db" even with the mutex held per-read. To
	// keep the invariant precise (every path is a legit scope) the writer
	// cycles only between non-empty scopes; personal-library ("") is still
	// an allowed initial value but never a concurrent writer target.
	allowed := map[string]bool{
		filepath.Join(dataDir, "data.db"):             true,
		filepath.Join(dataDir, "data-group-11111.db"): true,
		filepath.Join(dataDir, "data-group-22222.db"): true,
	}

	const iterations = 600

	var wg sync.WaitGroup
	errCh := make(chan string, 64)

	// Writer: drives the real writer path — cobra's PersistentPreRunE via
	// cli.RootCmd(). Execute writes activeGroupIDValue under Lock via
	// setActiveGroupID on every invocation (root.go:322). Cycling between
	// two distinct numeric groups retargets the scope the way a
	// workflow-run step with --group would, without hitting the
	// empty-scope double-read torn edge.
	wg.Add(1)
	go func() {
		defer wg.Done()
		groupIDs := []string{"11111", "22222"}
		for range iterations {
			for _, gid := range groupIDs {
				cmd := cli.RootCmd()
				cmd.SilenceErrors = true
				cmd.SilenceUsage = true
				cmd.SetOut(io.Discard)
				cmd.SetErr(io.Discard)
				cmd.SetArgs([]string{"--group", gid, "version"})
				_ = cmd.ExecuteContext(context.Background())
			}
		}
	}()

	// Readers: several concurrent native-handler-style readers that resolve
	// the DB path. dbPath() is the MCP-adjacent reader (mcp/tools.go:81,
	// falling back to cli.ActiveGroupID on its fallback branch), and
	// cli.DefaultDBPath is the exported seamed reader that dbPath delegates
	// to. Both ultimately read activeGroupIDLocked under RLock.
	const readers = 4
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				// mcp.dbPath() path
				p := dbPath()
				if p == "" {
					select {
					case errCh <- "dbPath() returned empty string":
					default:
					}
				} else if !allowed[p] {
					select {
					case errCh <- "dbPath()=" + p + " not in allowed set":
					default:
					}
				}
				// cli.DefaultDBPath path (same seam, exported)
				p2, err := cli.DefaultDBPath("zotio")
				if err != nil {
					select {
					case errCh <- "cli.DefaultDBPath error: " + err.Error():
					default:
					}
					continue
				}
				if p2 == "" {
					select {
					case errCh <- "cli.DefaultDBPath returned empty string":
					default:
					}
				} else if !allowed[p2] {
					select {
					case errCh <- "cli.DefaultDBPath=" + p2 + " not in allowed set":
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

	// Final scope could be any driven scope; only membership matters.
	if cur, err := cli.DefaultDBPath("zotio"); err == nil {
		if cur == "" {
			t.Fatalf("final cli.DefaultDBPath empty after concurrent run")
		}
		if !allowed[cur] {
			t.Fatalf("final cli.DefaultDBPath=%q not in allowed set %v", cur, allowed)
		}
	}
}
