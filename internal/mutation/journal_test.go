// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// journal build + append/list/read roundtrip.

package mutation

import (
	"encoding/json"
	"errors"
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

// Revert is read after Apply and only for a write that landed: a failed or
// conflicting op wrote nothing, so a recorded inverse would undo nothing.
// The record also has to survive the journal file, including an empty prior
// value (Add: ""), which is what restores a field that was blank.
func TestBuildJournalEntryRecordsRevertOnlyForAppliedOps(t *testing.T) {
	revert := []Change{
		{Field: "DOI", Add: "", Remove: "10.1/x"},
		{Field: "extra_line", Remove: "zotio: DOI added via CrossRef on 2026-10-12"},
	}
	called := map[string]int{}
	recorder := func(id string) func() []Change {
		return func() []Change {
			called[id]++
			return revert
		}
	}
	changes := []Change{{Field: "DOI", Add: "10.1/x"}}
	env := Envelope{
		Operation: "items.enrich",
		Mode:      "apply",
		Plan: Plan{Operations: []Op{
			{ID: "applied", Key: "K1", Kind: "missing_doi", Changes: changes, Revert: recorder("applied")},
			{ID: "failed", Key: "K2", Kind: "missing_doi", Changes: changes, Revert: recorder("failed")},
			{ID: "plain", Key: "K3", Kind: "tag_add", Changes: []Change{{Field: "tags", Add: "ml"}}},
		}},
		Result: &Result{
			Summary: ResultSummary{Attempted: 3, Applied: 2, Failed: 1},
			Items: []ResultItem{
				{OpID: "applied", Key: "K1", Status: "applied"},
				{OpID: "failed", Key: "K2", Status: "failed"},
				{OpID: "plain", Key: "K3", Status: "applied"},
			},
		},
	}
	entry, ok := BuildJournalEntry(env, time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("apply envelope should build an entry")
	}
	if called["failed"] != 0 || called["applied"] != 1 {
		t.Fatalf("recorder calls = %v, want only the applied op's recorder, once", called)
	}
	if len(entry.Ops[0].Revert) != 2 || len(entry.Ops[1].Revert) != 0 || len(entry.Ops[2].Revert) != 0 {
		t.Fatalf("revert records = %+v, want one for the applied op only", entry.Ops)
	}

	dir := t.TempDir()
	if err := WriteEntry(dir, entry); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadEntry(dir, entry.RunID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Ops[0].Revert) != 2 || got.Ops[0].Revert[0] != revert[0] || got.Ops[0].Revert[1] != revert[1] {
		t.Fatalf("round-tripped revert = %+v, want %+v (an empty prior value must survive)", got.Ops[0].Revert, revert)
	}
}
func TestBuildJournalEntryUsesAppliedResultKey(t *testing.T) {
	env := Envelope{
		Operation: "items.new",
		Mode:      "apply",
		Plan: Plan{Operations: []Op{{
			ID: "create", Key: "journalArticle", Kind: "item_create",
			Changes: []Change{{Field: "item", Add: map[string]any{"itemType": "journalArticle"}}},
		}}},
		Result: &Result{
			Summary: ResultSummary{Attempted: 1, Applied: 1},
			Items:   []ResultItem{{OpID: "create", Key: "REALKEY1", Status: "applied"}},
		},
	}
	entry, ok := BuildJournalEntry(env, time.Now())
	if !ok {
		t.Fatal("applied envelope should build a journal entry")
	}
	if got := entry.Ops[0].Key; got != "REALKEY1" {
		t.Fatalf("journal key = %q, want applied key REALKEY1 (not item type)", got)
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

func TestListEntriesReportsPersistentIncompleteFinalRecord(t *testing.T) {
	dir := t.TempDir()
	entry, _ := BuildJournalEntry(appliedEnvelope(), time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC))
	entry.RunID = "run-1"
	if err := WriteEntry(dir, entry); err != nil {
		t.Fatalf("write entry: %v", err)
	}
	journalPath := filepath.Join(dir, JournalFileName)
	f, err := os.OpenFile(journalPath, os.O_APPEND|os.O_WRONLY, 0o600)
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
	var incomplete *IncompleteJournalError
	if !errors.As(err, &incomplete) {
		t.Fatalf("list error = %v, want IncompleteJournalError", err)
	}
	if incomplete.Path != journalPath {
		t.Errorf("incomplete journal path = %q, want %q", incomplete.Path, journalPath)
	}
	if len(entries) != 1 || entries[0].RunID != "run-1" {
		t.Fatalf("entries = %+v, want prior valid entry only", entries)
	}

	found, err := ReadEntry(dir, "run-1")
	if err != nil || found.RunID != "run-1" {
		t.Fatalf("read valid prefix = (%+v, %v), want run-1", found, err)
	}
	if _, err := ReadEntry(dir, "missing"); !errors.As(err, &incomplete) {
		t.Fatalf("read missing with incomplete tail error = %v, want IncompleteJournalError", err)
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

// A kill during append leaves a torn final record. The next WriteEntry must
// quarantine the fragment (preserving its bytes for diagnosis) instead of
// appending behind it, which used to fuse both records onto one line and make
// the whole journal unreadable.
func TestWriteEntryQuarantinesTornTailAndPreservesPrefix(t *testing.T) {
	dir := t.TempDir()
	first, _ := BuildJournalEntry(appliedEnvelope(), time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC))
	first.RunID = "run-1"
	if err := WriteEntry(dir, first); err != nil {
		t.Fatalf("write first entry: %v", err)
	}
	journalPath := filepath.Join(dir, JournalFileName)
	torn := []byte(`{"run_id":`)
	f, err := os.OpenFile(journalPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	if _, err := f.Write(torn); err != nil {
		_ = f.Close()
		t.Fatalf("write torn tail: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	second, _ := BuildJournalEntry(appliedEnvelope(), time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC))
	second.RunID = "run-2"
	if err := WriteEntry(dir, second); err != nil {
		t.Fatalf("write after torn tail: %v", err)
	}

	entries, err := ListEntries(dir)
	if err != nil {
		t.Fatalf("list after repair: %v", err)
	}
	if len(entries) != 2 || entries[0].RunID != "run-2" || entries[1].RunID != "run-1" {
		t.Fatalf("entries = %+v, want newest-first [run-2, run-1]", entries)
	}

	matches, err := filepath.Glob(journalPath + ".torn-*")
	if err != nil {
		t.Fatalf("glob sidecars: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("torn sidecars = %v, want exactly one", matches)
	}
	preserved, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if string(preserved) != string(torn) {
		t.Fatalf("sidecar = %q, want torn bytes %q", preserved, torn)
	}
	assertMode(t, matches[0], 0o600)
}

// A tail that decodes is a complete record whose terminator was lost, not a
// torn fragment: the next append terminates it in place instead of discarding
// the recorded run.
func TestWriteEntryTerminatesCompleteUnterminatedTail(t *testing.T) {
	dir := t.TempDir()
	first, _ := BuildJournalEntry(appliedEnvelope(), time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC))
	first.RunID = "run-1"
	if err := WriteEntry(dir, first); err != nil {
		t.Fatalf("write first entry: %v", err)
	}
	complete, _ := BuildJournalEntry(appliedEnvelope(), time.Date(2026, 6, 28, 10, 0, 0, 0, time.UTC))
	complete.RunID = "run-2"
	line, err := json.Marshal(complete)
	if err != nil {
		t.Fatalf("marshal second entry: %v", err)
	}
	journalPath := filepath.Join(dir, JournalFileName)
	f, err := os.OpenFile(journalPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		t.Fatalf("write unterminated record: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	third, _ := BuildJournalEntry(appliedEnvelope(), time.Date(2026, 6, 28, 11, 0, 0, 0, time.UTC))
	third.RunID = "run-3"
	if err := WriteEntry(dir, third); err != nil {
		t.Fatalf("write after unterminated record: %v", err)
	}

	entries, err := ListEntries(dir)
	if err != nil {
		t.Fatalf("list after repair: %v", err)
	}
	if len(entries) != 3 || entries[0].RunID != "run-3" || entries[1].RunID != "run-2" || entries[2].RunID != "run-1" {
		t.Fatalf("entries = %+v, want newest-first [run-3, run-2, run-1]", entries)
	}
	if matches, _ := filepath.Glob(journalPath + ".torn-*"); len(matches) != 0 {
		t.Fatalf("torn sidecars = %v, want none for a complete record", matches)
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

func TestNewRunIDFallbackIsNonConstant(t *testing.T) {
	orig := journalRandRead
	journalRandRead = func(b []byte) (int, error) { return 0, errors.New("entropy unavailable") }
	t.Cleanup(func() { journalRandRead = orig })

	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	id1 := NewRunID(now)
	id2 := NewRunID(now)
	if id1 == id2 {
		t.Fatalf("fallback RunIDs collided: %q == %q", id1, id2)
	}
	if strings.HasSuffix(id1, "-0000") || strings.HasSuffix(id2, "-0000") {
		t.Fatalf("fallback RunID must not be deterministic 0000: %q %q", id1, id2)
	}
	if !strings.HasPrefix(id1, "20260817T120000Z-") || !strings.HasPrefix(id2, "20260817T120000Z-") {
		t.Fatalf("RunID timestamp prefix wrong: %q %q", id1, id2)
	}
}

func TestNewRunIDSuccessPathUnchanged(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	id := NewRunID(now)
	if !strings.HasPrefix(id, "20260817T120000Z-") {
		t.Fatalf("RunID = %q, want prefix 20260817T120000Z-", id)
	}
	suffix := strings.TrimPrefix(id, "20260817T120000Z-")
	if len(suffix) != 8 {
		t.Fatalf("success suffix length = %d, want 8 (4 random bytes hex); id=%q", len(suffix), id)
	}
}

func checkpointRecord(t *testing.T, runID, opID, key, status string, reason any) JournalEntry {
	t.Helper()
	entry, ok := BuildJournalCheckpoint(runID, "items.tags.add", "apply",
		Op{ID: opID, Key: key, Kind: "tag_add", Changes: []Change{{Field: "tags", Add: "ml"}}},
		ResultItem{OpID: opID, Key: key, Status: status, Reason: reason}, time.Unix(1700000000, 0))
	if !ok {
		t.Fatalf("BuildJournalCheckpoint(%s %s) ok = false, want a record", opID, status)
	}
	return entry
}

// An op that left nothing behind has no op-level record.
func TestBuildJournalCheckpointSkipsOpsThatLeftNothing(t *testing.T) {
	for _, item := range []ResultItem{
		{OpID: "op1", Status: "failed", Reason: "boom"},
		{OpID: "op1", Status: "conflict", Reason: "stale"},
		{OpID: "op1", Status: "skipped"},
	} {
		if entry, ok := BuildJournalCheckpoint("run", "x", "apply", Op{ID: "op1"}, item, time.Now()); ok {
			t.Fatalf("BuildJournalCheckpoint(%s) = %+v, want no record", item.Status, entry)
		}
	}
	if _, ok := BuildJournalCheckpoint("run", "x", "apply", Op{ID: "op1"}, ResultItem{OpID: "op1", Status: "conflict", Reason: map[string]any{"committed": true}}, time.Now()); !ok {
		t.Fatal("a committed conflict wrote something, so it must keep its op-level record")
	}
}

// A run killed before its final record shows the merge of its op-level records;
// a run that finished shows only its final record; other runs are untouched.
func TestListEntriesFoldsOpLevelRecords(t *testing.T) {
	dir := t.TempDir()
	killed := "20231114T221320Z-killed00"
	finished := "20231114T221321Z-finish00"
	for _, entry := range []JournalEntry{
		checkpointRecord(t, killed, "op1", "K1", "applied", nil),
		checkpointRecord(t, finished, "op1", "F1", "applied", nil),
		checkpointRecord(t, killed, "op2", "K2", "applied", nil),
		checkpointRecord(t, finished, "op2", "F2", "applied", nil),
		{SchemaVersion: JournalSchemaVersion, RunID: finished, Operation: "items.tags.add", Mode: "apply", OK: true,
			Summary: ResultSummary{Attempted: 2, Applied: 2},
			Ops:     []JournalOp{{ID: "op1", Key: "F1", Status: "applied"}, {ID: "op2", Key: "F2", Status: "applied"}}},
	} {
		if err := WriteEntry(dir, entry); err != nil {
			t.Fatalf("WriteEntry: %v", err)
		}
	}
	entries, err := ListEntries(dir)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ListEntries = %d runs, want 2 (killed, finished): %+v", len(entries), entries)
	}
	done, cut := entries[0], entries[1]
	if done.RunID != finished || done.Partial || !done.OK || len(done.Ops) != 2 {
		t.Fatalf("finished run = %+v, want its final record alone", done)
	}
	if cut.RunID != killed || !cut.Partial || cut.OK || cut.Summary.Applied != 2 || cut.Summary.Attempted != 2 {
		t.Fatalf("killed run = %+v, want a partial run counting its two landed ops", cut)
	}
	if len(cut.Ops) != 2 || cut.Ops[0].Key != "K1" || cut.Ops[1].Key != "K2" {
		t.Fatalf("killed run ops = %+v, want K1 then K2 in write order", cut.Ops)
	}
	got, err := ReadEntry(dir, killed)
	if err != nil || len(got.Ops) != 2 {
		t.Fatalf("ReadEntry(killed) = %+v, %v, want the merged run", got, err)
	}
}

// RunObserved reports each attempted op, in order, before the next one starts.
func TestRunObservedCallsBackAfterEachAttemptedOp(t *testing.T) {
	var seen []string
	apply := func(status string) func() (string, any, error) {
		return func() (string, any, error) {
			seen = append(seen, "apply:"+status)
			return status, nil, nil
		}
	}
	ops := []Op{
		{ID: "a", Key: "A", Changes: []Change{{Field: "x"}}, Apply: apply("applied")},
		{ID: "n", Key: "N"}, // no-op: nothing attempted, nothing reported
		{ID: "f", Key: "F", Changes: []Change{{Field: "x"}}, Apply: apply("failed")},
		{ID: "z", Key: "Z", Changes: []Change{{Field: "x"}}, Apply: apply("applied")},
	}
	_, _ = RunObserved(Options{Yes: true, MaxChanges: -1, RunID: "run-1"}, "t", ops, func(op Op, item ResultItem) {
		seen = append(seen, "outcome:"+op.ID+"="+item.Status)
	})
	want := "apply:applied,outcome:a=applied,apply:failed,outcome:f=failed"
	if got := strings.Join(seen, ","); got != want {
		t.Fatalf("sequence = %q, want %q (fail-fast leaves z not attempted)", got, want)
	}
}
