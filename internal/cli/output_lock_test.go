// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/cliutil"
)

// blockFirstRequestServer answers every request with one item, but parks the
// first one until release is closed. That holds the first writer inside its
// transaction, which is when a competing writer must be refused.
func blockFirstRequestServer(t *testing.T) (srv *httptest.Server, firstRequest <-chan struct{}, release func()) {
	t.Helper()
	arrived := make(chan struct{})
	releaseCh := make(chan struct{})
	// CompareAndSwap, not sync.Once: Once.Do blocks concurrent callers, which
	// would park every later request behind the first one and hide whether the
	// competing writer was refused by the lock or merely queued behind a server.
	var parked atomic.Bool
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if parked.CompareAndSwap(false, true) {
			close(arrived)
			<-releaseCh
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"K1","version":1,"data":{"title":"t"}}]`))
	}))
	t.Cleanup(srv.Close)
	var releaseOnce sync.Once
	release = func() { releaseOnce.Do(func() { close(releaseCh) }) }
	// Registered after srv.Close so cleanup runs it first: a failed assertion
	// must not leave the handler parked, which would deadlock Close.
	t.Cleanup(release)
	return srv, arrived, release
}

func awaitFirstRequest(t *testing.T, firstRequest <-chan struct{}, who string) {
	t.Helper()
	select {
	case <-firstRequest:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never reached its first request", who)
	}
}

// holdOutputWriterLock takes the collision-namespace lock for target the way a
// concurrent zotio process would. flock is per open file description, so this
// contends with the command under test even inside one test binary.
func holdOutputWriterLock(t *testing.T, target string) *cliutil.WriterLock {
	t.Helper()
	lockPath, _, err := outputWriterLockPath(target)
	if err != nil {
		t.Fatalf("deriving output lock path: %v", err)
	}
	lock, err := cliutil.AcquireWriterLock(lockPath, "test holder")
	if err != nil {
		t.Fatalf("holding output lock: %v", err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	return lock
}

// useUnreachableAPI points the client at a closed port. A command that refuses a
// busy target before reading its source never contacts it, so a connection
// error here would prove the lock was taken too late.
func useUnreachableAPI(t *testing.T) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
}

func assertBusyPrecondition(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s succeeded against a busy target; want busy precondition exit 9", what)
	}
	if ExitCode(err) != 9 {
		t.Fatalf("%s exit = %d, want busy precondition exit 9: %v", what, ExitCode(err), err)
	}
}

// `export snapshot` and a plain `export -o` name the same file, so they must
// resolve to one lock identity. Plain exports took no lock at all before this,
// and would happily replace an artifact a running snapshot was still appending.
func TestExportBusyWhenSnapshotHoldsTheTarget(t *testing.T) {
	srv, firstRequest, release := blockFirstRequestServer(t)
	useTestServer(t, srv)

	target := filepath.Join(t.TempDir(), "shared.jsonl")
	snapshot := newExportSnapshotCmd(&rootFlags{})
	snapshot.SilenceErrors, snapshot.SilenceUsage = true, true
	snapshot.SetArgs([]string{"--output", target})
	snapshot.SetOut(&bytes.Buffer{})
	snapshot.SetErr(&bytes.Buffer{})
	snapshotErr := make(chan error, 1)
	go func() { snapshotErr <- snapshot.Execute() }()
	awaitFirstRequest(t, firstRequest, "export snapshot")

	export := newExportCmd(&rootFlags{asJSON: true})
	export.SilenceErrors, export.SilenceUsage = true, true
	export.SetArgs([]string{"items", "--output", target})
	export.SetOut(&bytes.Buffer{})
	export.SetErr(&bytes.Buffer{})
	assertBusyPrecondition(t, export.Execute(), "export")

	release()
	if err := <-snapshotErr; err != nil {
		t.Fatalf("snapshot: %v", err)
	}
}

// The same identity in the other direction, through both commands' own
// derivations: a snapshot must refuse a target a plain export is publishing.
func TestSnapshotBusyWhenExportLockHeld(t *testing.T) {
	srv, firstRequest, release := blockFirstRequestServer(t)
	useTestServer(t, srv)

	target := filepath.Join(t.TempDir(), "shared.jsonl")
	export := newExportCmd(&rootFlags{asJSON: true})
	export.SilenceErrors, export.SilenceUsage = true, true
	export.SetArgs([]string{"items", "--output", target})
	export.SetOut(&bytes.Buffer{})
	export.SetErr(&bytes.Buffer{})
	exportErr := make(chan error, 1)
	go func() { exportErr <- export.Execute() }()
	awaitFirstRequest(t, firstRequest, "export")

	snapshot := newExportSnapshotCmd(&rootFlags{})
	snapshot.SilenceErrors, snapshot.SilenceUsage = true, true
	snapshot.SetArgs([]string{"--output", target})
	snapshot.SetOut(&bytes.Buffer{})
	snapshot.SetErr(&bytes.Buffer{})
	assertBusyPrecondition(t, snapshot.Execute(), "export snapshot")

	release()
	if err := <-exportErr; err != nil {
		t.Fatalf("export: %v", err)
	}
}

func TestCollectionsExportBusyWhenTargetLockHeld(t *testing.T) {
	useUnreachableAPI(t)
	target := filepath.Join(t.TempDir(), "refs.bib")
	holdOutputWriterLock(t, target)

	cmd := newCollectionsExportCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"ROOT", "--output", target})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	assertBusyPrecondition(t, cmd.Execute(), "collections export")

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("refused export touched the target: %v", err)
	}
}

func TestAnnotationsExportBusyWhenTargetLockHeld(t *testing.T) {
	useUnreachableAPI(t)
	target := filepath.Join(t.TempDir(), "annotations.md")
	holdOutputWriterLock(t, target)

	cmd := newAnnotationsExportCmd(&rootFlags{asJSON: true, noCache: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"--collection", "COLL1", "--refresh", "--limit", "1", "--output", target})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	assertBusyPrecondition(t, cmd.Execute(), "annotations export")

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("refused export touched the target: %v", err)
	}
}

// The lock sibling lives in the user's output directory, so acquiring it must
// never destroy a file that is already there. zotio takes the advisory lock and
// leaves the bytes alone.
func TestExportPreservesPreexistingLockSiblingContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"K1","version":1,"data":{"title":"t"}}]`))
	}))
	t.Cleanup(srv.Close)
	useTestServer(t, srv)

	target := filepath.Join(t.TempDir(), "items.jsonl")
	const sentinel = "sentinel bytes"
	if err := os.WriteFile(target+".lock", []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newExportCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"items", "--output", target})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export: %v", err)
	}

	got, err := os.ReadFile(target + ".lock")
	if err != nil {
		t.Fatalf("reading lock sibling: %v", err)
	}
	if string(got) != sentinel {
		t.Fatalf("lock sibling content = %q, want %q", got, sentinel)
	}
}
