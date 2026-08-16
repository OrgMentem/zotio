// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestApplyGroupScopeFromEnvDoesNotClobberActiveScope guards the
// check-then-set ordering inside ApplyGroupScopeFromEnv: when a scope is
// already established (by --group / PersistentPreRunE), the ZOTERO_GROUP
// fallback must not overwrite it. The check and the assignment share one
// write lock (activeGroupMu) so they cannot interleave with a concurrent
// setActiveGroupID from cobra.
//
// Baseline (commit 6c87ef3) returned nil when active scope was already set,
// regardless of ZOTERO_GROUP validity. The buggy version validated env first
// and returned an error even with an active scope — a semantic change not
// authorized by the race fix.
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

	// Malformed env is rejected when no scope is active.
	setActiveGroupID("")
	t.Setenv("ZOTERO_GROUP", "team-alpha")
	if err := ApplyGroupScopeFromEnv(); err == nil {
		t.Fatal("ApplyGroupScopeFromEnv() error = nil, want rejection for non-numeric ZOTERO_GROUP")
	}
	if got := ActiveGroupID(); got != "" {
		t.Fatalf("ActiveGroupID() = %q, want %q after rejected env (empty pre-state)", got, "")
	}
	// Baseline contract: when a scope is already active and ZOTERO_GROUP is
	// malformed, the call is a no-op and returns nil — the active scope
	// takes precedence and the malformed env is never validated.
	// Changed assertion (was: expect error with active scope "44444"):
	//   old: if err := ApplyGroupScopeFromEnv(); err == nil { t.Fatal(...) }
	//   new: expect nil — baseline returned nil when activeGroupID != ""
	setActiveGroupID("44444")
	t.Setenv("ZOTERO_GROUP", "bad!")
	if err := ApplyGroupScopeFromEnv(); err != nil {
		t.Fatalf("ApplyGroupScopeFromEnv() error = %v, want nil (baseline: active scope suppresses env validation)", err)
	}
	if got := ActiveGroupID(); got != "44444" {
		t.Fatalf("ActiveGroupID() = %q, want %q after malformed env with active scope (must preserve prior scope)", got, "44444")
	}
}

