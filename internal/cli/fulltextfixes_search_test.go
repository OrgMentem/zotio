// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/store"
)

func sliceSearchSeedParentAttachment(t *testing.T, dbPath string) {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"ABCD2345","version":1,"data":{"key":"ABCD2345","itemType":"journalArticle","title":"Parent Paper"}}`),
		json.RawMessage(`{"key":"ABCD2346","version":1,"data":{"key":"ABCD2346","itemType":"attachment","parentItem":"ABCD2345","contentType":"application/pdf"}}`),
	}); err != nil {
		_ = db.Close()
		t.Fatalf("seed items: %v", err)
	}
	if err := db.SaveSyncStateContext(context.Background(), "items", "", -1); err != nil {
		_ = db.Close()
		t.Fatalf("seed items sync state: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func sliceSearchSeedFulltext(t *testing.T, dbPath, attachmentKey, content string) {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	payload, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		_ = db.Close()
		t.Fatalf("encode full text: %v", err)
	}
	if _, err := db.UpsertKeyed("fulltext", []string{attachmentKey}, []json.RawMessage{payload}); err != nil {
		_ = db.Close()
		t.Fatalf("seed full text: %v", err)
	}
	if err := db.SaveSyncState("fulltext", "", 1); err != nil {
		_ = db.Close()
		t.Fatalf("seed fulltext completion: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

func sliceSearchRun(t *testing.T, flags *rootFlags, args ...string) (string, error) {
	t.Helper()
	cmd := newSearchCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// An empty --fulltext answer from a mirror that never indexed any PDF text is
// "not indexed", not "no match": it must refuse with exit 9 and name
// `zotio sync --fulltext` instead of reporting zero results.
func TestSliceSearchFulltextWithoutIndexIsPrecondition(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "search-noft.db")
	sliceSearchSeedParentAttachment(t, dbPath)
	out, err := sliceSearchRun(t, &rootFlags{asJSON: true, dataSource: "local"},
		"calibration", "--fulltext", "--db", dbPath)
	if err == nil {
		t.Fatalf("search --fulltext without indexed text succeeded (out=%s), want exit 9", out)
	}
	if got := ExitCode(err); got != 9 {
		t.Fatalf("exit code = %d, want 9 (err=%v, out=%s)", got, err, out)
	}
	if !strings.Contains(err.Error(), "sync --fulltext") {
		t.Fatalf("error = %q, want remediation naming `zotio sync --fulltext`", err.Error())
	}
}

// Once full text is indexed, the same empty answer is a genuine no-match with
// exit 0: the readiness gate must not refuse indexed mirrors.
func TestSliceSearchFulltextIndexedEmptyIsNoMatch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "search-indexed.db")
	sliceSearchSeedParentAttachment(t, dbPath)
	sliceSearchSeedFulltext(t, dbPath, "ABCD2346", "some unrelated prose about methods")
	out, err := sliceSearchRun(t, &rootFlags{asJSON: true, dataSource: "local"},
		"zzzznomatch", "--fulltext", "--db", dbPath)
	if err != nil {
		t.Fatalf("search --fulltext on indexed mirror: %v (out=%s)", err, out)
	}
	var got struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode search output %q: %v", out, err)
	}
	if len(got.Results) != 0 {
		t.Fatalf("results = %d, want 0", len(got.Results))
	}
}

// An empty or whitespace-only query is a usage error (exit 2) raised before
// the store is touched, so scripts with an unset query variable never see an
// FTS5 syntax error from SQLite.
func TestSliceSearchEmptyQueryIsUsage(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "empty", args: []string{""}},
		{name: "whitespace", args: []string{"  "}},
		{name: "joined whitespace", args: []string{" ", " "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sliceSearchRun(t, &rootFlags{asJSON: true, dataSource: "local"},
				append(tc.args, "--db", filepath.Join(t.TempDir(), "unused.db"))...)
			if err == nil {
				t.Fatalf("search %q succeeded, want exit 2", tc.args)
			}
			if got := ExitCode(err); got != 2 {
				t.Fatalf("search %q exit = %d, want 2 (err=%v)", tc.args, got, err)
			}
			if !strings.Contains(err.Error(), "empty") {
				t.Fatalf("search %q error = %q, want it to name the empty query", tc.args, err.Error())
			}
		})
	}
}
