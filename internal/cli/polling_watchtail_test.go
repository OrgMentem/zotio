// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/cliutil"

	"github.com/spf13/cobra"
)

type watchtailLog struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	lines chan string
}

func (w *watchtailLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.buf.Write(p)
	select {
	case w.lines <- string(p):
	default:
	}
	return len(p), nil
}

func (w *watchtailLog) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func watchtailWaitLine(t *testing.T, log *watchtailLog, text string) {
	t.Helper()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-log.lines:
			if strings.Contains(line, text) {
				return
			}
		case <-timer.C:
			t.Fatalf("no %q log: %s", text, log.text())
		}
	}
}

// The completion log precedes the deferred unlock by a few instructions.
// Wait only for that handoff, not for the next (10-second) watch cycle.
func watchtailAcquireIdle(t *testing.T) *cliutil.WriterLock {
	t.Helper()
	path, err := installationWriterLockPath(nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		lock, err := cliutil.AcquireWriterLock(path, "another writer between polls")
		if err == nil {
			return lock
		}
		var busy *cliutil.WriterLockBusyError
		if !errors.As(err, &busy) || time.Now().After(deadline) {
			t.Fatalf("idle polling command holds installation lock: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

func watchtailRoot(flags *rootFlags, leaf *cobra.Command) *cobra.Command {
	root := &cobra.Command{Use: "zotio", SilenceErrors: true, SilenceUsage: true,
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
	}
	root.AddCommand(leaf)
	installInstallationWriterLocks(root, flags)
	return root
}

func TestWatchIdleUnlockAndMarkerReloadWatchtail(t *testing.T) {
	woSeededLibrary(t)
	spec := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{{Args: []string{"version"}}}})
	marker, err := watchWorkflowOwedPath(spec)
	if err != nil {
		t.Fatal(err)
	}
	flags := &rootFlags{timeout: 5 * time.Second, maxChanges: -1}
	root := watchtailRoot(flags, newWatchCmd(flags))
	log := &watchtailLog{lines: make(chan string, 100)}
	root.SetOut(io.Discard)
	root.SetErr(log)
	root.SetArgs([]string{"watch", "items", "--interval", "10s", "--workflow", spec, "--workflow-on-change"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	watchtailWaitLine(t, log, "cycle complete")
	lock := watchtailAcquireIdle(t)
	// A different process leaves an obligation while this watcher sleeps.
	err = os.WriteFile(marker, []byte(`{"upserted":7,"deleted":0}`), 0o600)
	_ = lock.Release()
	if err != nil {
		t.Fatal(err)
	}
	watchtailWaitLine(t, log, "cycle complete")
	lock = watchtailAcquireIdle(t)
	defer func() { _ = lock.Release() }()
	if !strings.Contains(log.text(), "library changed: 7 upserted") || !strings.Contains(log.text(), "workflow preview") {
		t.Fatalf("watch failed to reload the obligation: %s", log.text())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("handled marker remains: %v", err)
	}
}

func TestWatchMarkerFailureLoggedAndReturnedWatchtail(t *testing.T) {
	woSeededLibrary(t)
	spec := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{{Args: []string{"version"}}}})
	marker, err := watchWorkflowOwedPath(spec)
	if err != nil {
		t.Fatal(err)
	}
	flags := &rootFlags{timeout: 5 * time.Second, maxChanges: -1}
	root := watchtailRoot(flags, newWatchCmd(flags))
	log := &watchtailLog{lines: make(chan string, 100)}
	root.SetOut(io.Discard)
	root.SetErr(log)
	root.SetArgs([]string{"watch", "items", "--interval", "10s", "--workflow", spec, "--workflow-on-change"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	joined := false
	defer func() {
		cancel()
		if !joined {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
	}()
	watchtailWaitLine(t, log, "cycle complete")
	lock := watchtailAcquireIdle(t)
	err = os.Mkdir(marker, 0o700)
	if err == nil {
		err = os.WriteFile(filepath.Join(marker, "keep"), []byte("occupied"), 0o600)
	}
	_ = lock.Release()
	if err != nil {
		t.Fatal(err)
	}
	watchtailWaitLine(t, log, "cycle error: recording the workflow marker before the cycle")
	cancel()
	err = <-done
	joined = true
	if err == nil || !strings.Contains(err.Error(), "recording the workflow marker before the cycle") {
		t.Fatalf("shutdown lost last cycle failure after earlier success: %v", err)
	}
}

func TestTailFollowRetriesHeldCursorAndUnlocksWatchtail(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("failFirst=%t", failFirst), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("ZOTERO_HOME", "")
			t.Setenv("ZOTERO_DATA_DIR", t.TempDir())
			t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
			t.Setenv("ZOTIO_DEMO", "")
			var polls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Last-Modified-Version", "10")
				if strings.HasSuffix(r.URL.Path, "/deleted") {
					_, _ = io.WriteString(w, `{"items":[]}`)
					return
				}
				n := polls.Add(1)
				if (failFirst && n == 1) || (!failFirst && n == 2) {
					_, _ = io.WriteString(w, `not-json`)
					return
				}
				if !failFirst && n >= 3 && r.URL.Query().Get("since") != "10" {
					t.Errorf("retry cursor = %q, want 10", r.URL.Query().Get("since"))
				}
				_, _ = io.WriteString(w, `[{"key":"ABCD2345","version":10,"data":{"key":"ABCD2345","itemType":"book"}}]`)
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL)
			flags := &rootFlags{timeout: time.Second, maxChanges: -1}
			root := watchtailRoot(flags, newTailCmd(flags))
			log := &watchtailLog{lines: make(chan string, 100)}
			out := &watchtailLog{lines: make(chan string, 100)}
			root.SetOut(out)
			root.SetErr(log)
			root.SetArgs([]string{"tail", "items", "--interval", "100ms"})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- root.ExecuteContext(ctx) }()
			defer func() {
				cancel()
				if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			}()
			watchtailWaitLine(t, log, "poll error:")
			lock := watchtailAcquireIdle(t)
			_ = lock.Release()
			if !failFirst {
				watchtailWaitLine(t, out, `"event":"upsert"`)
			}
			watchtailWaitLine(t, out, `"event":"upsert"`)
			want := int32(3)
			if failFirst {
				want = 2
			}
			if polls.Load() < want {
				t.Fatalf("follow stopped after failure: %d polls", polls.Load())
			}
		})
	}
}

