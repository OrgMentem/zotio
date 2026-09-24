// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package desktop

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"zotio/internal/connector"
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
	noAnswer := func(context.Context) error {
		return fmt.Errorf("connector ping: %w", &url.Error{Op: "Get", URL: "http://127.0.0.1:23119/connector/ping", Err: context.DeadlineExceeded})
	}
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
		info, err := os.Stat(filepath.Join(dir, lockFileName))
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Profiles[0].LockSince; got == nil || !got.Equal(info.ModTime().UTC()) {
			t.Fatalf("lock_since = %v, want the lock file mtime %v", got, info.ModTime().UTC())
		}
	})

	// The live case, 2026-09-24: Zotero up for well over the startup window,
	// its connector port accepting connections and never answering.
	// One silent check past the window is not evidence of a hang: a single
	// probe reports busy, never unresponsive (only Wait can see a stall).
	t.Run("busy past the startup window", func(t *testing.T) {
		dir := t.TempDir()
		startLockHolder(t, dir)
		for _, startup := range []time.Duration{time.Hour, time.Nanosecond} {
			p := &Prober{Install: Install{Profiles: []string{dir}}, Ping: noAnswer, StartupWindow: startup}
			st := p.Probe(t.Context())
			want := StateBusy
			if startup == time.Hour {
				want = StateStarting // the same silence inside the window is a start
			}
			if !st.Running || st.State != want || st.Evidence != EvidenceProfileLock {
				t.Fatalf("startup window %v: status = %+v, want running, %s", startup, st, want)
			}
		}
	})

	t.Run("connector_off past the startup window", func(t *testing.T) {
		dir := t.TempDir()
		startLockHolder(t, dir)
		refusedDial := func(context.Context) error { return realRefusal(t) }
		p := &Prober{Install: Install{Profiles: []string{dir}}, Ping: refusedDial, StartupWindow: time.Nanosecond}
		if st := p.Probe(t.Context()); !st.Running || st.State != StateConnectorOff {
			t.Fatalf("status = %+v, want running, connector_off", st)
		}
	})

	// A closed Zotero is stopped however old its lock file is.
	t.Run("stopped ignores the window", func(t *testing.T) {
		p := &Prober{Install: Install{Profiles: []string{t.TempDir()}}, Ping: noAnswer, StartupWindow: time.Nanosecond}
		if st := p.Probe(t.Context()); st.Running || st.State != StateStopped {
			t.Fatalf("status = %+v, want stopped", st)
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

// realRefusal dials a port nothing listens on, as a ping to a closed or
// connector-disabled Zotero does.
func realRefusal(t *testing.T) error {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	conn := connector.New("http://"+addr+"/connector", time.Second)
	err = conn.Ping(t.Context())
	if err == nil {
		t.Fatal("ping to a closed port succeeded")
	}
	return err
}

// The unresponsive/connector_off split rests on telling "nothing listens"
// from "something holds the port and does not answer", so it is pinned
// against the errors the real connector client returns.
func TestConnectorRefusedClassifiesRealPingErrors(t *testing.T) {
	if !connectorRefused(realRefusal(t)) {
		t.Fatal("a refused dial did not classify as refused")
	}

	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // accept the connection, never answer: a blocked main thread
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer hung.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := connector.New(hung.URL+"/connector", time.Minute).Ping(ctx)
	if err == nil || connectorRefused(err) {
		t.Fatalf("hung connector ping error = %v; want a failure that is not a refusal", err)
	}
	err = connector.New(hung.URL+"/connector", 100*time.Millisecond).Ping(t.Context())
	if err == nil || connectorRefused(err) {
		t.Fatalf("client-timeout ping error = %v; want a failure that is not a refusal", err)
	}

	wrong := httptest.NewServer(http.NotFoundHandler())
	defer wrong.Close()
	err = connector.New(wrong.URL+"/connector", time.Second).Ping(t.Context())
	if err == nil || connectorRefused(err) {
		t.Fatalf("404 ping error = %v; want a failure that is not a refusal", err)
	}
}

// Zotero's connector runs on its main thread, which a large sync can hold for
// seconds. A connector that stalls for 10s and then answers must end the wait
// as ready, never as unresponsive, under the production stall evidence
// (60s span, 3 checks, 10s confirming pings). Only the re-check cadence is
// shortened, so the test takes about 10s rather than 20s.
func TestWaitTreatsATenSecondStallAsBusyNotUnresponsive(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a real 10s connector stall")
	}
	dir := t.TempDir()
	startLockHolder(t, dir)
	stallUntil := time.Now().Add(10 * time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Until(stallUntil)):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	conn := connector.New(srv.URL+"/connector", time.Minute)
	prober := &Prober{
		Install:       Install{Profiles: []string{dir}},
		Ping:          conn.Ping,
		StartupWindow: time.Nanosecond, // Zotero has been up for hours
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	st, err := Wait(ctx, WaitOptions{Prober: prober, WatchDirs: []string{dir}, StallRecheck: 2 * time.Second})
	if err != nil || st.State != StateReady {
		t.Fatalf("Wait across a 10s stall = %+v, %v; want ready", st, err)
	}
	if time.Now().Before(stallUntil) {
		t.Fatal("Wait returned ready before the stall ended")
	}
}

// resetListener reproduces the hung Zotero measured 2026-09-24: a listener on
// 127.0.0.1 only, whose connections complete the handshake and are then
// reset. Dialled as "localhost", [::1] refuses first.
func resetListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0) // close with RST
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return "http://localhost:" + port + "/connector"
}

func TestListeningOnSeesAListenerBehindARefusedAddress(t *testing.T) {
	if !ListeningOn(t.Context(), resetListener(t)) {
		t.Fatal("ListeningOn = false for a port held on 127.0.0.1 behind a refusing [::1]")
	}
	closed := realRefusalURL(t)
	if ListeningOn(t.Context(), closed) {
		t.Fatalf("ListeningOn(%s) = true for a closed port", closed)
	}
}

// The misclassification seen live: the ping reported "connection refused"
// (from [::1]) while Zotero held the port on 127.0.0.1. That is a hung
// Zotero, not a disabled connector, and the user must not be told to enable
// the connector setting.
func TestARefusedPingWithAListenerBehindItIsBusyNotConnectorOff(t *testing.T) {
	dir := t.TempDir()
	startLockHolder(t, dir)
	base := resetListener(t)
	misleading := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	p := &Prober{
		Install:       Install{Profiles: []string{dir}},
		Ping:          func(context.Context) error { return fmt.Errorf("connector ping: %w", misleading) },
		ConnectorURL:  base,
		StartupWindow: time.Nanosecond,
		Listening:     func(ctx context.Context) bool { return ListeningOn(ctx, base) },
	}
	if st := p.Probe(t.Context()); st.State != StateBusy {
		t.Fatalf("status = %+v, want busy: something holds the connector port", st)
	}
	closed := realRefusalURL(t)
	p.Listening = func(ctx context.Context) bool { return ListeningOn(ctx, closed) }
	if st := p.Probe(t.Context()); st.State != StateConnectorOff {
		t.Fatalf("status = %+v, want connector_off when every address refuses", st)
	}
}

func realRefusalURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	return "http://localhost:" + port + "/connector"
}
