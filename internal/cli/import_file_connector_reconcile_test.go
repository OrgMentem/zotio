// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Regression: zotio-95ec99f — connector import must reconcile op status against the translator count.

package cli

import (
	"io"
	"strings"
	"testing"

	"zotio/internal/mutation"
)

// Stub the connector Import to return 2 items for a 3-record file: summary
// reports 2 applied and 1 non-applied with a translator-count reason, never 3
// applied. The preview/plan fan-out stays at 3 so --dry-run is unchanged.

func TestImportFileConnectorReconcilesTranslatorCount(t *testing.T) {
	t.Run("per_op_status", func(t *testing.T) {
		s := &importFileConnectorSession{
			flags:         &rootFlags{},
			content:       []byte("@article{a, title={One}} @article{b, title={Two}} @article{c, title={Three}}"),
			format:        "bibtex",
			records:       3,
			imported:      2,
			sessionID:     "SESSION",
			keys:          []string{"AAA11111", "BBB22222"},
			done:          true,
			target:        "",
			collectionKey: "",
		}
		cmd := newImportFileCmd(&rootFlags{asJSON: true})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)

		for _, tc := range []struct {
			index      int
			wantStatus string
		}{
			{index: 0, wantStatus: "applied"},
			{index: 1, wantStatus: "applied"},
			{index: 2, wantStatus: "skipped"},
		} {
			status, reason, err := s.apply(cmd, tc.index)
			if err != nil {
				t.Fatalf("index %d apply err %v", tc.index, err)
			}
			if status != tc.wantStatus {
				t.Fatalf("index %d status %q, want %q (reason %v)", tc.index, status, tc.wantStatus, reason)
			}
			if tc.wantStatus == "skipped" {
				msg, _ := reason.(string)
				if !strings.Contains(msg, "translator returned 2") || !strings.Contains(msg, "3 parsed") {
					t.Fatalf("skipped reason %q, want translator count 2 for 3", msg)
				}
			}
		}
		status, reason, _ := s.apply(cmd, 0)
		if status != "applied" {
			t.Fatalf("index 0 status %q", status)
		}
		m, ok := reason.(map[string]any)
		if !ok || m["imported"] != 2 {
			t.Fatalf("index 0 reason %v, want imported=2", reason)
		}
	})

	t.Run("mutation_summary", func(t *testing.T) {
		s := &importFileConnectorSession{
			flags:     &rootFlags{},
			records:   3,
			imported:  2,
			sessionID: "SESSION",
			keys:      []string{"AAA11111", "BBB22222"},
			done:      true,
		}
		cmd := newImportFileCmd(&rootFlags{asJSON: true})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)

		ops := make([]mutation.Op, 0, 3)
		for index := range 3 {
			ops = append(ops, mutation.Op{
				ID:      "import.file:connector",
				Key:     "record",
				Kind:    "item_create",
				Changes: []mutation.Change{{Field: "item", Add: map[string]any{"record": index + 1}}},
				Apply: func() (string, any, error) {
					return s.apply(cmd, index)
				},
			})
		}
		// Loop variable capture is per-iteration in Go 1.22+, but the classic
		// closure-over-index pitfall would still misattribute ops if this code
		// ever regressed; the per-op reason above asserts the index identity.
		_ = ops
		// Run via mutation engine so summary counts are the real contract.
		// Must be in apply mode (Yes:true) — preview mode returns no Result.
		opts := mutation.Options{Yes: true, MaxChanges: -1}
		env, _ := mutation.Run(opts, "import.file", ops)
		if env.Result == nil {
			t.Fatalf("no result")
		}
		if env.Result.Summary.Applied != 2 {
			t.Fatalf("applied %d, want 2", env.Result.Summary.Applied)
		}
		if env.Result.Summary.Skipped != 1 {
			t.Fatalf("skipped %d, want 1", env.Result.Summary.Skipped)
		}
		if env.Result.Summary.Failed != 0 {
			t.Fatalf("failed %d, want 0", env.Result.Summary.Failed)
		}
		var nonAppliedReason any
		for _, it := range env.Result.Items {
			if it.Status != "applied" {
				nonAppliedReason = it.Reason
				break
			}
		}
		msg, _ := nonAppliedReason.(string)
		if !strings.Contains(strings.ToLower(msg), "translator returned 2") || !strings.Contains(strings.ToLower(msg), "3 parsed") {
			t.Fatalf("non-applied reason %q, want translator returned 2 for 3 parsed", msg)
		}
	})

	t.Run("dry_run_preview_still_three", func(t *testing.T) {
		content := "@article{a,\n title={One}\n}\n@article{b,\n title={Two}\n}\n@article{c,\n title={Three}\n}\n"
		records := countImportFileRecords(content, "bibtex")
		if records != 3 {
			t.Fatalf("count %d, want 3", records)
		}
	})
}

func TestImportFileConnectorPartialResultOnFilingFailure(t *testing.T) {
	// Import committed (session populated) but UpdateSession failed. apply must
	// return the populated result plus the error, never a zero value.
	s := &importFileConnectorSession{
		flags:     &rootFlags{},
		sessionID: "SESS123",
		keys:      []string{"AAA11111", "BBB22222"},
		imported:  2,
		records:   3,
		target:    "COLL",
		done:      true,
		err:       io.ErrUnexpectedEOF,
	}
	cmd := newImportFileCmd(&rootFlags{asJSON: true})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	status, reason, err := s.apply(cmd, 0)
	if status != "failed" {
		t.Fatalf("status %q, want failed", status)
	}
	if err == nil {
		t.Fatalf("want error from filing failure")
	}
	m, ok := reason.(map[string]any)
	if !ok {
		t.Fatalf("reason %v, want map with session/keys", reason)
	}
	if m["session"] != "SESS123" {
		t.Fatalf("session %v, want SESS123", m["session"])
	}
	if m["imported"] != 2 {
		t.Fatalf("imported %v, want 2", m["imported"])
	}
}
