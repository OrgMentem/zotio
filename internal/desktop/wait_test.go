// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeConnector stands in for Zotero's /connector/ping and counts every
// probe, which is how these tests tell event-driven waiting from polling.
type fakeConnector struct {
	mu    sync.Mutex
	up    bool
	calls int
}

func (f *fakeConnector) ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.up {
		return nil
	}
	return errors.New("dial tcp 127.0.0.1:23119: connect: connection refused")
}

func (f *fakeConnector) setUp() {
	f.mu.Lock()
	f.up = true
	f.mu.Unlock()
}

func (f *fakeConnector) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// install is a closed Zotero: a profile directory and a data directory.
type install struct {
	profile, data string
}

func newInstall(t *testing.T) install {
	t.Helper()
	root := t.TempDir()
	in := install{profile: filepath.Join(root, "Profiles", "abcd1234.default"), data: filepath.Join(root, "Zotero")}
	for _, dir := range []string{in.profile, in.data} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return in
}

type waitResult struct {
	st  Status
	err error
}

// startWait runs Wait in the background and returns once its watches are
// installed, so a later filesystem change is guaranteed to be seen.
func startWait(t *testing.T, ctx context.Context, in install, conn *fakeConnector, tune func(*WaitOptions)) <-chan waitResult {
	t.Helper()
	watching := make(chan struct{})
	opts := WaitOptions{
		Prober:     &Prober{Install: Install{Profiles: []string{in.profile}, DataDir: in.data}, Ping: conn.ping},
		WatchDirs:  []string{in.profile, in.data},
		Settle:     20 * time.Millisecond,
		OnWatching: func() { close(watching) },
	}
	if tune != nil {
		tune(&opts)
	}
	done := make(chan waitResult, 1)
	go func() {
		st, err := Wait(ctx, opts)
		done <- waitResult{st, err}
	}()
	select {
	case <-watching:
	case r := <-done:
		t.Fatalf("Wait returned before watching: %+v, %v", r.st, r.err)
	case <-time.After(10 * time.Second):
		t.Fatal("Wait never installed its watches")
	}
	return done
}

func awaitResult(t *testing.T, done <-chan waitResult) waitResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return")
		return waitResult{}
	}
}

func assertStillWaiting(t *testing.T, done <-chan waitResult, d time.Duration) {
	t.Helper()
	select {
	case r := <-done:
		t.Fatalf("Wait returned early: %+v, %v", r.st, r.err)
	case <-time.After(d):
	}
}

func TestWaitReturnsAtOnceWhenTheConnectorAlreadyAnswers(t *testing.T) {
	conn := &fakeConnector{up: true}
	// No profile and nothing to watch: an answering connector needs neither.
	st, err := Wait(t.Context(), WaitOptions{Prober: &Prober{Ping: conn.ping}})
	if err != nil || !st.ConnectorReachable || st.State != StateReady {
		t.Fatalf("Wait = %+v, %v; want ready at once", st, err)
	}
	if conn.count() != 1 {
		t.Fatalf("pings = %d, want exactly 1", conn.count())
	}
}

func TestWaitWithNoProfileAndNoConnectorFailsWithErrNoProfile(t *testing.T) {
	conn := &fakeConnector{}
	st, err := Wait(t.Context(), WaitOptions{Prober: &Prober{Ping: conn.ping}})
	if !errors.Is(err, ErrNoProfile) || st.Running {
		t.Fatalf("Wait = %+v, %v; want ErrNoProfile", st, err)
	}
}

