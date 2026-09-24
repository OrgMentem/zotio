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

// WaitOptions configures Wait. The startup window is the Prober's.
type WaitOptions struct {
	Prober *Prober
	// WatchDirs are watched for changes; see Install.WatchDirs.
	WatchDirs []string

	Settle       time.Duration
	ConfirmFirst time.Duration
	ConfirmMax   time.Duration

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
// writes no file. When the startup window passes with the connector still
// silent, or when Zotero is already past it, Wait returns ErrStuck with the
// unresponsive or connector_off Status.
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

	// firstSeen stands in for the lock time when the platform reports none.
	var firstSeen time.Time
	probe := func() Status {
		st := o.Prober.probe(ctx, firstSeen)
		switch {
		case !st.LockHeld():
			firstSeen = time.Time{}
		case firstSeen.IsZero():
			firstSeen = st.CheckedAt
		}
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

	st := probe()
	if done, err := settled(st); done {
		return st, err
	}
	if watcher == nil {
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
		confirm *time.Timer
		backoff time.Duration
	)
	stopTimer := func(t **time.Timer) {
		if *t != nil {
			(*t).Stop()
			*t = nil
		}
	}
	defer stopTimer(&settle)
	defer stopTimer(&confirm)
	timerC := func(t *time.Timer) <-chan time.Time {
		if t == nil {
			return nil
		}
		return t.C
	}
	// nextConfirm schedules the next re-check: the backoff step, but never
	// past the end of the startup window, so leaving "starting" is noticed
	// when it happens rather than up to one step later.
	nextConfirm := func() {
		delay := backoff
		if until := time.Until(o.Prober.startingUntil(st, firstSeen)); until < delay {
			delay = max(until, 0) + 10*time.Millisecond
		}
		confirm = time.NewTimer(delay)
	}
	// track keeps the re-checks running exactly while Zotero is starting.
	track := func() {
		if st.State != StateStarting {
			stopTimer(&confirm)
			return
		}
		if confirm == nil {
			backoff = o.ConfirmFirst
			nextConfirm()
		}
	}
	track()

	for {
		select {
		case <-ctx.Done():
			return st, context.Cause(ctx)

		case _, ok := <-watcher.Events:
			if !ok {
				return st, errors.New("filesystem watcher closed")
			}
			if settle == nil {
				settle = time.NewTimer(o.Settle)
			}

		case _, ok := <-watcher.Errors:
			if !ok {
				return st, errors.New("filesystem watcher closed")
			}
			// An overflow or read error means events may have been lost:
			// probe as though one arrived rather than trusting the silence.
			if settle == nil {
				settle = time.NewTimer(o.Settle)
			}

		case <-timerC(settle):
			settle = nil
			st = probe()
			if done, err := settled(st); done {
				return st, err
			}
			track()

		case <-timerC(confirm):
			confirm = nil
			st = probe()
			if done, err := settled(st); done {
				return st, err
			}
			if st.State == StateStarting {
				backoff = min(backoff*2, o.ConfirmMax)
				nextConfirm()
			}
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
