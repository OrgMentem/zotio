// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase3): append-only run journal. Every applied mutation
// run is recorded as one JSON line so it can be listed, inspected, and (where
// reversible) undone. Pure model + file I/O; the cli resolves the directory.

package mutation

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// JournalSchemaVersion versions the on-disk journal-entry format.
const JournalSchemaVersion = 1

// JournalFileName is the append-only log within the journal directory.
const JournalFileName = "journal.jsonl"

// JournalOp is one operation as recorded in the journal, carrying the applied
// status and the field-level changes needed to describe (and later reverse) it.
type JournalOp struct {
	ID          string   `json:"id"`
	Key         string   `json:"key"`
	Kind        string   `json:"kind"`
	Status      string   `json:"status"`
	Destructive bool     `json:"destructive,omitempty"`
	Changes     []Change `json:"changes"`
}

// JournalEntry records one applied mutation run.
type JournalEntry struct {
	SchemaVersion int           `json:"schema_version"`
	RunID         string        `json:"run_id"`
	Operation     string        `json:"operation"`
	Mode          string        `json:"mode"`
	Timestamp     time.Time     `json:"timestamp"`
	OK            bool          `json:"ok"`
	Summary       ResultSummary `json:"summary"`
	Ops           []JournalOp   `json:"ops"`
}

// BuildJournalEntry builds an entry from an applied envelope, joining each plan
// operation with its result status. It returns ok=false when the envelope is not
// an apply (no Result) so callers can skip recording previews.
func BuildJournalEntry(env Envelope, now time.Time) (JournalEntry, bool) {
	if env.Result == nil {
		return JournalEntry{}, false
	}
	status := make(map[string]string, len(env.Result.Items))
	for _, item := range env.Result.Items {
		status[item.OpID] = item.Status
	}
	ops := make([]JournalOp, 0, len(env.Plan.Operations))
	for _, op := range env.Plan.Operations {
		ops = append(ops, JournalOp{
			ID:          op.ID,
			Key:         op.Key,
			Kind:        op.Kind,
			Status:      status[op.ID],
			Destructive: op.Destructive,
			Changes:     op.Changes,
		})
	}
	return JournalEntry{
		SchemaVersion: JournalSchemaVersion,
		RunID:         newRunID(now),
		Operation:     env.Operation,
		Mode:          env.Mode,
		Timestamp:     now.UTC(),
		OK:            env.OK,
		Summary:       env.Result.Summary,
		Ops:           ops,
	}, true
}

func newRunID(now time.Time) string {
	var b [4]byte
	suffix := "0000"
	if _, err := rand.Read(b[:]); err == nil {
		suffix = hex.EncodeToString(b[:])
	}
	return now.UTC().Format("20060102T150405Z") + "-" + suffix
}

// WriteEntry appends the entry as one JSON line to <dir>/journal.jsonl, creating
// the directory if needed.
func WriteEntry(dir string, e JournalEntry) error {
	if dir == "" {
		return fmt.Errorf("empty journal directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating journal dir: %w", err)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encoding journal entry: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, JournalFileName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening journal: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("writing journal entry: %w", err)
	}
	return nil
}

// ListEntries reads every recorded run, newest first. A missing journal is not
// an error: it returns an empty slice.
func ListEntries(dir string) ([]JournalEntry, error) {
	f, err := os.Open(filepath.Join(dir, JournalFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	entries := make([]JournalEntry, 0)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e JournalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("parsing journal entry: %w", err)
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	// Newest first.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries, nil
}

// ReadEntry returns the recorded run with the given id, or an error if absent.
func ReadEntry(dir, runID string) (JournalEntry, error) {
	entries, err := ListEntries(dir)
	if err != nil {
		return JournalEntry{}, err
	}
	for _, e := range entries {
		if e.RunID == runID {
			return e, nil
		}
	}
	return JournalEntry{}, fmt.Errorf("no journal entry with run id %q", runID)
}
