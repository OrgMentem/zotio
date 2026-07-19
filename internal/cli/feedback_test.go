// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestFeedbackListReportsCorruptJournalLines(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path, err := feedbackFilePath()
	if err != nil {
		t.Fatalf("feedbackFilePath: %v", err)
	}
	valid := FeedbackEntry{
		Text:      "useful feedback",
		CLI:       "zotio",
		Version:   "test",
		Timestamp: time.Date(2026, time.July, 20, 0, 0, 0, 0, time.UTC),
	}
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatalf("marshal feedback: %v", err)
	}
	if err := os.WriteFile(path, append(append(encoded, '\n'), []byte(`{"text":`)...), 0o600); err != nil {
		t.Fatalf("write feedback ledger: %v", err)
	}

	cmd := newFeedbackListCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err = cmd.Execute()
	if code := ExitCode(err); code != 13 {
		t.Fatalf("feedback list exit = %d, want 13 (degraded); err = %v", code, err)
	}

	var result feedbackListResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode feedback list %q: %v", out.String(), err)
	}
	if len(result.Entries) != 1 || result.Entries[0].Text != valid.Text {
		t.Errorf("entries = %#v, want only valid journal entry", result.Entries)
	}
	if result.SkippedCorruptLines != 1 {
		t.Errorf("skipped_corrupt_lines = %d, want 1", result.SkippedCorruptLines)
	}
	if got := errOut.String(); got != "warning: skipped 1 corrupt feedback journal line(s)\n" {
		t.Errorf("stderr = %q, want corrupt-journal warning", got)
	}
}
