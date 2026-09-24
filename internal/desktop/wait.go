// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package desktop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ErrNoProfile means discovery found no Zotero profile directory, so there
// is nothing whose changes could announce a start, and the connector does
// not answer either.
var ErrNoProfile = errors.New("no Zotero desktop profile directory found")

// ErrStuck means Zotero is running past its startup window and its connector
// cannot take requests; the returned Status.State says which way
// (StateUnresponsive or StateConnectorOff). No filesystem change announces a
// recovery, so Wait returns instead of waiting silently: the caller tells the
// user and decides when to wait again.
var ErrStuck = errors.New("Zotero desktop is running but its connector cannot take requests")

// Wait tuning. A package-level default so a caller passing a zero WaitOptions
// gets the documented behaviour.
const (
	// DefaultSettle coalesces a burst of filesystem events into one probe:
	// a Zotero start touches the lock file and creates two WAL files within
	// about a second.
	DefaultSettle = 200 * time.Millisecond
	// DefaultConfirmFirst and DefaultConfirmMax shape the connector re-checks
	// while Zotero is starting: the first comes quickly, later ones at most
	// this far apart.
	DefaultConfirmFirst = 250 * time.Millisecond
	DefaultConfirmMax   = 2 * time.Second
)

// WaitOptions configures Wait. The startup window is the Prober's; zero
// durations and counts take the package defaults.
type WaitOptions struct {
	Prober *Prober
	// WatchDirs are watched for changes; see Install.WatchDirs.
	WatchDirs []string

	Settle       time.Duration
	ConfirmFirst time.Duration
	ConfirmMax   time.Duration

	// Sustained-stall evidence for StateUnresponsive; see DefaultStallSpan.
	StallSpan        time.Duration
	StallChecks      int
	StallRecheck     time.Duration
	StallPingTimeout time.Duration

	// OnWatching, when set, runs once the watches are installed and the
	// first probe found the connector down. Tests use it to know that a
	// later filesystem change will be seen.
	OnWatching func()
}

func (o WaitOptions) withDefaults() WaitOptions {
	if o.Settle <= 0 {
		o.Settle = DefaultSettle
	}
	if o.ConfirmFirst <= 0 {
		o.ConfirmFirst = DefaultConfirmFirst
	}
	if o.ConfirmMax < o.ConfirmFirst {
		o.ConfirmMax = max(DefaultConfirmMax, o.ConfirmFirst)
	}
	if o.StallSpan <= 0 {
		o.StallSpan = DefaultStallSpan
	}
	if o.StallChecks <= 0 {
		o.StallChecks = DefaultStallChecks
	}
	if o.StallRecheck <= 0 {
		o.StallRecheck = DefaultStallRecheck
	}
	if o.StallPingTimeout <= 0 {
		o.StallPingTimeout = DefaultStallPingTimeout
	}
	return o
}

