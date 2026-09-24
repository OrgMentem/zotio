// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

// Package desktop reports whether Zotero desktop is running and waits for it
// to start, without polling while it is closed.
//
// # What "running" means
//
// Two independent signals exist, and they answer different questions:
//
//   - The profile lock. Zotero is a Mozilla-platform application and holds a
//     lock in its profile directory for as long as the process lives: an
//     fcntl write lock on ".parentlock" on macOS and Linux, and an exclusive
//     (share mode 0, delete-on-close) handle on "parent.lock" on Windows. A
//     held lock says the PROCESS is up. It says nothing about whether Zotero
//     can accept work yet.
//   - The connector. Zotero's HTTP server answers GET /connector/ping with
//     200 once it listens. Imports, saves and every other connector write
//     need this, and nothing else proves it.
//
// Status.Running is true when either signal holds. Status.ConnectorReachable
// is true only when the connector answered during this check, and it is the
// field a caller that wants to import must gate on.
//
// # The transition window, and after it
//
// Measured against Zotero 7 on macOS, Zotero takes the profile lock about 3s
// after launch and creates its database WAL files about 4s after launch; the
// connector starts listening a few seconds later still. In between, the
// process is up and the connector refuses connections or does not answer:
// Status.State is "starting".
//
// "starting" lasts only for the startup window (Prober.StartupWindow, 2
// minutes by default) measured from when the lock was taken. Mozilla's lock
// open truncates the lock file, so its modification time is the lock time:
// measured on macOS, .parentlock's mtime was 3s after the Zotero process
// started, on a file created years earlier. On Windows parent.lock is
// deleted on exit and created again at launch, so its mtime is the launch
// time by construction. Past the window, a connector that still cannot take
// requests is reported as one of two states:
//
//   - "unresponsive": the connector port accepts the connection but no
//     answer arrives within the ping bound (or the answer is not a 200).
//     Zotero's main thread is blocked: open, but not responding.
//   - "connector_off": nothing listens on the connector port. The connector
//     is disabled (Settings -> Advanced -> "Allow other applications to
//     communicate with Zotero") or on another port.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"zotio/internal/zoteroprefs"
)

// LockState is what a profile lock probe observed.
type LockState string

const (
	// LockHeld means another process holds the profile lock: Zotero runs.
	LockHeld LockState = "held"
	// LockFree means the lock file exists and nobody holds it.
	LockFree LockState = "free"
	// LockAbsent means the lock file does not exist. On Windows Zotero deletes
	// it on exit; on macOS and Linux it means Zotero never ran on the profile.
	LockAbsent LockState = "absent"
	// LockError means the probe could not decide.
	LockError LockState = "error"
)

// LockProbe is the result of probing one profile directory's lock.
type LockProbe struct {
	State LockState
	// PID is the lock holder's process ID where the platform reports it
	// (fcntl F_GETLK on macOS and Linux), else 0.
	PID int
	// Since is when the lock was taken: the lock file's modification time
	// (see the package doc). Zero when the lock is not held or the time is
	// unknown.
	Since time.Time
	Err   error
}

// ProbeLock reports whether another process holds the Zotero profile lock in
// profileDir. It only queries: it never creates, truncates, or locks the file.
func ProbeLock(profileDir string) LockProbe {
	return probeLock(profileDir)
}

// State summarises the two signals.
type State string

const (
	// StateReady means the connector answered: imports can proceed.
	StateReady State = "ready"
	// StateStarting means the process holds its profile lock, the connector
	// does not answer yet, and the lock is younger than the startup window.
	StateStarting State = "starting"
	// StateUnresponsive means the lock is older than the startup window and
	// the connector port accepts connections without answering: Zotero is
	// open but not responding.
	StateUnresponsive State = "unresponsive"
	// StateConnectorOff means the lock is older than the startup window and
	// nothing listens on the connector port.
	StateConnectorOff State = "connector_off"
	// StateStopped means neither signal holds.
	StateStopped State = "stopped"
)

