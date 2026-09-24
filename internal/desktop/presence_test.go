// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"zotio/internal/zoteroprefs"
)

func TestProbeLockAbsentFileIsNotCreated(t *testing.T) {
	dir := t.TempDir()
	if got := ProbeLock(dir); got.State != LockAbsent || got.Err != nil {
		t.Fatalf("ProbeLock(empty dir) = %+v, want absent", got)
	}
	if _, err := os.Stat(filepath.Join(dir, lockFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the probe created %s (stat err %v); it must never create the file Zotero owns", lockFileName, err)
	}
}

// The production sequence: Zotero takes the lock and holds it while it runs,
// then releases it on quit. The probe must see all three states, and on
// macOS and Linux name the holder.
func TestProbeLockFollowsAHolderProcessThroughStartAndQuit(t *testing.T) {
	dir := t.TempDir()
	h := startLockHolder(t, dir)

	got := ProbeLock(dir)
	if got.State != LockHeld {
		t.Fatalf("ProbeLock while a child holds the lock = %+v, want held", got)
	}
	if runtime.GOOS != "windows" && got.PID != h.pid {
		t.Fatalf("lock holder PID = %d, want the child's %d", got.PID, h.pid)
	}

	h.release(t)
	got = ProbeLock(dir)
	want := LockFree
	if runtime.GOOS == "windows" {
		want = LockAbsent // delete-on-close removes parent.lock with the last handle
	}
	if got.State != want || got.Err != nil {
		t.Fatalf("ProbeLock after the holder quit = %+v, want %s", got, want)
	}
}

// A probe that raised filesystem events would wake its own watcher, and the
// waiter would re-probe forever: polling by feedback loop.
func TestProbeLockRaisesNoFilesystemEvents(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, lockFileName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if err := w.Add(dir); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if got := ProbeLock(dir); got.State != LockFree {
			t.Fatalf("ProbeLock = %+v, want free", got)
		}
	}
	select {
	case ev := <-w.Events:
		t.Fatalf("probing the lock raised %v", ev)
	case err := <-w.Errors:
		t.Fatalf("watcher error: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestProbeStatesFollowTheTwoSignals(t *testing.T) {
	refused := func(context.Context) error { return errors.New("connection refused") }
	answers := func(context.Context) error { return nil }

	t.Run("stopped", func(t *testing.T) {
		p := &Prober{Install: Install{Profiles: []string{t.TempDir()}}, Ping: refused}
		st := p.Probe(t.Context())
		if st.Running || st.ConnectorReachable || st.State != StateStopped || st.Evidence != EvidenceNone {
			t.Fatalf("status = %+v, want stopped with no evidence", st)
		}
		if st.ConnectorError != "connection refused" {
			t.Fatalf("connector_error = %q, want the ping error", st.ConnectorError)
		}
	})

	// Process up, connector not yet listening: the transition window.
	t.Run("starting", func(t *testing.T) {
		dir := t.TempDir()
		startLockHolder(t, dir)
		p := &Prober{Install: Install{Profiles: []string{dir}}, Ping: refused}
		st := p.Probe(t.Context())
		if !st.Running || st.ConnectorReachable || st.State != StateStarting || st.Evidence != EvidenceProfileLock {
			t.Fatalf("status = %+v, want running, starting, profile_lock evidence", st)
		}
		if len(st.Profiles) != 1 || st.Profiles[0].Lock != LockHeld {
			t.Fatalf("profiles = %+v, want the one profile held", st.Profiles)
		}
	})

	// The connector is the stronger signal: it answers even when discovery
	// found no profile (a pinned or unusual install).
	t.Run("ready without a profile", func(t *testing.T) {
		p := &Prober{Ping: answers, ConnectorURL: "http://127.0.0.1:23119/connector"}
		st := p.Probe(t.Context())
		if !st.Running || !st.ConnectorReachable || st.State != StateReady || st.Evidence != EvidenceConnector {
			t.Fatalf("status = %+v, want ready on connector evidence", st)
		}
		if st.ConnectorError != "" || st.Profiles == nil {
			t.Fatalf("status = %+v, want no connector_error and an empty (not null) profiles list", st)
		}
	})

	t.Run("no addressable connector", func(t *testing.T) {
		cause := errors.New("the desktop connector is only available with a local Zotero base URL")
		p := &Prober{ConnectorErr: cause}
		st := p.Probe(t.Context())
		if st.ConnectorReachable || st.ConnectorError != cause.Error() {
			t.Fatalf("status = %+v, want the connector cause reported", st)
		}
	})

	// A connector that accepts the connection and never answers must not
	// hang the probe past its bound.
	t.Run("ping bounded", func(t *testing.T) {
		hang := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
		p := &Prober{Ping: hang, PingTimeout: 50 * time.Millisecond}
		start := time.Now()
		st := p.Probe(t.Context())
		if st.ConnectorReachable || time.Since(start) > 2*time.Second {
			t.Fatalf("status = %+v after %v, want an unreachable connector within the ping bound", st, time.Since(start))
		}
	})
}

func TestDiscoverUsesThePinnedProfileAndItsDataDirectory(t *testing.T) {
	profile := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "ZoteroData")
	prefs := `user_pref("extensions.zotero.useDataDir", true);` + "\n" +
		`user_pref("extensions.zotero.dataDir", "` + filepath.ToSlash(dataDir) + `");` + "\n"
	if err := os.WriteFile(filepath.Join(profile, "prefs.js"), []byte(prefs), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(zoteroprefs.ProfileDirEnv, profile)

	in := Discover()
	if in.Err != nil {
		t.Fatalf("Discover: %v", in.Err)
	}
	if len(in.Profiles) != 1 || in.Profiles[0] != profile {
		t.Fatalf("profiles = %v, want [%s]", in.Profiles, profile)
	}
	if in.DataDir != dataDir {
		t.Fatalf("data dir = %q, want %q", in.DataDir, dataDir)
	}
	if got := in.WatchDirs(); len(got) != 2 || got[0] != profile || got[1] != in.DataDir {
		t.Fatalf("watch dirs = %v, want the profile then the data dir", got)
	}
}

func TestDiscoverReportsABadPinInsteadOfGuessing(t *testing.T) {
	t.Setenv(zoteroprefs.ProfileDirEnv, filepath.Join(t.TempDir(), "missing"))
	if in := Discover(); in.Err == nil || len(in.Profiles) != 0 {
		t.Fatalf("Discover with a pin at a missing directory = %+v, want an error and no profiles", in)
	}
}
