// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// journal build + append/list/read roundtrip.

package mutation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func appliedEnvelope() Envelope {
	return Envelope{
		SchemaVersion: SchemaVersion,
		OK:            true,
		Operation:     "items.tags.add",
		Mode:          "apply",
		Plan: Plan{
			Operations: []Op{
				{ID: "op1", Key: "K1", Kind: "tag_add", Changes: []Change{{Field: "tags", Add: "ml"}}},
				{ID: "op2", Key: "K2", Kind: "tag_add"}, // no-op
			},
		},
		Result: &Result{
			Summary: ResultSummary{Attempted: 2, Applied: 1, NoOp: 1},
			Items: []ResultItem{
				{OpID: "op1", Key: "K1", Status: "applied"},
				{OpID: "op2", Key: "K2", Status: "no_op"},
			},
		},
	}
}

func TestBuildJournalEntrySkipsPreview(t *testing.T) {
	preview := Envelope{Operation: "x", Mode: "preview"} // no Result
	if _, ok := BuildJournalEntry(preview, time.Now()); ok {
		t.Fatal("preview envelope should not produce a journal entry")
	}
}

func TestBuildJournalEntryJoinsStatus(t *testing.T) {
	e, ok := BuildJournalEntry(appliedEnvelope(), time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("apply envelope should build an entry")
	}
	if e.RunID == "" || e.Operation != "items.tags.add" || !e.OK {
		t.Fatalf("entry header = %+v", e)
	}
	if e.Library != "user" {
		t.Fatalf("entry library = %q, want user", e.Library)
	}
	if len(e.Ops) != 2 {
		t.Fatalf("ops = %d, want 2", len(e.Ops))
	}
	if e.Ops[0].Status != "applied" || e.Ops[0].Kind != "tag_add" || len(e.Ops[0].Changes) != 1 {
		t.Errorf("op1 = %+v, want applied tag_add with one change", e.Ops[0])
	}
	if e.Ops[1].Status != "no_op" {
		t.Errorf("op2 status = %q, want no_op", e.Ops[1].Status)
	}
}

func TestJournalWriteListReadRoundtrip(t *testing.T) {
	dir := t.TempDir()

	// Empty journal: list returns nothing, read errors.
	if entries, err := ListEntries(dir); err != nil || len(entries) != 0 {
		t.Fatalf("empty list = (%v, %v), want ([], nil)", entries, err)
	}

	e1, _ := BuildJournalEntry(appliedEnvelope(), time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC))
	e1.RunID = "run-1"
	e2, _ := BuildJournalEntry(appliedEnvelope(), time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC))
	e2.RunID = "run-2"
	for _, e := range []JournalEntry{e1, e2} {
		if err := WriteEntry(dir, e); err != nil {
			t.Fatalf("write %s: %v", e.RunID, err)
		}
	}

	entries, err := ListEntries(dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 || entries[0].RunID != "run-2" {
		t.Fatalf("list = %+v, want newest-first [run-2, run-1]", entries)
	}

	got, err := ReadEntry(dir, "run-1")
	if err != nil || got.Operation != "items.tags.add" || len(got.Ops) != 2 {
		t.Fatalf("read run-1 = (%+v, %v)", got, err)
	}
	if _, err := ReadEntry(dir, "missing"); err == nil {
		t.Error("read missing run id should error")
	}
}

func TestWriteEntryPrivateFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not portable on windows")
	}
	dir := filepath.Join(t.TempDir(), "journal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod dir setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, JournalFileName), nil, 0o644); err != nil { // #nosec G306 -- deliberately lax pre-existing file; the test asserts WriteEntry re-chmods it to 0600
		t.Fatalf("write journal setup: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, JournalFileName), 0o644); err != nil {
		t.Fatalf("chmod journal setup: %v", err)
	}
	e, _ := BuildJournalEntry(appliedEnvelope(), time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC))
	if err := WriteEntry(dir, e); err != nil {
		t.Fatalf("write: %v", err)
	}

	assertMode(t, dir, 0o700)
	assertMode(t, filepath.Join(dir, JournalFileName), 0o600)
}

func TestWriteEntryReturnsJournalPathFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, JournalFileName), 0o700); err != nil {
		t.Fatalf("creating journal path directory: %v", err)
	}
	entry, _ := BuildJournalEntry(appliedEnvelope(), time.Now())
	if err := WriteEntry(dir, entry); err == nil {
		t.Fatal("WriteEntry succeeded with a directory at the journal path")
	}
}

