// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package desktop

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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
	// fail is the error while down; nil means a generic failure.
	fail error
}

func (f *fakeConnector) ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.up {
		return nil
	}
	if f.fail != nil {
		return f.fail
	}
	return errors.New("dial tcp 127.0.0.1:23119: connect: connection refused")
}

// errNoAnswer is how a ping to a connector that accepts the connection and
// never answers fails.
var errNoAnswer = fmt.Errorf("connector ping: %w", &url.Error{Op: "Get", URL: "http://127.0.0.1:23119/connector/ping", Err: context.DeadlineExceeded})

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
		o.ConfirmFirst, o.ConfirmMax = 10*time.Millisecond, 40*time.Millisecond
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

// stallTuning shrinks the sustained-stall evidence to test scale.
func stallTuning(o *WaitOptions) {
	o.StallSpan, o.StallChecks = 300*time.Millisecond, 3
	o.StallRecheck, o.StallPingTimeout = 50*time.Millisecond, 20*time.Millisecond
}

// The live case: Zotero starts, takes its lock, and hangs before its
// connector answers. After the startup window it is busy; once the silence
// has lasted the stall span over enough checks, Wait returns the
// unresponsive status rather than wait silently, since no filesystem change
// would ever announce a recovery.
func TestWaitReturnsUnresponsiveOnlyAfterASustainedStall(t *testing.T) {
	in := newInstall(t)
	conn := &fakeConnector{fail: errNoAnswer}
	done := startWait(t, t.Context(), in, conn, func(o *WaitOptions) {
		o.ConfirmFirst, o.ConfirmMax = 10*time.Millisecond, 40*time.Millisecond
		o.Prober.StartupWindow = 300 * time.Millisecond
		stallTuning(o)
	})

	locked := time.Now()
	startLockHolder(t, in.profile)
	r := awaitResult(t, done)
	if !errors.Is(r.err, ErrStuck) || r.st.State != StateUnresponsive || !r.st.Running {
		t.Fatalf("Wait = %+v, %v; want ErrStuck with the unresponsive status", r.st, r.err)
	}
	// Not before the startup window plus the stall span.
	if elapsed := time.Since(locked); elapsed < 550*time.Millisecond {
		t.Fatalf("Wait gave up %v after the lock; the window (300ms) plus the stall span (300ms) had not passed", elapsed)
	}
	if r.st.StalledSince == nil || r.st.CheckedAt.Sub(*r.st.StalledSince) < 300*time.Millisecond {
		t.Fatalf("stalled_since = %v at %v, want at least the stall span before", r.st.StalledSince, r.st.CheckedAt)
	}
}

// A stall shorter than the span, followed by an answer, is a busy Zotero,
// not a hung one: the wait ends as ready.
func TestWaitRidesOutAStallThatEndsBeforeTheSpan(t *testing.T) {
	in := newInstall(t)
	startLockHolder(t, in.profile)
	conn := &fakeConnector{fail: errNoAnswer}
	done := startWait(t, t.Context(), in, conn, func(o *WaitOptions) {
		o.Prober.StartupWindow = time.Nanosecond
		stallTuning(o)
		o.StallSpan = 2 * time.Second
	})
	time.Sleep(300 * time.Millisecond) // several failed stall checks
	if conn.count() < 3 {
		t.Fatalf("pings during the stall = %d, want re-checks", conn.count())
	}
	conn.setUp()
	if r := awaitResult(t, done); r.err != nil || r.st.State != StateReady {
		t.Fatalf("Wait = %+v, %v; want ready once the stall ends", r.st, r.err)
	}
}

// A connector that is off refuses on every check. That too needs the
// sustained span, because a hung Zotero's listener also refuses some
// connects; once proven, the state names the connector, not a hang.
func TestWaitReturnsConnectorOffAfterASustainedRefusal(t *testing.T) {
	in := newInstall(t)
	startLockHolder(t, in.profile)
	conn := &fakeConnector{fail: realRefusal(t)}
	done := startWait(t, t.Context(), in, conn, func(o *WaitOptions) {
		o.Prober.StartupWindow = time.Nanosecond
		stallTuning(o)
	})
	r := awaitResult(t, done)
	if !errors.Is(r.err, ErrStuck) || r.st.State != StateConnectorOff {
		t.Fatalf("Wait = %+v, %v; want ErrStuck in state connector_off", r.st, r.err)
	}
	if conn.count() < 3 {
		t.Fatalf("pings = %d, want the sustained-stall checks", conn.count())
	}
}

// A stall in which any check found a listener is a hang, even if other
// checks were refused (the intermittent refusals of a hung listener).
func TestWaitCallsAMixedStallUnresponsive(t *testing.T) {
	in := newInstall(t)
	startLockHolder(t, in.profile)
	refused := realRefusal(t)
	var mu sync.Mutex
	n := 0
	ping := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n%2 == 0 {
			return errNoAnswer
		}
		return refused
	}
	prober := &Prober{Install: Install{Profiles: []string{in.profile}, DataDir: in.data}, Ping: ping, StartupWindow: time.Nanosecond}
	opts := WaitOptions{Prober: prober, WatchDirs: []string{in.profile, in.data}}
	stallTuning(&opts)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	st, err := Wait(ctx, opts)
	if !errors.Is(err, ErrStuck) || st.State != StateUnresponsive {
		t.Fatalf("Wait = %+v, %v; want ErrStuck in state unresponsive", st, err)
	}
}

// Busy re-checks are timer-driven; a sync writing its WAL constantly must
// not turn every filesystem event into another ping of a busy Zotero.
func TestWaitIgnoresEventsWhileBusy(t *testing.T) {
	in := newInstall(t)
	startLockHolder(t, in.profile)
	conn := &fakeConnector{fail: errNoAnswer}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := startWait(t, ctx, in, conn, func(o *WaitOptions) {
		o.Prober.StartupWindow = time.Nanosecond
		o.StallSpan, o.StallRecheck = time.Hour, time.Hour
	})
	for i := range 20 {
		if err := os.WriteFile(filepath.Join(in.data, "zotero.sqlite-wal"), []byte{byte(i)}, 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	assertStillWaiting(t, done, 200*time.Millisecond)
	if got := conn.count(); got != 1 {
		t.Fatalf("pings while busy with WAL events = %d, want only the first", got)
	}
	cancel()
	awaitResult(t, done)
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
