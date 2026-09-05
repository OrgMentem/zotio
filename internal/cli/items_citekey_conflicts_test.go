// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"zotio/internal/store"
)

func seedSyncedBibcheckItems(t *testing.T, items []json.RawMessage) {
	t.Helper()
	seedBibcheckItems(t, items)
	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
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
			// Zotero itself writes the pinned line without the space, while
			// zotio's importer writes it with one. findRowMatchesExact has
			// accepted both spellings since ac8ea71, so a parser that read
			// only the spaced form left every inventory built on this
			// function — conflicts, health, bibcheck, near-key suggestions —
			// blind to keys the exact lookup can already match.
			name:        "colon-tight Extra fallback",
			citationKey: "",
			extra:       "notes\nCitation Key:tightKey\nmore notes",
			want:        "tightKey",
		},
		{
			// Extra is free text, so a bare label can precede the real line.
			// Returning "" at the first label would hide the key below it.
			name:        "bare label does not shadow a later key",
			citationKey: "",
			extra:       "Citation Key:\nCitation Key: realKey",
			want:        "realKey",
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

// runCitekeyConflicts runs `items citekey-conflicts` with the given flags and
// decodes the rows it prints.
func runCitekeyConflicts(t *testing.T, args ...string) []citekeyConflictRow {
	t.Helper()
	cmd := newItemsCitekeyConflictsCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("items citekey-conflicts %v: %v", args, err)
	}
	var rows []citekeyConflictRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("decode citekey-conflicts rows %q: %v", out.String(), err)
	}
	return rows
}

// Reading the colon-tight spelling changes this command's answer, and the new
// answer is the correct one: an item whose Extra holds "Citation Key:same2023"
// DOES carry a pinned key, so calling it missing was a false negative, and two
// items carrying that one key really are the conflict this command reports.
// That is what a library Zotero pinned the keys in looks like.
func TestItemsCitekeyConflictsReadsColonTightPinnedKeys(t *testing.T) {
	bibcheckIsolatedHome(t)
	seedBibcheckItems(t, []json.RawMessage{
		json.RawMessage(`{"key":"TIGHT1","version":1,"data":{"key":"TIGHT1","itemType":"journalArticle","title":"Tight One","extra":"Citation Key:same2023"}}`),
		json.RawMessage(`{"key":"TIGHT2","version":1,"data":{"key":"TIGHT2","itemType":"journalArticle","title":"Tight Two","extra":"Citation Key:same2023"}}`),
		json.RawMessage(`{"key":"SPACED","version":1,"data":{"key":"SPACED","itemType":"journalArticle","title":"Spaced One","extra":"Citation Key: solo2023"}}`),
		json.RawMessage(`{"key":"NOKEY","version":1,"data":{"key":"NOKEY","itemType":"journalArticle","title":"No Key","extra":"just a note"}}`),
	})

	missing := runCitekeyConflicts(t, "--missing")
	if len(missing) != 1 || missing[0].Key != "NOKEY" {
		t.Fatalf("missing rows = %+v, want only NOKEY: a colon-tight pinned key is a key", missing)
	}

	conflicts := runCitekeyConflicts(t, "--conflicts")
	gotKeys := make([]string, 0, len(conflicts))
	for _, row := range conflicts {
		if row.CiteKey != "same2023" {
			t.Fatalf("conflict row = %+v, want the shared colon-tight key same2023", row)
		}
		gotKeys = append(gotKeys, row.Key)
	}
	if !reflect.DeepEqual(gotKeys, []string{"TIGHT1", "TIGHT2"}) {
		t.Fatalf("conflict items = %#v, want both items holding same2023", gotKeys)
	}
}

func TestItemsCitekeyConflictsUsesCitationKeyFieldOnlyItems(t *testing.T) {
	bibcheckIsolatedHome(t)
	seedBibcheckItems(t, []json.RawMessage{
		json.RawMessage(`{"key":"FIELD1","version":1,"data":{"key":"FIELD1","itemType":"journalArticle","title":"Field One","citationKey":"fielddup"}}`),
		json.RawMessage(`{"key":"FIELD2","version":1,"data":{"key":"FIELD2","itemType":"journalArticle","title":"Field Two","citationKey":"fielddup"}}`),
	})

	missing := runCitekeyConflicts(t, "--missing")
	if len(missing) != 0 {
		t.Fatalf("citationKey-field-only items reported missing citekeys: %+v", missing)
	}

	conflicts := runCitekeyConflicts(t, "--conflicts")
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