func TestListEntriesRetriesPartialFinalAppend(t *testing.T) {
	dir := t.TempDir()
	first, _ := BuildJournalEntry(appliedEnvelope(), time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC))
	first.RunID = "run-1"
	if err := WriteEntry(dir, first); err != nil {
		t.Fatalf("write first entry: %v", err)
	}
	second, _ := BuildJournalEntry(appliedEnvelope(), time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC))
	second.RunID = "run-2"
	line, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second entry: %v", err)
	}

	partialWritten := make(chan struct{})
	finishWrite := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		f, err := os.OpenFile(filepath.Join(dir, JournalFileName), os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Errorf("open partial writer: %v", err)
			return
		}
		defer f.Close()
		if _, err := f.Write(line[:len(line)/2]); err != nil {
			t.Errorf("write partial record: %v", err)
			return
		}
		if err := f.Sync(); err != nil {
			t.Errorf("sync partial record: %v", err)
			return
		}
		close(partialWritten)
		<-finishWrite
		if _, err := f.Write(append(line[len(line)/2:], '\n')); err != nil {
			t.Errorf("complete partial record: %v", err)
			return
		}
		if err := f.Sync(); err != nil {
			t.Errorf("sync complete record: %v", err)
		}
	}()
	<-partialWritten

	originalBackoff := journalListRetryBackoff
	retryObserved := make(chan struct{})
	journalListRetryBackoff = func() {
		close(retryObserved)
		<-writerDone
	}
	t.Cleanup(func() {
		journalListRetryBackoff = originalBackoff
	})

	type listResult struct {
		entries []JournalEntry
		err     error
	}
	listDone := make(chan listResult, 1)
	go func() {
		entries, err := ListEntries(dir)
		listDone <- listResult{entries: entries, err: err}
	}()

	select {
	case <-retryObserved:
	case <-time.After(time.Second):
		t.Fatal("ListEntries did not retry the partial final record")
	}
	close(finishWrite)

	select {
	case result := <-listDone:
		if result.err != nil {
			t.Fatalf("list after append completed: %v", result.err)
		}
		if len(result.entries) != 2 || result.entries[0].RunID != "run-2" || result.entries[1].RunID != "run-1" {
			t.Fatalf("entries = %+v, want newest-first [run-2, run-1]", result.entries)
		}
	case <-time.After(time.Second):
		t.Fatal("ListEntries did not finish after append completed")
	}
}

func TestListEntriesIgnoresPersistentIncompleteFinalRecord(t *testing.T) {
	dir := t.TempDir()
	entry, _ := BuildJournalEntry(appliedEnvelope(), time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC))
	entry.RunID = "run-1"
	if err := WriteEntry(dir, entry); err != nil {
		t.Fatalf("write entry: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, JournalFileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open incomplete journal: %v", err)
	}
	if _, err := f.WriteString(`{"run_id":`); err != nil {
		_ = f.Close()
		t.Fatalf("write incomplete journal: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close incomplete journal: %v", err)
	}

	entries, err := ListEntries(dir)
	if err != nil {
		t.Fatalf("list with persistent incomplete final record: %v", err)
	}
	if len(entries) != 1 || entries[0].RunID != "run-1" {
		t.Fatalf("entries = %+v, want prior valid entry only", entries)
	}
}

func TestListEntriesRejectsMalformedMiddleRecord(t *testing.T) {
	dir := t.TempDir()
	first, _ := BuildJournalEntry(appliedEnvelope(), time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC))
	first.RunID = "run-1"
	second, _ := BuildJournalEntry(appliedEnvelope(), time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC))
	second.RunID = "run-2"
	firstLine, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first entry: %v", err)
	}
	secondLine, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second entry: %v", err)
	}
	journal := append(firstLine, '\n')
	journal = append(journal, []byte(`{"run_id": not-json}`+"\n")...)
	journal = append(journal, secondLine...)
	journal = append(journal, '\n')
	if err := os.WriteFile(filepath.Join(dir, JournalFileName), journal, 0o600); err != nil {
		t.Fatalf("write malformed journal: %v", err)
	}

	if _, err := ListEntries(dir); err == nil || !strings.Contains(err.Error(), "parsing journal entry") {
		t.Fatalf("list malformed middle record error = %v, want parse error", err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode() & os.ModePerm; got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}
