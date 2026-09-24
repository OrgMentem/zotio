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

// Wait tuning. A package-level default so a caller passing a zero WaitOptions
// gets the documented behaviour.
const (
	// DefaultSettle coalesces a burst of filesystem events into one probe:
	// a Zotero start touches the lock file and creates two WAL files within
	// about a second.
	DefaultSettle = 200 * time.Millisecond
	// DefaultConfirmWindow bounds the connector re-checks after the profile
	// lock is first seen held. Zotero's connector listens a few seconds after
	// the lock; a database upgrade after a Zotero update can take longer, and
	// such an upgrade keeps writing the WAL, which raises further events
	// after the window closes.
	DefaultConfirmWindow = 2 * time.Minute
	// DefaultConfirmFirst and DefaultConfirmMax shape the confirm backoff:
	// the first re-check comes quickly, later ones at most this far apart.
	DefaultConfirmFirst = 250 * time.Millisecond
	DefaultConfirmMax   = 2 * time.Second
)

// WaitOptions configures Wait.
type WaitOptions struct {
	Prober *Prober
	// WatchDirs are watched for changes; see Install.WatchDirs.
	WatchDirs []string

	Settle        time.Duration
	ConfirmWindow time.Duration
	ConfirmFirst  time.Duration
	ConfirmMax    time.Duration

	// OnWatching, when set, runs once the watches are installed and the
	// first probe found the connector down. Tests use it to know that a
	// later filesystem change will be seen.
	OnWatching func()
}

func (o WaitOptions) withDefaults() WaitOptions {
	if o.Settle <= 0 {
		o.Settle = DefaultSettle
	}
	if o.ConfirmWindow <= 0 {
		o.ConfirmWindow = DefaultConfirmWindow
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
// a change settles. Once the profile lock is seen held, the connector is
// re-checked on a capped backoff for ConfirmWindow, because the connector
// listens seconds after the lock and its start writes no file. If the window
// passes with the lock held and the connector still silent (connector
// disabled, another port, a hung Zotero), Wait goes back to sleeping on
// events; a new confirm window opens only when the lock is released and
// taken again.
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

	st := o.Prober.Probe(ctx)
	if st.ConnectorReachable {
		return st, nil
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
		settle          *time.Timer
		confirm         *time.Timer
		confirmDeadline time.Time
		backoff         time.Duration
		lockSeen        = st.LockHeld()
	)
	stopTimer := func(t **time.Timer) {
		if *t != nil {
			(*t).Stop()
			*t = nil
		}
	}
	defer stopTimer(&settle)
	defer stopTimer(&confirm)
	startConfirm := func() {
		stopTimer(&confirm)
		confirmDeadline = time.Now().Add(o.ConfirmWindow)
		backoff = o.ConfirmFirst
		confirm = time.NewTimer(backoff)
	}
	if lockSeen {
		startConfirm()
	}
	timerC := func(t *time.Timer) <-chan time.Time {
		if t == nil {
			return nil
		}
		return t.C
	}
	armSettle := func() {
		if settle == nil {
			settle = time.NewTimer(o.Settle)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return st, context.Cause(ctx)

		case _, ok := <-watcher.Events:
			if !ok {
				return st, errors.New("filesystem watcher closed")
			}
			armSettle()

		case _, ok := <-watcher.Errors:
			if !ok {
				return st, errors.New("filesystem watcher closed")
			}
			// An overflow or read error means events may have been lost:
			// probe as though one arrived rather than trusting the silence.
			armSettle()

		case <-timerC(settle):
			settle = nil
			st = o.Prober.Probe(ctx)
			if st.ConnectorReachable {
				return st, nil
			}
			held := st.LockHeld()
			switch {
			case held && !lockSeen:
				startConfirm()
			case !held:
				stopTimer(&confirm)
			}
			lockSeen = held

		case <-timerC(confirm):
			confirm = nil
			st = o.Prober.Probe(ctx)
			if st.ConnectorReachable {
				return st, nil
			}
			lockSeen = st.LockHeld()
			remaining := time.Until(confirmDeadline)
			if !lockSeen || remaining <= 0 {
				continue
			}
			backoff = min(backoff*2, o.ConfirmMax)
			confirm = time.NewTimer(min(backoff, remaining))
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