// TestActiveGroupIDIsRaceFreeAgainstConcurrentGroupScopeWrites drives writers
// and readers concurrently to expose torn composite reads. Writers cycle
// through the empty scope as well so torn values like data-group-.db,
// group:, journal-group-, groups/, and cross-scope hybrids are structurally
// observable. Every observed value must be exactly a complete personal or
// group form.
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
	errCh := make(chan string, 256)

	// Writer 1: direct setActiveGroupID cycling including empty scope.
	wg.Add(1)
	go func() {
		defer wg.Done()
		ids := []string{"", "11111", "22222"}
		for range iterations {
			for _, id := range ids {
				setActiveGroupID(id)
			}
		}
	}()

	// Writer 2: ApplyGroupScopeFromEnv interleaving.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range iterations {
			setActiveGroupID("")
			t.Setenv("ZOTERO_GROUP", "11111")
			_ = ApplyGroupScopeFromEnv()
			setActiveGroupID("22222")
			setActiveGroupID("")
			t.Setenv("ZOTERO_GROUP", "22222")
			_ = ApplyGroupScopeFromEnv()
			setActiveGroupID("11111")
		}
	}()

	isAllowed := func(s string, allowedSet map[string]bool) bool {
		return allowedSet[s]
	}

	isTorn := func(s string) bool {
		if s == "data-group-.db" || s == "group:" || s == "journal-group-" || s == "groups/" {
			return true
		}
		if strings.Contains(s, "data-group-.db") {
			return true
		}
		return false
	}

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
				if isTorn(id) {
					select {
					case errCh <- "ActiveGroupID()=" + id + " is torn value":
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
				if isTorn(p) {
					select {
					case errCh <- "DefaultDBPath=" + p + " is torn value":
					default:
					}
				}
				if !isAllowed(p, allowed) {
					select {
					case errCh <- "DefaultDBPath=" + p + " not in allowed set":
					default:
					}
				}

				jd, err := journalDir()
				if err != nil {
					select {
					case errCh <- "journalDir error: " + err.Error():
					default:
					}
					continue
				}
				jBase := filepath.Base(jd)
				if jBase != "journal" && jBase != "journal-group-11111" && jBase != "journal-group-22222" {
					select {
					case errCh <- "journalDir base=" + jBase + " not in allowed set; full=" + jd:
					default:
					}
				}
				if isTorn(jBase) || jBase == "journal-group-" {
					select {
					case errCh <- "journalDir base=" + jBase + " is torn value":
					default:
					}
				}

				lib := currentJournalLibrary()
				if lib != "user" && lib != "group:11111" && lib != "group:22222" {
					select {
					case errCh <- "currentJournalLibrary()=" + lib + " not in allowed set":
					default:
					}
				}
				if lib == "group:" {
					select {
					case errCh <- "currentJournalLibrary()=group: is torn value":
					default:
					}
				}

				seg := zoteroLibrarySegment()
				if seg != "library" && seg != "groups/11111" && seg != "groups/22222" {
					select {
					case errCh <- "zoteroLibrarySegment()=" + seg + " not in allowed set":
					default:
					}
				}
				if seg == "groups/" {
					select {
					case errCh <- "zoteroLibrarySegment()=groups/ is torn value":
					default:
					}
				}

				vid := vaultLibraryID(nil)
				if vid != "" && vid != "groups/11111" && vid != "groups/22222" {
					if !strings.HasPrefix(vid, "users/") {
						select {
						case errCh <- "vaultLibraryID()=" + vid + " not in allowed set":
						default:
						}
					}
				}
				if vid == "groups/" {
					select {
					case errCh <- "vaultLibraryID()=groups/ is torn value":
					default:
					}
				}

				uri, _, libScope, err := zoteroDeepLink("item", "ABC123")
				if err != nil {
					select {
					case errCh <- "zoteroDeepLink error: " + err.Error():
					default:
					}
					continue
				}
				if libScope != "personal" && libScope != "group:11111" && libScope != "group:22222" {
					select {
					case errCh <- "zoteroDeepLink libScope=" + libScope + " not in allowed set":
					default:
					}
				}
				if libScope == "group:" {
					select {
					case errCh <- "zoteroDeepLink libScope=group: is torn value":
					default:
					}
				}
				expectedSeg := "library"
				if strings.HasPrefix(libScope, "group:") {
					expectedSeg = "groups/" + strings.TrimPrefix(libScope, "group:")
				}
				if !strings.Contains(uri, expectedSeg) {
					select {
					case errCh <- "zoteroDeepLink mismatch: libScope=" + libScope + " uri=" + uri + " expected segment " + expectedSeg:
					default:
					}
				}
				if strings.Contains(uri, "groups/") && libScope == "personal" {
					select {
					case errCh <- "zoteroDeepLink mismatch: personal scope but uri has groups/: " + uri:
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

// TestApplyGroupScopeFromEnvConcurrentNoClobber asserts the final scope under
// concurrent ApplyGroupScopeFromEnv and setActiveGroupID is deterministic:
// the explicit setter wins regardless of ordering. Resets scope each iteration.
func TestApplyGroupScopeFromEnvConcurrentNoClobber(t *testing.T) {
	restore := SnapshotGlobals()
	defer restore()

	t.Setenv("ZOTERO_GROUP", "11111")
	t.Setenv("ZOTIO_DEMO", "")
	t.Setenv("ZOTERO_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	const iterations = 300
	for i := 0; i < iterations; i++ {
		setActiveGroupID("")

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = ApplyGroupScopeFromEnv()
		}()
		go func() {
			defer wg.Done()
			setActiveGroupID("22222")
		}()
		wg.Wait()

		if got := ActiveGroupID(); got != "22222" {
			t.Fatalf("iteration %d: ActiveGroupID()=%q, want 22222 (explicit setter must win regardless of ordering)", i, got)
		}
	}
}
