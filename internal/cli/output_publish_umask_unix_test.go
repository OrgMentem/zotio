// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// withUmask runs fn under mask and restores the previous umask. The umask is
// process-wide, so callers must not run in parallel.
func withUmask(t *testing.T, mask int, fn func()) {
	t.Helper()
	old := syscall.Umask(mask)
	defer syscall.Umask(old)
	fn()
}

// A direct write creates a new file with perm minus the umask. The atomic
// publication sets the mode with fchmod, which no umask masks, so a card for a
// user with `umask 077` was published world-readable (0644) unless the mode it
// is given already reflects the umask.
func TestPublishedOutputModeAppliesUmaskToNewTargets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		umask int
		want  os.FileMode
	}{
		{"private umask", 0o077, 0o600},
		{"group-readable umask", 0o027, 0o640},
		{"default umask keeps the requested mode", 0o022, 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "card.svg")
			withUmask(t, tc.umask, func() {
				if got := publishedOutputMode(target, 0o644); got != tc.want {
					t.Fatalf("publishedOutputMode(new target) = %04o, want %04o", got, tc.want)
				}
			})
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatalf("mode probe left files behind: %v (err %v)", entries, err)
			}
		})
	}

	t.Run("missing directory falls back to the requested mode", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "absent", "card.svg")
		withUmask(t, 0o077, func() {
			if got := publishedOutputMode(target, 0o644); got != 0o644 {
				t.Fatalf("publishedOutputMode(missing dir) = %04o, want the fallback 0644", got)
			}
		})
	})
}

// End to end through the real card writer: under a restrictive umask the
// published card must be no more permissive than a direct write would leave it,
// and an existing card keeps its own mode.
func TestWriteLibraryWrappedCardHonoursUmask(t *testing.T) {
	dir := t.TempDir()
	card := filepath.Join(dir, "card.svg")
	withUmask(t, 0o077, func() {
		if err := writeLibraryWrappedCard(card, libraryWrappedReport{}, "overview"); err != nil {
			t.Fatalf("writeLibraryWrappedCard: %v", err)
		}
	})
	info, err := os.Stat(card)
	if err != nil {
		t.Fatalf("stat card: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("new card mode under umask 077 = %04o, want 0600", got)
	}

	if err := os.Chmod(card, 0o644); err != nil {
		t.Fatal(err)
	}
	withUmask(t, 0o077, func() {
		if err := writeLibraryWrappedCard(card, libraryWrappedReport{}, "overview"); err != nil {
			t.Fatalf("rewrite card: %v", err)
		}
	})
	info, err = os.Stat(card)
	if err != nil {
		t.Fatalf("stat rewritten card: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("rewritten card mode = %04o, want the existing 0644 preserved", got)
	}
}