// While Zotero is closed nothing may run on a timer: after the first probe
// the connector is not asked again until the filesystem changes.
func TestWaitSleepsWhileClosedAndWakesOnAFilesystemEvent(t *testing.T) {
	in := newInstall(t)
	conn := &fakeConnector{}
	done := startWait(t, t.Context(), in, conn, nil)

	assertStillWaiting(t, done, 400*time.Millisecond)
	if got := conn.count(); got != 1 {
		t.Fatalf("pings while Zotero is closed = %d, want 1 (no polling)", got)
	}

	conn.setUp()
	// Zotero opening its database creates the WAL file in the data dir.
	if err := os.WriteFile(filepath.Join(in.data, "zotero.sqlite-wal"), []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := awaitResult(t, done)
	if r.err != nil || r.st.State != StateReady {
		t.Fatalf("Wait = %+v, %v; want ready after the event", r.st, r.err)
	}
}

// The measured Zotero start: the profile lock is taken first, the connector
// listens seconds later, and starting the connector writes no file. Wait
// must bridge that gap on its own without a further event.
func TestWaitConfirmsTheConnectorAfterTheLockWithoutAFurtherEvent(t *testing.T) {
	in := newInstall(t)
	conn := &fakeConnector{}
	done := startWait(t, t.Context(), in, conn, func(o *WaitOptions) {
		o.ConfirmFirst, o.ConfirmMax, o.ConfirmWindow = 10*time.Millisecond, 40*time.Millisecond, 10*time.Second
	})

	h := startLockHolder(t, in.profile) // Zotero takes its profile lock
	deadline := time.Now().Add(10 * time.Second)
	for conn.count() < 4 { // the lock event's probe plus confirm re-checks
		if time.Now().After(deadline) {
			t.Fatalf("pings after the lock = %d, want confirm re-checks", conn.count())
		}
		time.Sleep(5 * time.Millisecond)
	}
	conn.setUp() // the connector starts listening; no file changes

	r := awaitResult(t, done)
	if r.err != nil || r.st.State != StateReady || !r.st.LockHeld() {
		t.Fatalf("Wait = %+v, %v; want ready with the lock held", r.st, r.err)
	}
	h.release(t)
}

// Zotero running with its connector disabled keeps the lock held and never
// answers. The confirm re-checks must stop when the window closes; after
// that only a filesystem event may cause a probe.
func TestWaitStopsReCheckingWhenTheConfirmWindowCloses(t *testing.T) {
	in := newInstall(t)
	startLockHolder(t, in.profile)
	conn := &fakeConnector{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := startWait(t, ctx, in, conn, func(o *WaitOptions) {
		o.ConfirmFirst, o.ConfirmMax, o.ConfirmWindow = 10*time.Millisecond, 20*time.Millisecond, 150*time.Millisecond
	})

	time.Sleep(400 * time.Millisecond) // well past the window
	settled := conn.count()
	if settled < 3 {
		t.Fatalf("pings during the confirm window = %d, want re-checks", settled)
	}
	assertStillWaiting(t, done, 400*time.Millisecond)
	if got := conn.count(); got != settled {
		t.Fatalf("pings after the confirm window = %d, then %d; want no polling", settled, got)
	}

	if err := os.WriteFile(filepath.Join(in.data, "zotero.sqlite-wal"), []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for conn.count() == settled {
		if time.Now().After(deadline) {
			t.Fatal("a filesystem event after the window caused no probe")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if r := awaitResult(t, done); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("Wait after cancel = %+v, %v; want context.Canceled", r.st, r.err)
	}
}

func TestWaitTimeoutReturnsItsCause(t *testing.T) {
	in := newInstall(t)
	conn := &fakeConnector{}
	errTimeout := errors.New("wait timed out")
	ctx, cancel := context.WithTimeoutCause(t.Context(), 150*time.Millisecond, errTimeout)
	defer cancel()
	done := startWait(t, ctx, in, conn, nil)

	r := awaitResult(t, done)
	if !errors.Is(r.err, errTimeout) {
		t.Fatalf("Wait = %+v, %v; want the timeout cause", r.st, r.err)
	}
	if r.st.State != StateStopped || r.st.CheckedAt.IsZero() {
		t.Fatalf("status at timeout = %+v, want the last (stopped) probe", r.st)
	}
}

func TestWaitCancellationReturnsPromptly(t *testing.T) {
	in := newInstall(t)
	conn := &fakeConnector{}
	ctx, cancel := context.WithCancel(t.Context())
	done := startWait(t, ctx, in, conn, nil)

	start := time.Now()
	cancel()
	r := awaitResult(t, done)
	if !errors.Is(r.err, context.Canceled) {
		t.Fatalf("Wait = %+v, %v; want context.Canceled", r.st, r.err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Wait took %v to honour cancellation", elapsed)
	}
}

// A data directory that does not exist (Zotero not yet run, or moved) must
// not stop the wait while the profile directory can still be watched.
func TestWaitWatchesWhatExists(t *testing.T) {
	in := newInstall(t)
	if err := os.Remove(in.data); err != nil {
		t.Fatal(err)
	}
	conn := &fakeConnector{}
	done := startWait(t, t.Context(), in, conn, nil)
	conn.setUp()
	startLockHolder(t, in.profile)
	if r := awaitResult(t, done); r.err != nil || r.st.State != StateReady {
		t.Fatalf("Wait = %+v, %v; want ready from a profile-directory event", r.st, r.err)
	}
}
