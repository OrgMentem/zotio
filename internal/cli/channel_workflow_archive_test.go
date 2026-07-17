// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"zotio/internal/store"
)

func TestWorkflowArchive_FetchFailureRetainsCursorAndReportsIncomplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resource := strings.TrimPrefix(r.URL.Path, "/users/0/")
		if resource == "items" {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "archive.db")
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.SaveSyncState("items", "PREVIOUS", 3); err != nil {
		_ = db.Close()
		t.Fatalf("seed sync state: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}

	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newWorkflowArchiveCmd(&rootFlags{asJSON: true, noCache: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--db", dbPath})

	err = cmd.Execute()
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("archive error = %v, exit=%d; want degraded non-zero error", err, ExitCode(err))
	}
	var result struct {
		Status   string   `json:"status"`
		Failures []string `json:"failures"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode archive result %q: %v", out.String(), err)
	}
	if result.Status != "incomplete" || len(result.Failures) == 0 {
		t.Fatalf("archive result = %+v, want incomplete result with failures", result)
	}
	if strings.Contains(out.String(), "Archived ") {
		t.Fatalf("archive reported success after failure: %s", out.String())
	}

	db, err = store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer db.Close()
	cursor, _, _, err := db.GetSyncState("items")
	if err != nil || cursor != "PREVIOUS" {
		t.Fatalf("items cursor = %q, %v; want unchanged PREVIOUS", cursor, err)
	}
}
