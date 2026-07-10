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

func seedSyncedBibcheckItems(t *testing.T, items []json.RawMessage) {
	t.Helper()
	seedBibcheckItems(t, items)
	db, err := store.OpenWithContext(context.Background(), defaultDBPath("zotio"))
	if err != nil {
		t.Fatalf("open store to save sync state: %v", err)
	}
	defer db.Close()
	if err := db.SaveSyncState("items", "", len(items)); err != nil {
		t.Fatalf("save items sync state: %v", err)
	}
}

func TestResolveCiteKeyPrefersFieldWithExtraFallback(t *testing.T) {
	tests := []struct {
		name        string
		citationKey string
		extra       string
		want        string
	}{
		{
			name:        "field wins over pinned Extra",
			citationKey: " dynamicKey ",
			extra:       "Citation Key: pinnedKey",
			want:        "dynamicKey",
		},
		{
			name:        "Extra fallback when field empty",
			citationKey: "  ",
			extra:       "notes\nCitation Key: pinnedKey\nmore notes",
			want:        "pinnedKey",
		},
		{
			name:        "empty when neither source exists",
			citationKey: "",
			extra:       "ordinary Extra notes",
			want:        "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveCiteKey(tc.citationKey, tc.extra); got != tc.want {
				t.Fatalf("resolveCiteKey(%q, %q) = %q, want %q", tc.citationKey, tc.extra, got, tc.want)
			}
		})
	}
}

func TestItemsCitekeyConflictsUsesCitationKeyFieldOnlyItems(t *testing.T) {
	bibcheckIsolatedHome(t)
	seedBibcheckItems(t, []json.RawMessage{
		json.RawMessage(`{"key":"FIELD1","version":1,"data":{"key":"FIELD1","itemType":"journalArticle","title":"Field One","citationKey":"fielddup"}}`),
		json.RawMessage(`{"key":"FIELD2","version":1,"data":{"key":"FIELD2","itemType":"journalArticle","title":"Field Two","citationKey":"fielddup"}}`),
	})

	missingCmd := newItemsCitekeyConflictsCmd(&rootFlags{})
	missingCmd.SilenceErrors, missingCmd.SilenceUsage = true, true
	missingCmd.SetArgs([]string{"--missing"})
	var missingOut bytes.Buffer
	missingCmd.SetOut(&missingOut)
	missingCmd.SetErr(&bytes.Buffer{})
	if err := missingCmd.Execute(); err != nil {
		t.Fatalf("items citekey-conflicts --missing: %v", err)
	}
	var missing []citekeyConflictRow
	if err := json.Unmarshal(missingOut.Bytes(), &missing); err != nil {
		t.Fatalf("decode missing rows %q: %v", missingOut.String(), err)
	}
	if len(missing) != 0 {
		t.Fatalf("citationKey-field-only items reported missing citekeys: %+v", missing)
	}

	conflictCmd := newItemsCitekeyConflictsCmd(&rootFlags{})
	conflictCmd.SilenceErrors, conflictCmd.SilenceUsage = true, true
	conflictCmd.SetArgs([]string{"--conflicts"})
	var conflictOut bytes.Buffer
	conflictCmd.SetOut(&conflictOut)
	conflictCmd.SetErr(&bytes.Buffer{})
	if err := conflictCmd.Execute(); err != nil {
		t.Fatalf("items citekey-conflicts --conflicts: %v", err)
	}
	var conflicts []citekeyConflictRow
	if err := json.Unmarshal(conflictOut.Bytes(), &conflicts); err != nil {
		t.Fatalf("decode conflict rows %q: %v", conflictOut.String(), err)
	}
	if len(conflicts) != 2 {
		t.Fatalf("conflicts = %+v, want two rows sharing fielddup", conflicts)
	}
	for _, row := range conflicts {
		if row.CiteKey != "fielddup" {
			t.Fatalf("conflict row = %+v, want citationKey field value fielddup", row)
		}
	}
}

func TestBibcheckRootCommandAcceptsCitationKeyFieldOnlyLibrary(t *testing.T) {
	home := isolateDemoEnv(t, "0")
	seedSyncedBibcheckItems(t, []json.RawMessage{
		json.RawMessage(`{"key":"FIELDOK","version":1,"data":{"key":"FIELDOK","itemType":"journalArticle","title":"Field Key Work","creators":[{"lastName":"Doe"}],"date":"2026","publicationTitle":"Journal of Regression Tests","citationKey":"fieldonly"}}`),
	})
	manuscript := filepath.Join(home, "paper.tex")
	writeTestFile(t, manuscript, `\cite{fieldonly}`)

	flags := &rootFlags{}
	root := newRootCmd(flags)
	root.SilenceErrors, root.SilenceUsage = true, true
	root.SetArgs([]string{"--json", "items", "bibcheck", manuscript})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatalf("root items bibcheck with citationKey-only library returned error: %v; output=%s", err, out.String())
	}

	var report bibcheckReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode bibcheck JSON %q: %v", out.String(), err)
	}
	if report.Summary != (bibcheckSummary{Total: 1, OK: 1}) {
		t.Fatalf("summary = %+v, want one resolved citation and no findings", report.Summary)
	}
	if len(report.Keys) != 1 || report.Keys[0].CiteKey != "fieldonly" || report.Keys[0].Status != "ok" || report.Keys[0].ItemKey != "FIELDOK" {
		t.Fatalf("keys = %+v, want fieldonly resolved to FIELDOK", report.Keys)
	}
}
