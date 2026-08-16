// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Regression: zotio-95ec99f — connector import must reconcile op status against the translator count.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		if m["key"] != "AAA11111" {
			t.Fatalf("index 0 key %v, want AAA11111", m["key"])
		}
		status, reason, _ = s.apply(cmd, 1)
		m2, _ := reason.(map[string]any)
		if m2["key"] != "BBB22222" {
			t.Fatalf("index 1 key %v, want BBB22222 (status %v)", m2["key"], status)
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
			idx := index
			ops = append(ops, mutation.Op{
				ID:      fmt.Sprintf("import.file:connector:%03d", idx+1),
				Key:     "record",
				Kind:    "item_create",
				Changes: []mutation.Change{{Field: "item", Add: map[string]any{"record": idx + 1}}},
				Apply: func() (string, any, error) {
					return s.apply(cmd, idx)
				},
			})
		}
		_ = ops
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
		for i, it := range env.Result.Items {
			if it.Status == "applied" {
				want := ""
				if i < len(s.keys) {
					want = s.keys[i]
				}
				if it.Key != want {
					t.Fatalf("item %d key %q, want %q", i, it.Key, want)
				}
			}
		}
	})

	t.Run("dry_run_preview_still_three", func(t *testing.T) {
		content := "@article{a, title={One}} @article{b, title={Two}} @article{c, title={Three}}\n"
		path := filepath.Join(t.TempDir(), "three.bib")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		flags := &rootFlags{asJSON: true, dryRun: true, maxChanges: -1, configPath: testConfigFile(t, "http://localhost:23119/api/users/0")}
		cmd := newImportFileCmd(flags)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetArgs([]string{path})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("preview: %v", err)
		}
		var env struct {
			Mode          string `json:"mode"`
			PreviewReason string `json:"preview_reason"`
			Result        *any   `json:"result"`
			Plan          struct {
				Summary struct {
					Planned int `json:"planned"`
				} `json:"summary"`
				Operations []struct {
					ID string `json:"id"`
				} `json:"operations"`
			} `json:"plan"`
		}
		if err := json.Unmarshal(out.Bytes(), &env); err != nil {
			t.Fatalf("decode preview: %v (%s)", err, out.String())
		}
		if env.Mode != "preview" || env.Result != nil {
			t.Fatalf("want preview with no result, got mode %q result %v (%s)", env.Mode, env.Result, out.String())
		}
		if env.Plan.Summary.Planned != 3 || len(env.Plan.Operations) != 3 {
			t.Fatalf("planned %d ops %d, want 3/3 (%s)", env.Plan.Summary.Planned, len(env.Plan.Operations), out.String())
		}
		if got := countImportFileRecords(content, "bibtex"); got != 3 {
			t.Fatalf("countImportFileRecords %d, want 3", got)
		}
	})
}

func TestImportFileConnectorPartialResultOnFilingFailure(t *testing.T) {
	s := &importFileConnectorSession{
		flags:     &rootFlags{},
		records:   3,
		imported:  2,
		sessionID: "SESS123",
		keys:      []string{"AAA11111", "BBB22222"},
		target:    "C999",
		done:      true,
		err:       io.ErrUnexpectedEOF,
	}
	cmd := newImportFileCmd(&rootFlags{asJSON: true})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	for _, tc := range []struct {
		index      int
		wantStatus string
		wantKey    string
	}{
		{index: 0, wantStatus: "applied", wantKey: "AAA11111"},
		{index: 1, wantStatus: "applied", wantKey: "BBB22222"},
		{index: 2, wantStatus: "skipped", wantKey: ""},
	} {
		status, reason, err := s.apply(cmd, tc.index)
		if err != nil {
			t.Fatalf("index %d err %v, want nil (partial success is applied, not failed)", tc.index, err)
		}
		if status != tc.wantStatus {
			t.Fatalf("index %d status %q, want %q (reason %v)", tc.index, status, tc.wantStatus, reason)
		}
		if tc.wantStatus == "applied" {
			m, ok := reason.(map[string]any)
			if !ok {
				t.Fatalf("index %d reason %T, want map with singular key", tc.index, reason)
			}
			if m["key"] != tc.wantKey {
				t.Fatalf("index %d key %v, want %q", tc.index, m["key"], tc.wantKey)
			}
			if m["session"] != "SESS123" {
				t.Fatalf("index %d session %v, want SESS123", tc.index, m["session"])
			}
			msg, _ := m["message"].(string)
			if !strings.Contains(msg, tc.wantKey) || !strings.Contains(msg, "filing failed") {
				t.Fatalf("index %d message %q, want key %q and filing warning", tc.index, msg, tc.wantKey)
			}
			if _, has := m["keys"]; has {
				t.Fatalf("index %d reason must use singular key, not plural keys", tc.index)
			}
		}
	}

	ops := make([]mutation.Op, 0, 3)
	for index := range 3 {
		idx := index
		ops = append(ops, mutation.Op{
			ID:      fmt.Sprintf("import.file:connector:%03d", idx+1),
			Key:     "record",
			Kind:    "item_create",
			Changes: []mutation.Change{{Field: "item", Add: map[string]any{"record": idx + 1}}},
			Apply: func() (string, any, error) {
				return s.apply(cmd, idx)
			},
		})
	}
	env, _ := mutation.Run(mutation.Options{Yes: true, MaxChanges: -1}, "import.file", ops)
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
		t.Fatalf("failed %d, want 0 (partial is applied, not failed)", env.Result.Summary.Failed)
	}
	for i, it := range env.Result.Items {
		if i < 2 {
			if it.Status != "applied" {
				t.Fatalf("item %d status %q, want applied", i, it.Status)
			}
			if it.Key != s.keys[i] {
				t.Fatalf("item %d key %q, want %q (singular key must populate ResultItem.Key)", i, it.Key, s.keys[i])
			}
			m, _ := it.Reason.(map[string]any)
			msg, _ := m["message"].(string)
			if !strings.Contains(msg, "retry filing only") {
				t.Fatalf("item %d message %q, want retry guidance", i, msg)
			}
		} else if it.Status != "skipped" {
			t.Fatalf("item %d status %q, want skipped (beyond imported)", i, it.Status)
		}
	}
	journal, ok := mutation.BuildJournalEntry(env, time.Now())
	if !ok {
		t.Fatalf("BuildJournalEntry ok false")
	}
	for i, op := range journal.Ops {
		if i < 2 && op.Key != s.keys[i] {
			t.Fatalf("journal op %d key %q, want %q", i, op.Key, s.keys[i])
		}
	}
	if journal.Summary.Applied != 2 {
		t.Fatalf("journal applied %d, want 2", journal.Summary.Applied)
	}
}