// Stuck reports whether Zotero is running past its startup window without a
// usable connector (StateUnresponsive or StateConnectorOff): waiting longer
// will not help until something changes, so a caller should tell the user.
func (s State) Stuck() bool { return s == StateUnresponsive || s == StateConnectorOff }

// Evidence names the strongest signal behind Status.Running.
type Evidence string

const (
	EvidenceConnector   Evidence = "connector"
	EvidenceProfileLock Evidence = "profile_lock"
	EvidenceNone        Evidence = "none"
)

// ProfileStatus is one profile directory and its lock.
type ProfileStatus struct {
	Path      string     `json:"path"`
	Lock      LockState  `json:"lock"`
	LockPID   int        `json:"lock_pid,omitempty"`
	LockSince *time.Time `json:"lock_since,omitempty"`
	LockError string     `json:"lock_error,omitempty"`
}

// Status is one presence check. Its JSON shape is the machine contract of
// `zotio desktop status`.
type Status struct {
	Running            bool            `json:"running"`
	ConnectorReachable bool            `json:"connector_reachable"`
	State              State           `json:"state"`
	Evidence           Evidence        `json:"evidence"`
	ConnectorURL       string          `json:"connector_url,omitempty"`
	ConnectorError     string          `json:"connector_error,omitempty"`
	Profiles           []ProfileStatus `json:"profiles"`
	DiscoveryError     string          `json:"discovery_error,omitempty"`
	DataDir            string          `json:"data_dir,omitempty"`
	CheckedAt          time.Time       `json:"checked_at"`
}

// LockHeld reports whether any profile lock was held.
func (s Status) LockHeld() bool {
	return slices.ContainsFunc(s.Profiles, func(p ProfileStatus) bool { return p.Lock == LockHeld })
}

// lockSince is when the newest held lock was taken, zero if no held lock has
// a known time. The newest lock is the conservative choice: a profile locked
// moments ago is still starting whatever an older lock says.
func (s Status) lockSince() time.Time {
	var newest time.Time
	for _, p := range s.Profiles {
		if p.Lock == LockHeld && p.LockSince != nil && p.LockSince.After(newest) {
			newest = *p.LockSince
		}
	}
	return newest
}

// Install is the Zotero desktop installation discovery found.
type Install struct {
	// Profiles are the profile directories to probe, preferred first.
	Profiles []string
	// DataDir is the preferred profile's data directory, "" if unresolved.
	DataDir string
	// Err explains an incomplete discovery: profiles could not be listed, or
	// a data directory could not be resolved.
	Err error
}

// WatchDirs lists the directories whose changes can mean Zotero started:
// every profile directory (the lock file) and the data directory (the
// database's WAL files).
func (in Install) WatchDirs() []string {
	dirs := slices.Clone(in.Profiles)
	if in.DataDir != "" && !slices.Contains(dirs, in.DataDir) {
		dirs = append(dirs, in.DataDir)
	}
	return dirs
}

// Discover locates Zotero's profile directories and data directory through
// zoteroprefs, the same discovery the stored-upload guard uses.
func Discover() Install {
	all, preferred, err := zoteroprefs.Profiles()
	if err != nil {
		return Install{Err: err}
	}
	in := Install{Profiles: make([]string, 0, len(all))}
	if preferred != "" {
		in.Profiles = append(in.Profiles, preferred)
	}
	for _, dir := range all {
		if dir != preferred {
			in.Profiles = append(in.Profiles, dir)
		}
	}
	if preferred != "" {
		dataDir, err := zoteroprefs.DataDir(preferred)
		if err != nil {
			in.Err = fmt.Errorf("resolving Zotero data directory: %w", err)
		} else {
			in.DataDir = dataDir
		}
	}
	return in
}

// ErrConnectorUnavailable is the Prober.Ping error when no connector can be
// addressed at all (for example a non-local base URL).
var ErrConnectorUnavailable = errors.New("desktop connector unavailable")

// DefaultPingTimeout bounds one connector ping. The connector runs on
// Zotero's main thread, so a busy Zotero can be slow to answer; a closed one
// refuses the connection at once.
const DefaultPingTimeout = 3 * time.Second

