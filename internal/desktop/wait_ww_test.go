// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package desktop

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Without a watch, only the re-check timer drives probes, and it runs only
// while Zotero is starting or busy. When Zotero quits from either state no
// filesystem event could announce its next start, so Wait must end the way
// a wait that began with Zotero closed ends instead of blocking forever.
func TestWaitWithoutWatchesEndsWhenAStartingOrBusyZoteroQuits(t *testing.T) {
	tests := []struct {
		name string
		// missingDir watches a directory that does not exist, so no watch
		// can be installed; otherwise there is nothing to watch at all.
		missingDir    bool
		busy          bool
		wantNoProfile bool
	}{
		{name: "starting, watch failed", missingDir: true},
		{name: "busy, watch failed", missingDir: true, busy: true},
		{name: "starting, nothing to watch", wantNoProfile: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := newInstall(t)
			h := startLockHolder(t, in.profile) // Zotero takes its lock
			conn := &fakeConnector{fail: errNoAnswer}
			done := startWait(t, t.Context(), in, conn, func(o *WaitOptions) {
				o.WatchDirs = nil
				if tt.missingDir {
					o.WatchDirs = []string{filepath.Join(in.data, "missing")}
				}
				o.ConfirmFirst, o.ConfirmMax = 10*time.Millisecond, 40*time.Millisecond
				o.Prober.StartupWindow = time.Hour
				if tt.busy {
					o.Prober.StartupWindow = time.Nanosecond
				}
				// Busy re-checks come quickly but never prove a stall.
				o.StallRecheck, o.StallSpan = 10*time.Millisecond, time.Hour
			})

			h.release(t) // Zotero quits before its connector answers
			r := awaitResult(t, done)
			if r.st.State != StateStopped {
				t.Fatalf("Wait = %+v, %v; want the stopped status", r.st, r.err)
			}
			switch {
			case r.err == nil || errors.Is(r.err, ErrStuck):
				t.Fatalf("Wait error = %v, want the reason nothing is watched", r.err)
			case tt.wantNoProfile && !errors.Is(r.err, ErrNoProfile):
				t.Fatalf("Wait error = %v, want ErrNoProfile", r.err)
			case !tt.wantNoProfile && errors.Is(r.err, ErrNoProfile):
				t.Fatalf("Wait error = %v, want the watch failure, not ErrNoProfile", r.err)
			}
		})
	}
}
