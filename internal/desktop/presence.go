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
// # The transition window
//
// Measured against Zotero 7 on macOS, Zotero takes the profile lock about 3s
// after launch and creates its database WAL files about 4s after launch; the
// connector starts listening a few seconds later still. In between, the
// process is up and the connector refuses connections: Status.State is
// "starting". The same state persists if the connector is disabled, bound to
// another port, or Zotero is hung, so "starting" means "process up, connector
// not answering", not a promise that it will answer.
package desktop

import (
	"context"
	"errors"
	"fmt"
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
	Err error
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
	// StateStarting means the process holds its profile lock but the
	// connector did not answer (see the package doc's transition window).
	StateStarting State = "starting"
	// StateStopped means neither signal holds.
	StateStopped State = "stopped"
)

// Evidence names the strongest signal behind Status.Running.
type Evidence string

const (
	EvidenceConnector   Evidence = "connector"
	EvidenceProfileLock Evidence = "profile_lock"
	EvidenceNone        Evidence = "none"
)

// ProfileStatus is one profile directory and its lock.
type ProfileStatus struct {
	Path      string    `json:"path"`
	Lock      LockState `json:"lock"`
	LockPID   int       `json:"lock_pid,omitempty"`
	LockError string    `json:"lock_error,omitempty"`
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
}

// Probe runs one presence check. It never fails: an undecidable signal is
// reported in the Status rather than returned as an error.
func (p *Prober) Probe(ctx context.Context) Status {
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
		if lp.Err != nil {
			ps.LockError = lp.Err.Error()
		}
		st.Profiles = append(st.Profiles, ps)
	}

	if p.Ping == nil {
		err := p.ConnectorErr
		if err == nil {
			err = ErrConnectorUnavailable
		}
		st.ConnectorError = err.Error()
	} else {
		timeout := p.PingTimeout
		if timeout <= 0 {
			timeout = DefaultPingTimeout
		}
		pingCtx, cancel := context.WithTimeout(ctx, timeout)
		err := p.Ping(pingCtx)
		cancel()
		if err != nil {
			st.ConnectorError = err.Error()
		} else {
			st.ConnectorReachable = true
		}
	}

	held := st.LockHeld()
	st.Running = held || st.ConnectorReachable
	switch {
	case st.ConnectorReachable:
		st.State, st.Evidence = StateReady, EvidenceConnector
	case held:
		st.State, st.Evidence = StateStarting, EvidenceProfileLock
	default:
		st.State, st.Evidence = StateStopped, EvidenceNone
	}
	st.CheckedAt = time.Now().UTC()
	return st
}
