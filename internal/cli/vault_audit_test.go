// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"zotio/internal/store"
)

func seedVaultAuditStore(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotio"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	items := []json.RawMessage{
		json.RawMessage(`{"key":"LIVE","version":7,"data":{"key":"LIVE","itemType":"journalArticle","title":"Live item"}}`),
	}
	if _, _, err := db.UpsertBatch("items", items); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = db.Close()
}

func TestVaultAuditReportsOrphanedManagedNote(t *testing.T) {
	seedVaultAuditStore(t)
	vaultDir := t.TempDir()
	writeFile(t, filepath.Join(vaultDir, "managed.md"), "---\nzotero_key: GHOST\n---\n\n## Notes\n"+
		vaultNotesBegin+"\nlocal notes\n"+vaultNotesEnd+"\n")
	writeFile(t, filepath.Join(vaultDir, "unmanaged.md"), "# My own note\n\nNo Zotero metadata here.\n")

	flags := &rootFlags{asJSON: true}
	cmd := newVaultCmd(flags)
	cmd.SetArgs([]string{"audit", "--out", vaultDir})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("vault audit: %v", err)
	}

	var report struct {
		Scanned  int            `json:"scanned"`
		Managed  int            `json:"managed"`
		Counts   map[string]int `json:"counts"`
		Findings []Finding      `json:"findings"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode audit JSON: %v\n%s", err, out.String())
	}
	if report.Scanned != 2 {
		t.Fatalf("scanned = %d, want 2", report.Scanned)
	}
	if report.Managed < 1 {
		t.Fatalf("managed = %d, want at least 1", report.Managed)
	}
	if report.Counts[vaultAuditIssueOrphaned] != 1 {
		t.Fatalf("orphaned count = %d, want 1 (report=%+v)", report.Counts[vaultAuditIssueOrphaned], report)
	}
	for _, finding := range report.Findings {
		if finding.Kind == "vault_orphan" && finding.ItemKey == "GHOST" {
			return
		}
	}
	t.Fatalf("missing orphaned GHOST finding: %+v", report.Findings)
}