// Wait blocks until the Zotero connector accepts requests and returns the
// Status that proved it. If the connector already answers, it returns at
// once.
//
// While Zotero is closed nothing runs on a timer: Wait sleeps on filesystem
// notifications for the profile and data directories and probes only after
// a change settles. While Zotero is starting (lock held, connector silent,
// lock younger than the startup window) the connector is re-checked on a
// capped backoff, because it listens seconds after the lock and its start
// writes no file.
//
// Past the startup window a connector that cannot take a request is
// re-checked every StallRecheck with the longer StallPingTimeout. The wait
// ends with ErrStuck only once StallChecks consecutive checks have failed
// across at least StallSpan: as StateUnresponsive if any of them found a
// listener on the connector port, as StateConnectorOff if none did. Any
// answer in between ends it as ready, so a sync that stalls Zotero briefly
// is never reported as a hang. While Zotero is starting or busy, the
// re-check timer alone drives probes; filesystem events (a sync writes its
// WAL constantly) do not add more.
//
// On cancellation Wait returns the last Status and context.Cause(ctx), so a
// caller that set a deadline with context.WithTimeoutCause can tell a
// timeout from an interrupt.
func Wait(ctx context.Context, opts WaitOptions) (Status, error) {
	if opts.Prober == nil {
		return Status{}, errors.New("desktop.Wait: nil Prober")
	}
	o := opts.withDefaults()

	// Subscribe before the first probe: a Zotero that starts between the
	// probe and the subscription would otherwise raise its events unseen.
	watcher, watchErr := newWatcher(o.WatchDirs)
	if watcher != nil {
		defer watcher.Close()
	}

	var (
		// firstSeen stands in for the lock time when the platform reports none.
		firstSeen  time.Time
		stallSince time.Time
		stallFails int
		// stallHeld: some check in the stall found a listener on the
		// connector port, so the stall is a hang rather than a connector
		// that is off.
		stallHeld bool
	)
	probe := func(pingTimeout time.Duration) Status {
		st := o.Prober.probe(ctx, firstSeen, pingTimeout)
		switch {
		case !st.LockHeld():
			firstSeen = time.Time{}
		case firstSeen.IsZero():
			firstSeen = st.CheckedAt
		}
		if st.State != StateBusy && st.State != StateConnectorOff {
			stallSince, stallFails, stallHeld = time.Time{}, 0, false
			return st
		}
		if stallSince.IsZero() {
			stallSince = st.CheckedAt
		}
		stallFails++
		stallHeld = stallHeld || st.State == StateBusy
		since := stallSince
		st.StalledSince = &since
		if stallFails >= o.StallChecks && st.CheckedAt.Sub(stallSince) >= o.StallSpan {
			if stallHeld {
				st.State = StateUnresponsive
			} else {
				st.State = StateConnectorOff
			}
			return st
		}
		// Not proven yet either way: keep waiting.
		st.State = StateBusy
		return st
	}
	// settled reports whether st ends the wait, and how.
	settled := func(st Status) (bool, error) {
		switch {
		case st.ConnectorReachable:
			return true, nil
		case st.State.Stuck():
			return true, ErrStuck
		default:
			return false, nil
		}
	}

	st := probe(0)
	if done, err := settled(st); done {
		return st, err
	}
	if watcher == nil && st.State != StateBusy && st.State != StateStarting {
		if watchErr == nil {
			return st, ErrNoProfile
		}
		return st, watchErr
	}
	if o.OnWatching != nil {
		o.OnWatching()
	}

	var (
		settle  *time.Timer
		recheck *time.Timer
		backoff time.Duration
		phase   State
	)
	stopTimer := func(t **time.Timer) {
		if *t != nil {
			(*t).Stop()
			*t = nil
		}
	}
	defer stopTimer(&settle)
	defer stopTimer(&recheck)
	timerC := func(t *time.Timer) <-chan time.Time {
		if t == nil {
			return nil
		}
		return t.C
	}
	// reschedule arms the re-check timer for the state just observed:
	// backoff while starting (never past the end of the startup window, so
	// leaving it is noticed when it happens), the stall cadence while busy
	// (never past the end of the stall span), nothing otherwise.
	reschedule := func() {
		stopTimer(&recheck)
		var delay time.Duration
		switch st.State {
		case StateStarting:
			if phase == StateStarting {
				backoff = min(backoff*2, o.ConfirmMax)
			} else {
				backoff = o.ConfirmFirst
			}
			delay = backoff
			if until := time.Until(o.Prober.startingUntil(st, firstSeen)); until < delay {
				delay = max(until, 0) + 10*time.Millisecond
			}
		case StateBusy:
			delay = o.StallRecheck
			if until := time.Until(stallSince.Add(o.StallSpan)); until > 0 && until < delay {
				delay = until + 10*time.Millisecond
			}
		}
		phase = st.State
		if delay > 0 {
			recheck = time.NewTimer(delay)
		}
	}
	reschedule()

	// A nil watcher (nothing watchable, but Zotero already up) leaves the
	// event cases blocked forever, which is what they should be.
	var events <-chan fsnotify.Event
	var watchErrs <-chan error
	if watcher != nil {
		events, watchErrs = watcher.Events, watcher.Errors
	}
	onEvent := func() {
		// While starting or busy the re-check timer drives probes.
		if recheck == nil && settle == nil {
			settle = time.NewTimer(o.Settle)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return st, context.Cause(ctx)

		case _, ok := <-events:
			if !ok {
				return st, errors.New("filesystem watcher closed")
			}
			onEvent()

		case _, ok := <-watchErrs:
			if !ok {
				return st, errors.New("filesystem watcher closed")
			}
			// An overflow or read error means events may have been lost:
			// probe as though one arrived rather than trusting the silence.
			onEvent()

		case <-timerC(settle):
			settle = nil
			st = probe(0)
			if done, err := settled(st); done {
				return st, err
			}
			reschedule()

		case <-timerC(recheck):
			recheck = nil
			pingTimeout := time.Duration(0)
			if phase == StateBusy {
				pingTimeout = o.StallPingTimeout
			}
			st = probe(pingTimeout)
			if done, err := settled(st); done {
				return st, err
			}
			reschedule()
		}
	}
}

// newWatcher watches every directory it can. It returns a nil watcher when
// none could be watched, with the reason, or with a nil error when there
// was nothing to watch.
func newWatcher(dirs []string) (*fsnotify.Watcher, error) {
	if len(dirs) == 0 {
		return nil, nil
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("starting the filesystem watcher: %w", err)
	}
	var errs []error
	added := 0
	for _, dir := range dirs {
		if err := w.Add(dir); err != nil {
			errs = append(errs, fmt.Errorf("watching %s: %w", dir, err))
			continue
		}
		added++
	}
	if added == 0 {
		_ = w.Close()
		return nil, errors.Join(errs...)
	}
	return w, nil
}