// DefaultStartupWindow is how long after taking its profile lock a Zotero
// whose connector does not answer counts as "starting". The connector
// normally listens within seconds; a database upgrade after a Zotero update
// can take longer, and is reported as unresponsive once it outlasts this.
const DefaultStartupWindow = 2 * time.Minute

// Prober checks both presence signals.
type Prober struct {
	Install Install
	// Ping asks the connector whether it accepts requests. Nil means no
	// connector can be addressed; ConnectorErr then says why.
	Ping         func(ctx context.Context) error
	ConnectorURL string
	ConnectorErr error
	// PingTimeout bounds each ping; zero means DefaultPingTimeout.
	PingTimeout time.Duration
	// StartupWindow bounds StateStarting; zero means DefaultStartupWindow.
	StartupWindow time.Duration
}

func (p *Prober) startupWindow() time.Duration {
	if p.StartupWindow > 0 {
		return p.StartupWindow
	}
	return DefaultStartupWindow
}

// Probe runs one presence check. It never fails: an undecidable signal is
// reported in the Status rather than returned as an error.
func (p *Prober) Probe(ctx context.Context) Status {
	return p.probe(ctx, time.Time{})
}

// probe runs one check. firstSeen is when the caller first saw the lock held;
// it stands in for the lock time when the platform could not report one, so a
// long-held lock of unknown age still leaves "starting" eventually.
func (p *Prober) probe(ctx context.Context, firstSeen time.Time) Status {
	st := Status{
		ConnectorURL: p.ConnectorURL,
		Profiles:     make([]ProfileStatus, 0, len(p.Install.Profiles)),
		DataDir:      p.Install.DataDir,
	}
	if p.Install.Err != nil {
		st.DiscoveryError = p.Install.Err.Error()
	}
	for _, dir := range p.Install.Profiles {
		lp := ProbeLock(dir)
		ps := ProfileStatus{Path: dir, Lock: lp.State, LockPID: lp.PID}
		if lp.State == LockHeld && !lp.Since.IsZero() {
			since := lp.Since.UTC()
			ps.LockSince = &since
		}
		if lp.Err != nil {
			ps.LockError = lp.Err.Error()
		}
		st.Profiles = append(st.Profiles, ps)
	}

	var pingErr error
	if p.Ping == nil {
		pingErr = p.ConnectorErr
		if pingErr == nil {
			pingErr = ErrConnectorUnavailable
		}
	} else {
		timeout := p.PingTimeout
		if timeout <= 0 {
			timeout = DefaultPingTimeout
		}
		pingCtx, cancel := context.WithTimeout(ctx, timeout)
		pingErr = p.Ping(pingCtx)
		cancel()
	}
	if pingErr != nil {
		st.ConnectorError = pingErr.Error()
	} else {
		st.ConnectorReachable = true
	}

	now := time.Now()
	held := st.LockHeld()
	st.Running = held || st.ConnectorReachable
	switch {
	case st.ConnectorReachable:
		st.State, st.Evidence = StateReady, EvidenceConnector
	case held:
		st.Evidence = EvidenceProfileLock
		since := st.lockSince()
		if since.IsZero() {
			since = firstSeen
		}
		switch {
		case since.IsZero() || now.Sub(since) < p.startupWindow():
			st.State = StateStarting
		case p.Ping == nil || connectorRefused(pingErr):
			// No connector zotio can address counts as none listening.
			st.State = StateConnectorOff
		default:
			st.State = StateUnresponsive
		}
	default:
		st.State, st.Evidence = StateStopped, EvidenceNone
	}
	st.CheckedAt = now.UTC()
	return st
}

// startingUntil is when a "starting" status stops being one.
func (p *Prober) startingUntil(st Status, firstSeen time.Time) time.Time {
	since := st.lockSince()
	if since.IsZero() {
		since = firstSeen
	}
	return since.Add(p.startupWindow())
}

// connectorRefused reports whether a ping failed because nothing listens on
// the port: the dial itself failed. A timeout, including a dial timeout, is
// the opposite case: something holds the port and does not answer.
func connectorRefused(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return false
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}