func TestSameOutputDeliverIsUsageErrorWatchtail(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	target := filepath.Join(t.TempDir(), "result.json")
	flags := &rootFlags{deliverSpec: "file:" + target, maxChanges: -1}
	ran := false
	leaf := &cobra.Command{Use: "export", RunE: func(*cobra.Command, []string) error { ran = true; return nil }}
	leaf.Flags().String("output", "", "")
	root := watchtailRoot(flags, leaf)
	root.SetArgs([]string{"export", "--output", filepath.Join(filepath.Dir(target), ".", filepath.Base(target))})
	err := root.Execute()
	if ExitCode(err) != 2 || !strings.Contains(err.Error(), "must name different files") || ran {
		t.Fatalf("same-target refusal: ran=%t exit=%d err=%v", ran, ExitCode(err), err)
	}
}

type watchtailAppend struct {
	*os.File
	short    bool
	writeErr error
	syncErr  error
	syncs    int
}

func (f *watchtailAppend) Write(p []byte) (int, error) {
	if f.short || f.writeErr != nil {
		n, err := f.File.Write(p[:len(p)/2])
		return n, errors.Join(err, f.writeErr)
	}
	return f.File.Write(p)
}

func (f *watchtailAppend) Sync() error {
	f.syncs++
	if f.syncs == 1 && f.syncErr != nil {
		return f.syncErr
	}
	return f.File.Sync()
}

func TestTailAppendDurabilityAndRollbackWatchtail(t *testing.T) {
	for _, mode := range []string{"success", "short", "write-error", "sync-error"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			before := []byte("{\"old\":true}\n")
			batch := []byte("{\"new\":true}\n")
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			f := &watchtailAppend{File: file, short: mode == "short"}
			if mode == "write-error" {
				f.writeErr = errors.New("disk full")
			}
			if mode == "sync-error" {
				f.syncErr = errors.New("sync failed")
			}
			err = appendTailBatch(f, batch)
			if (err != nil) != (mode != "success") {
				t.Fatalf("append: %v", err)
			}
			if mode == "short" && !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("short write: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := string(before)
			if mode == "success" {
				want += string(batch)
			}
			if string(got) != want {
				t.Fatalf("append contents = %q, want %q", got, want)
			}
			if f.syncs == 0 {
				t.Fatal("append/rollback was not synced")
			}
			if mode != "success" {
				if err := appendTailBatch(file, batch); err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(path)
				if err != nil || string(got) != string(before)+string(batch) {
					t.Fatalf("retry appended a partial line: %q, %v", got, err)
				}
			}
		})
	}
}
