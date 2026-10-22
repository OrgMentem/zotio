// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// postFeedback installs http.ErrUseLastResponse so a trusted endpoint cannot
// redirect feedback into an internal target. The refused 3xx response is
// therefore a send that never happened and must not be reported as delivered.
func TestPostFeedbackStatusHandling(t *testing.T) {
	oldAllowPrivateOutbound := allowPrivateOutboundForTests.Load()
	allowPrivateOutboundForTests.Store(true)
	t.Cleanup(func() { allowPrivateOutboundForTests.Store(oldAllowPrivateOutbound) })

	tests := []struct {
		name    string
		status  int
		wantErr string
	}{
		{name: "moved permanently", status: http.StatusMovedPermanently, wantErr: "feedback endpoint returned 301"},
		{name: "temporary redirect", status: http.StatusTemporaryRedirect, wantErr: "feedback endpoint returned 307"},
		{name: "ok", status: http.StatusOK},
		{name: "no content", status: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var paths []string
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if r.URL.Path == "/feedback" {
					w.Header().Set("Location", "/moved")
					w.WriteHeader(tt.status)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)
			// postFeedback requires https and builds its own client, so the
			// test server's certificate has to be trusted through the default
			// transport that externalFetchHTTPClient clones.
			oldTransport := http.DefaultTransport
			http.DefaultTransport = srv.Client().Transport
			t.Cleanup(func() { http.DefaultTransport = oldTransport })

			err := postFeedback(srv.URL+"/feedback", FeedbackEntry{Text: "hello"})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("postFeedback(%d) = %v, want success", tt.status, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("postFeedback reported %d as sent, want an error naming the status", tt.status)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("postFeedback error = %v, want it to contain %q", err, tt.wantErr)
			}
			if len(paths) != 1 || paths[0] != "/feedback" {
				t.Fatalf("request paths = %v, want the refused redirect not to be followed", paths)
			}
		})
	}
}

// Truncation must land on a rune boundary: a byte slice through a multi-byte
// rune stores and posts U+FFFD in place of its last character.
func TestFeedbackTruncationKeepsRunesWhole(t *testing.T) {
	// 3-byte runes: 4096 is not a multiple of 3, so byte 4096 falls inside one.
	text := strings.Repeat("語", feedbackMaxTextLen)
	cmd := newFeedbackCmd(&rootFlags{asJSON: true, dryRun: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(text))
	cmd.SetArgs([]string{"--stdin"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("feedback: %v", err)
	}
	var result struct {
		Truncated bool          `json:"truncated"`
		Entry     FeedbackEntry `json:"entry"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if !result.Truncated {
		t.Fatal("over-long text not reported as truncated")
	}
	if strings.ContainsRune(result.Entry.Text, '\uFFFD') {
		t.Fatalf("truncated text ends in a corrupted rune: %q", result.Entry.Text[len(result.Entry.Text)-6:])
	}
	if want := feedbackMaxTextLen / 3 * 3; len(result.Entry.Text) != want {
		t.Fatalf("truncated text is %d bytes, want %d (the last whole rune within the cap)", len(result.Entry.Text), want)
	}
	if got, kept := truncateFeedbackText(strings.Repeat("a", feedbackMaxTextLen)); kept || len(got) != feedbackMaxTextLen {
		t.Fatalf("text at the cap was changed: truncated=%v len=%d", kept, len(got))
	}
}
