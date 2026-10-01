// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

// TestFxeCreateMalformedFailureEntryIsNotApplied pins the create commands to
// the shared per-entry failure decoder: a 2xx batch response whose `failed`
// entry is not a {code, message} object with a non-zero code means Zotero
// rejected that object without saying how. Decoding the map in one call
// dropped every failure, so the rejected object was reported applied and the
// run exited 0.
func TestFxeCreateMalformedFailureEntryIsNotApplied(t *testing.T) {
	commands := []struct {
		name   string
		newCmd func(*rootFlags) *cobra.Command
		args   []string
		stdin  string
	}{
		{
			name:   "collections_create",
			newCmd: newCollectionsCreateCmd,
			args:   []string{"--stdin"},
			stdin:  `[{"name":"a"},{"name":"b"}]`,
		},
		{
			name:   "items_create",
			newCmd: newItemsCreateCmd,
			args:   []string{"--items", `[{"itemType":"journalArticle","title":"a"},{"itemType":"journalArticle","title":"b"}]`},
		},
	}
	entries := []struct {
		name  string
		entry string
	}{
		{name: "non_numeric_code", entry: `{"code":"bad","message":"x"}`},
		{name: "missing_code", entry: `{"message":"rejected"}`},
		{name: "not_an_object", entry: `"rejected"`},
	}
	for _, command := range commands {
		for _, entry := range entries {
			t.Run(command.name+"/"+entry.name, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				mutationJournalRecorder = recordMutationJournal
				t.Cleanup(func() { mutationJournalRecorder = nil })

				body := `{"success":{"0":"KEY00001"},"successful":{},"unchanged":{},"failed":{"1":` + entry.entry + `}}`
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
				}))
				defer srv.Close()
				t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

				cmd := command.newCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
				cmd.SilenceErrors, cmd.SilenceUsage = true, true
				cmd.SetErr(io.Discard)
				cmd.SetOut(&bytes.Buffer{})
				if command.stdin != "" {
					cmd.SetIn(strings.NewReader(command.stdin))
				}
				cmd.SetArgs(command.args)
				err := cmd.Execute()
				if err == nil || ExitCode(err) != 13 {
					t.Fatalf("error = %v, exit=%d; want degraded failure for a malformed failed entry", err, ExitCode(err))
				}
				for _, want := range []string{"index 1", "malformed failure entry", "outcome is unknown"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error = %q, want %q", err, want)
					}
				}

				journal, listErr := mutation.ListEntries(helpersTestJournalDir(t))
				if listErr != nil {
					t.Fatalf("list journal entries: %v", listErr)
				}
				if len(journal) != 1 {
					t.Fatalf("journal entries = %d, want 1", len(journal))
				}
				if got := journal[0].Summary; got.Applied != 1 || got.Failed != 1 {
					t.Fatalf("journaled summary = %+v, want 1 applied and 1 failed: the malformed entry must not be applied", got)
				}
			})
		}
	}
}
