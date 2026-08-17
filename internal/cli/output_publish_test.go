// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func tempTargets(t *testing.T) (dir string, target string) {
	t.Helper()
	dir = t.TempDir()
	return dir, filepath.Join(dir, "out.jsonl")
}

// countLeftoverTemps proves aborts do not litter. Callers assert 0.
func countLeftoverTemps(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".zotio-output-") {
			n++
		}
	}
	return n
}

func TestWithAtomicOutputFilePublishesOnSuccess(t *testing.T) {
	dir, target := tempTargets(t)
	err := withAtomicOutputFile(target, 0o600, func(w io.Writer) error {
		_, writeErr := io.WriteString(w, "published\n")
		return writeErr
	})
	if err != nil {
		t.Fatalf("withAtomicOutputFile: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if string(got) != "published\n" {
		t.Fatalf("target = %q, want %q", got, "published\n")
	}
	if n := countLeftoverTemps(t, dir); n != 0 {
		t.Fatalf("%d temporary files left behind", n)
	}
	info, err := os.Lstat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("published mode = %v, want 0600", info.Mode().Perm())
	}
}

// The whole point of the change: a failed producer must leave the previous
// artifact byte-identical rather than truncating or partially overwriting it.
func TestWithAtomicOutputFileFailureKeepsPreviousArtifact(t *testing.T) {
	dir, target := tempTargets(t)
	const previous = "the good export that must survive\n"
	if err := os.WriteFile(target, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("source fetch failed midway")
	err := withAtomicOutputFile(target, 0o600, func(w io.Writer) error {
		// Write real bytes first: truncation used to happen before this point,
		// so a producer that fails after emitting output is the exact case.
		if _, writeErr := io.WriteString(w, "partial garbage that must never be published"); writeErr != nil {
			return writeErr
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the producer's own error", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if string(got) != previous {
		t.Fatalf("previous artifact was damaged: %q", got)
	}
	if n := countLeftoverTemps(t, dir); n != 0 {
		t.Fatalf("%d temporary files left behind", n)
	}
}

func TestWithAtomicOutputFileFailureLeavesNoFileWhenTargetIsNew(t *testing.T) {
	dir, target := tempTargets(t)
	sentinel := errors.New("boom")
	if err := withAtomicOutputFile(target, 0o600, func(w io.Writer) error {
		_, _ = io.WriteString(w, "partial")
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("target exists after a failed first publication (err=%v)", err)
	}
	if n := countLeftoverTemps(t, dir); n != 0 {
		t.Fatalf("%d temporary files left behind", n)
	}
}

// Write barrier: while a publication is mid-flight, every open of the target
// must return the complete previous bytes. A postcondition-only test would
// pass even if the target were truncated and rewritten quickly.
func TestWithAtomicOutputFileNeverExposesPartialContent(t *testing.T) {
	_, target := tempTargets(t)
	const previous = "complete-previous-artifact"
	if err := os.WriteFile(target, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}

	released := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withAtomicOutputFile(target, 0o600, func(w io.Writer) error {
			if _, err := io.WriteString(w, "new-artifact-first-chunk"); err != nil {
				return err
			}
			// Hold the publication open with real bytes already in the temp.
			<-released
			_, err := io.WriteString(w, "-second-chunk")
			return err
		})
	}()

	for range 50 {
		got, err := os.ReadFile(target)
		if err != nil {
			t.Fatalf("target unreadable mid-publication: %v", err)
		}
		if string(got) != previous {
			t.Fatalf("observed partial/!previous content mid-publication: %q", got)
		}
	}
	close(released)
	if err := <-done; err != nil {
		t.Fatalf("withAtomicOutputFile: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if string(got) != "new-artifact-first-chunk-second-chunk" {
		t.Fatalf("final target = %q", got)
	}
}

func TestWithAtomicOutputFileRefusesNonRegularTargets(t *testing.T) {
	dir := t.TempDir()

	t.Run("symlink", func(t *testing.T) {
		referent := filepath.Join(dir, "referent.jsonl")
		if err := os.WriteFile(referent, []byte("referent"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link.jsonl")
		if err := os.Symlink(referent, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		err := withAtomicOutputFile(link, 0o600, func(w io.Writer) error {
			t.Fatal("producer must not run for a rejected target")
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("error = %v, want symlink refusal", err)
		}
		// The link and its referent must be untouched.
		if fi, statErr := os.Lstat(link); statErr != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink was replaced (err=%v)", statErr)
		}
		if got, _ := os.ReadFile(referent); string(got) != "referent" {
			t.Fatalf("referent was modified: %q", got)
		}
	})

	t.Run("directory", func(t *testing.T) {
		target := filepath.Join(dir, "adir")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		err := withAtomicOutputFile(target, 0o600, func(w io.Writer) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("error = %v, want directory refusal", err)
		}
	})
}

// Publication must not create directories the user never asked for: that was a
// reason not to reuse cliutil.atomicWrite, which MkdirAlls unconditionally.
func TestWithAtomicOutputFileDoesNotCreateParentDirectories(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope", "out.jsonl")
	err := withAtomicOutputFile(missing, 0o600, func(w io.Writer) error { return nil })
	if err == nil {
		t.Fatal("publication succeeded despite a missing parent directory")
	}
	if _, statErr := os.Lstat(filepath.Join(dir, "nope")); !os.IsNotExist(statErr) {
		t.Fatalf("parent directory was created (err=%v)", statErr)
	}
}

// A long basename must not push the temporary name past NAME_MAX, which is why
// the pattern is a short fixed prefix rather than the target's own name.
func TestWithAtomicOutputFileHandlesLongBasename(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, strings.Repeat("n", 200)+".jsonl")
	if err := withAtomicOutputFile(target, 0o600, func(w io.Writer) error {
		_, err := io.WriteString(w, "ok")
		return err
	}); err != nil {
		t.Fatalf("withAtomicOutputFile: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "ok" {
		t.Fatalf("target = %q (err=%v)", got, err)
	}
}

func TestPublishedOutputModePreservesExistingPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := publishedOutputMode(existing, 0o600); got != 0o644 {
		t.Fatalf("publishedOutputMode = %v, want 0644 (os.WriteFile leaves an existing mode alone)", got)
	}
	if got := publishedOutputMode(filepath.Join(dir, "absent"), 0o600); got != 0o600 {
		t.Fatalf("publishedOutputMode(absent) = %v, want the fallback 0600", got)
	}
}

// Two concurrent publications must both produce a complete artifact; the
// survivor is whichever renamed last. This documents that atomic publication
// prevents tearing but deliberately does not detect collisions -- collision
// detection is the separate path-lock stage.
func TestWithAtomicOutputFileConcurrentPublicationsAreNeverTorn(t *testing.T) {
	_, target := tempTargets(t)
	const n = 8
	done := make(chan error, n)
	for i := range n {
		body := fmt.Sprintf("complete-artifact-%d", i)
		go func() {
			done <- withAtomicOutputFile(target, 0o600, func(w io.Writer) error {
				for _, chunk := range strings.Split(body, "-") {
					if _, err := io.WriteString(w, chunk+"-"); err != nil {
						return err
					}
				}
				return nil
			})
		}()
	}
	for i := range n {
		if err := <-done; err != nil {
			t.Fatalf("publication %d: %v", i, err)
		}
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if !strings.HasPrefix(string(got), "complete-artifact-") || !strings.HasSuffix(string(got), "-") {
		t.Fatalf("target holds a torn artifact: %q", got)
	}
}
