// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package desktop

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// lockHolderEnv turns the test binary into a lock-holding child. The lock
// must live in ANOTHER process: fcntl F_GETLK never reports a lock the
// querying process holds itself, so an in-process holder would prove nothing.
const lockHolderEnv = "ZOTIO_TEST_DESKTOP_LOCK_HOLDER"

func TestMain(m *testing.M) {
	if path := os.Getenv(lockHolderEnv); path != "" {
		os.Exit(runLockHolder(path))
	}
	os.Exit(m.Run())
}

// runLockHolder takes the profile lock the way Zotero does, reports it, and
// holds it until its stdin closes.
func runLockHolder(path string) int {
	release, err := holdLock(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
	release()
	return 0
}

// lockHolder is a child process holding a profile lock.
type lockHolder struct {
	pid   int
	stdin io.WriteCloser
	cmd   *exec.Cmd
	done  bool
}

// startLockHolder starts a child that holds the Zotero lock file in
// profileDir, and returns once the lock is held.
func startLockHolder(t *testing.T, profileDir string) *lockHolder {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), lockHolderEnv+"="+filepath.Join(profileDir, lockFileName))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &lockHolder{pid: cmd.Process.Pid, stdin: stdin, cmd: cmd}
	t.Cleanup(func() {
		if !h.done {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	select {
	case line := <-ready:
		if line != "locked" {
			t.Fatalf("lock holder said %q, stderr: %s", line, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("lock holder did not take the lock; stderr: %s", stderr.String())
	}
	return h
}

// release makes the child drop the lock and exit, as Zotero does on quit.
func (h *lockHolder) release(t *testing.T) {
	t.Helper()
	_ = h.stdin.Close()
	if err := h.cmd.Wait(); err != nil {
		t.Fatalf("lock holder exit: %v", err)
	}
	h.done = true
}
