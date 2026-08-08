// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// append-only run journal. Every applied mutation
// run is recorded as one JSON line so it can be listed, inspected, and (where
// reversible) undone. Pure model + file I/O; the cli resolves the directory.

package mutation

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// JournalEntry records one applied mutation run. WorkflowRunID groups the
// entries applied under one transactional `workflow run` approval.
type JournalEntry struct {
	SchemaVersion int           `json:"schema_version"`
	RunID         string        `json:"run_id"`
	WorkflowRunID string        `json:"workflow_run_id,omitempty"`
	Operation     string        `json:"operation"`
	Library       string        `json:"library"`
	Mode          string        `json:"mode"`
	Timestamp     time.Time     `json:"timestamp"`
	OK            bool          `json:"ok"`
	Summary       ResultSummary `json:"summary"`
	Ops           []JournalOp   `json:"ops"`
}

// IncompleteJournalError reports that a final unterminated journal record could
// not be decoded after bounded retries. Entries preceding it are valid, but the
// journal cannot establish whether the missing record contains a requested run.
type IncompleteJournalError struct {
	Path string
	Err  error
}

func (e *IncompleteJournalError) Error() string {
	return fmt.Sprintf("journal %s has an incomplete final record: %v", e.Path, e.Err)
}

func (e *IncompleteJournalError) Unwrap() error { return e.Err }

// JournalEntryNotFoundError reports a complete journal lookup with no matching
// run. It lets callers distinguish a definite absence from an incomplete tail.
type JournalEntryNotFoundError struct {
	RunID string
}

func (e *JournalEntryNotFoundError) Error() string {
	return fmt.Sprintf("no journal entry with run id %q", e.RunID)
}

// BuildJournalEntry builds an entry from an applied envelope, joining each plan
// operation with its result status and post-write key. It returns ok=false when
// the envelope is not an apply (no Result) so callers can skip recording previews.
func BuildJournalEntry(env Envelope, now time.Time) (JournalEntry, bool) {
	if env.Result == nil {
		return JournalEntry{}, false
	}
	results := make(map[string]ResultItem, len(env.Result.Items))
	for _, item := range env.Result.Items {
		results[item.OpID] = item
	}
	ops := make([]JournalOp, 0, len(env.Plan.Operations))
	for _, op := range env.Plan.Operations {
		item := results[op.ID]
		key := op.Key
		// Creates do not know their key at plan time. Run adopts the key
		// returned by Apply into ResultItem.Key; use that applied key in the
		// journal so undo targets the object that was actually created. If the
		// route could not confirm a Zotero key (for example, a connector
		// correlation ID), keep it empty rather than recording the item type.
		if op.Kind == "item_create" {
			key = item.Key
		} else if item.Key != "" {
			key = item.Key
		}
		ops = append(ops, JournalOp{
			ID:          op.ID,
			Key:         key,
			Kind:        op.Kind,
			Status:      item.Status,
			Destructive: op.Destructive,
			Changes:     op.Changes,
		})
	}
	return JournalEntry{
		SchemaVersion: JournalSchemaVersion,
		RunID:         NewRunID(now),
		Library:       "user",
		Operation:     env.Operation,
		Mode:          env.Mode,
		Timestamp:     now.UTC(),
		OK:            env.OK,
		Summary:       env.Result.Summary,
		Ops:           ops,
	}, true
}

// NewRunID mints a journal run identifier: a UTC second timestamp plus a random
// suffix. Exported so `workflow run` can mint one transaction-level id shared
// by every step entry.
func NewRunID(now time.Time) string {
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating journal dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("securing journal dir: %w", err)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encoding journal entry: %w", err)
	}
	journalPath := filepath.Join(dir, JournalFileName)
	f, err := os.OpenFile(journalPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("opening journal: %w", err)
	}
	if err := os.Chmod(journalPath, 0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("securing journal: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing journal entry: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("syncing journal entry: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing journal entry: %w", err)
	}
	return nil
}

// ListEntries reads every recorded run, newest first. A missing journal is not
// an error: it returns an empty slice.
//
// Writers append JSON records while readers remain concurrent. If the final,
// unterminated record cannot be decoded after bounded retries, it is treated as
// an in-progress or crash-torn append and omitted; preceding valid records are
// returned with an IncompleteJournalError. Any malformed completed record, or
// malformed record followed by another record, is corruption and returns an
// error without entries.
func ListEntries(dir string) ([]JournalEntry, error) {
	path := filepath.Join(dir, JournalFileName)
	for attempt := range journalListReadAttempts {
		snapshot, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}

		entries, finalDecodeFailure, finalIncomplete, err := parseJournalSnapshot(snapshot)
		if err == nil {
			return newestFirst(entries), nil
		}
		if !finalDecodeFailure {
			return nil, err
		}
		if attempt+1 < journalListReadAttempts {
			journalListRetryBackoff()
			continue
		}
		if finalIncomplete {
			return newestFirst(entries), &IncompleteJournalError{Path: path, Err: err}
		}
		return nil, err
	}
	panic("unreachable")
}

const journalListReadAttempts = 3

var journalListRetryBackoff = func() {
	time.Sleep(5 * time.Millisecond)
}

// parseJournalSnapshot distinguishes a potentially in-progress final record
// from corruption in every earlier record. Empty lines retain their historical
// meaning and are ignored.
func parseJournalSnapshot(snapshot []byte) ([]JournalEntry, bool, bool, error) {
	lines := bytes.Split(snapshot, []byte{'\n'})
	lastNonEmpty := -1
	for i, line := range lines {
		if len(line) != 0 {
			lastNonEmpty = i
		}
	}
	finalIncomplete := len(snapshot) != 0 && snapshot[len(snapshot)-1] != '\n'

	entries := make([]JournalEntry, 0, len(lines))
	for i, line := range lines {
		if len(line) == 0 {
			continue
		}
		var entry JournalEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			parseErr := fmt.Errorf("parsing journal entry: %w", err)
			if i == lastNonEmpty {
				return entries, true, finalIncomplete, parseErr
			}
			return nil, false, false, parseErr
		}
		entries = append(entries, entry)
	}
	return entries, false, false, nil
}

func newestFirst(entries []JournalEntry) []JournalEntry {
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries
}

// ReadEntry returns the recorded run with the given id, or an error if absent.
// When the journal has an incomplete final record, a matching complete entry is
// still safe to return; an absent run is ambiguous and returns that error.
func ReadEntry(dir, runID string) (JournalEntry, error) {
	entries, err := ListEntries(dir)
	var incomplete *IncompleteJournalError
	if err != nil && !errors.As(err, &incomplete) {
		return JournalEntry{}, err
	}
	for _, e := range entries {
		if e.RunID == runID {
			return e, nil
		}
	}
	if incomplete != nil {
		return JournalEntry{}, incomplete
	}
	return JournalEntry{}, &JournalEntryNotFoundError{RunID: runID}
}
