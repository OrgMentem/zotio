// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean roadmap-phase5 vault-audit): add a read-only vault trust-plane audit for managed notes.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const (
	vaultAuditMaxWalkEntries   = 50000
	vaultAuditMaxMarkdownFiles = 10000

	vaultAuditIssueOrphaned           = "orphaned"
	vaultAuditIssueStale              = "stale"
	vaultAuditIssueUpgradeable        = "upgradeable"
	vaultAuditIssueNeedsNotesBoundary = "needs_notes_boundary"
)

type vaultAuditIssue struct {
	Path  string `json:"path"`
	Key   string `json:"key,omitempty"`
	Issue string `json:"issue"`
}

type vaultAuditReport struct {
	Vault     string            `json:"vault"`
	Status    string            `json:"status"`
	Synced    bool              `json:"synced"`
	Note      string            `json:"note,omitempty"`
	Scanned   int               `json:"scanned"`
	Managed   int               `json:"managed"`
	Unmanaged int               `json:"unmanaged"`
	Truncated bool              `json:"truncated"`
	Counts    map[string]int    `json:"counts"`
	Issues    []vaultAuditIssue `json:"issues"`
}

func newVaultAuditCmd(flags *rootFlags) *cobra.Command {
	var flagOut string
	cmd := &cobra.Command{
		Use:         "audit [--out <dir>]",
		Short:       "Audit managed vault notes without writing",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			outDir, err := resolveVaultOutDir(flags, flagOut)
			if err != nil {
				return err
			}
			report, err := auditVaultNotes(outDir)
			if err != nil {
				return err
			}
			return printVaultAuditReport(cmd, report, flags)
		},
	}
	cmd.Flags().StringVar(&flagOut, "out", "", "Vault directory (overrides [vault].root + notes_dir from config)")
	return cmd
}

// PATCH(glean roadmap-phase5 vault-audit): keep vault audit read-only and bounded for agent-facing use.
func auditVaultNotes(outDir string) (vaultAuditReport, error) {
	report := vaultAuditReport{
		Vault:  outDir,
		Status: "ok",
		Synced: true,
		Counts: newVaultAuditCounts(),
		Issues: []vaultAuditIssue{},
	}

	db, err := openStoreForRead(context.Background(), "zotio")
	if err != nil {
		return report, fmt.Errorf("opening local store: %w", err)
	}
	if db == nil {
		report.Status = "unsynced"
		report.Synced = false
		report.Note = "local store not synced; run sync"
	} else {
		defer db.Close()
	}

	qs := localQueryStore{db}
	walked := 0
	err = filepath.WalkDir(outDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		walked++
		if walked > vaultAuditMaxWalkEntries {
			report.Truncated = true
			return filepath.SkipAll
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		if report.Scanned >= vaultAuditMaxMarkdownFiles {
			report.Truncated = true
			return filepath.SkipAll
		}

		bodyBytes, err := os.ReadFile(path) //nolint:gosec // G122: audits the user's own local vault; not a security boundary, symlink TOCTOU is not a threat here.
		if err != nil {
			return err
		}
		report.Scanned++
		body := string(bodyBytes)
		zoteroKey := frontmatterKeyValue(body, "zotero_key")
		zoteroSelect := frontmatterKeyValue(body, "zotero")
		key := zoteroKey
		if key == "" {
			key = keyFromZoteroSelect(zoteroSelect)
		}
		if key == "" {
			report.Unmanaged++
			return nil
		}

		report.Managed++
		rel := vaultAuditRelPath(outDir, path)
		if zoteroKey == "" && keyFromZoteroSelect(zoteroSelect) != "" {
			addVaultAuditIssue(&report, rel, key, vaultAuditIssueUpgradeable)
		}
		if _, hasRegion := extractNotesRegion(body); !hasRegion {
			addVaultAuditIssue(&report, rel, key, vaultAuditIssueNeedsNotesBoundary)
		}
		if db == nil {
			return nil
		}

		rows, err := qs.QueryRaw(
			"SELECT COALESCE(json_extract(data,'$.data.version'), json_extract(data,'$.version')) AS version FROM resources WHERE resource_type='items' AND id=?",
			key,
		)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			addVaultAuditIssue(&report, rel, key, vaultAuditIssueOrphaned)
			return nil
		}
		storeVersion := sqlIntValue(rows[0]["version"])
		recorded := parseStateComment(body).NoteVersion
		if recorded > 0 && storeVersion > recorded {
			addVaultAuditIssue(&report, rel, key, vaultAuditIssueStale)
		}
		return nil
	})
	return report, err
}

func newVaultAuditCounts() map[string]int {
	return map[string]int{
		vaultAuditIssueOrphaned:           0,
		vaultAuditIssueStale:              0,
		vaultAuditIssueUpgradeable:        0,
		vaultAuditIssueNeedsNotesBoundary: 0,
	}
}

func addVaultAuditIssue(report *vaultAuditReport, path, key, issue string) {
	report.Counts[issue]++
	report.Issues = append(report.Issues, vaultAuditIssue{Path: path, Key: key, Issue: issue})
}

func vaultAuditRelPath(outDir, path string) string {
	rel, err := filepath.Rel(outDir, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func printVaultAuditReport(cmd *cobra.Command, report vaultAuditReport, flags *rootFlags) error {
	if flags.asJSON {
		data, err := json.Marshal(report)
		if err != nil {
			return err
		}
		return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Vault audit %s: status %s, scanned %d, managed %d, unmanaged %d, %s\n",
		report.Vault, report.Status, report.Scanned, report.Managed, report.Unmanaged, summarizeCounts(report.Counts))
	for _, issue := range report.Issues {
		line := fmt.Sprintf("  [%s] %s", issue.Issue, issue.Path)
		if issue.Key != "" {
			line += " — " + issue.Key
		}
		fmt.Fprintln(out, line)
	}
	return nil
}
