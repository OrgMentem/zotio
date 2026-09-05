// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The reading queue was the one table of this family that already sanitized,
// so it was also the one whose remaining hole was easy to miss:
// sanitizeForTerminal keeps tabs by design (helpers.go), and every cell here
// is fed to newTabWriter, where a tab inside a cell opens a column and pushes
// the author, year and date under the wrong headers. The key was not folded at
// all. Both halves are stored text, so both go through advisoryCell now.
func TestPrintReadingListRendersHostileLibraryTextAsInertData(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	result := readingListResult{
		Count:  2,
		Oldest: "2026-07-09\n== FAKE HEADING ==\x1b[31m",
		Items: []readingListItem{
			{Key: "K1", Title: "Plain Title", Author: "Jane Smith", Year: "2026", DateAdded: "2026-07-09", ItemType: "journalArticle"},
			{Key: "K2\x1b[32m", Title: hostileLibraryText, Author: "Ada\tLovelace", Year: "2026\n", DateAdded: "2026-07-10", ItemType: "book\tfake"},
		},
	}
	if err := printReadingList(cmd, result); err != nil {
		t.Fatalf("printReadingList: %v", err)
	}
	body := out.String()
	assertNoTerminalInjection(t, "reading list", body)
	assertOneRowPerRecord(t, "reading list", body, "Key", 2)
	if !strings.Contains(body, "Reading queue: 2 items") {
		t.Fatalf("the header line is gone:\n%q", body)
	}
}

// The title column used to be truncated at 80, a width inherited from the CLI
// this command was imported from and never justified anywhere. Folding tabs
// here means going through advisoryCell, which carries the cell budget with
// it, so the column now matches printTable and every other ranked item list.
// This pins that decision: a title too wide for the shared budget is cut.
func TestPrintReadingListTruncatesTheTitleToTheSharedCellBudget(t *testing.T) {
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	long := strings.Repeat("x", advisoryCellWidth+12)
	if err := printReadingList(cmd, readingListResult{
		Count:  1,
		Oldest: "2026-07-09",
		Items:  []readingListItem{{Key: "K1", Title: long, Author: "Jane Smith", Year: "2026", DateAdded: "2026-07-09", ItemType: "book"}},
	}); err != nil {
		t.Fatalf("printReadingList: %v", err)
	}
	body := out.String()
	if strings.Contains(body, long) {
		t.Fatalf("the title prints whole, so this table still carries a wider budget than every other one:\n%q", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "x") {
			continue
		}
		for _, cell := range tabwriterColumnGap.Split(strings.TrimRight(line, " "), -1) {
			if strings.HasPrefix(cell, "x") && displayWidth(cell) > advisoryCellWidth {
				t.Fatalf("the title cell is %d columns wide, want at most %d:\n%q", displayWidth(cell), advisoryCellWidth, body)
			}
		}
	}
}
