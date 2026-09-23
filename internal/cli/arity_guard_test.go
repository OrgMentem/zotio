// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Surplus positionals must fail before a read or a mutation can reach Zotero.
// Checking Cobra's actual error also catches commands that happen to fail later
// for an unrelated reason (such as a missing file or a required flag).
func TestArityGuardsRefuseSurplusBeforeRequests(t *testing.T) {
	tests := []struct {
		name string
		new  func(*rootFlags) *cobra.Command
		one  bool
	}{
		{"collections export", newCollectionsExportCmd, true},
		{"collections gaps", newCollectionsGapsCmd, true},
		{"collections items", newCollectionsItemsCmd, true},
		{"collections stats", newCollectionsStatsCmd, true},
		{"collections subcollections", newCollectionsSubcollectionsCmd, true},
		{"collections tags", newCollectionsTagsCmd, true},
		{"import arxiv", newImportArxivCmd, true},
		{"import doi", newImportDoiCmd, true},
		{"import file", newImportFileCmd, true},
		{"import isbn", newImportIsbnCmd, true},
		{"import pmid", newImportPmidCmd, true},
		{"import url", newImportUrlCmd, true},
		{"items annotations", newItemsAnnotationsCmd, true},
		{"items children", newItemsChildrenCmd, true},
		{"items cite", newItemsCiteCmd, true},
		{"items collections-of", newItemsCollectionsOfCmd, true},
		{"items file", newItemsFileCmd, true},
		{"items fulltext", newItemsFulltextCmd, true},
		{"items note-template", newItemsNoteTemplateCmd, true},
		{"items summarize", newItemsSummarizeCmd, true},
		{"searches materialize", newSearchesMaterializeCmd, true},
		{"searches run", newSearchesRunCmd, true},
		{"vault resolve", newVaultResolveCmd, true},
		{"creators rename", newCreatorsRenameCmd, false},
		{"items new", newItemsNewCmd, false},
		{"tags rename", newTagsRenameCmd, false},
		{"vault conflicts", newVaultConflictsCmd, false},
		{"vault pull", newVaultPullCmd, false},
		{"vault push", newVaultPushCmd, false},
		{"vault sync", newVaultSyncCmd, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctr := newReadSingleKeyCounter(t)
			args := []string{"stray"}
			want := "unknown command \"stray\""
			if tt.one {
				args = []string{"K1", "K2"}
				want = "accepts at most 1 arg(s), received 2"
			}
			out, err := executeReadSingleKeyCmd(t, ctr, tt.new(&rootFlags{asJSON: true, dataSource: "live", noCache: true}), args...)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s with surplus args: error = %v, output = %q; want %q", tt.name, err, out, want)
			}
			if ctr.requests != 0 {
				t.Errorf("%s with surplus args sent %d requests, want 0", tt.name, ctr.requests)
			}
			// MaximumNArgs must keep the old help path when no key is given.
			switch tt.name {
			case "collections items", "import doi", "items children", "searches materialize", "searches run", "vault resolve":
				helpCtr := newReadSingleKeyCounter(t)
				help, helpErr := executeReadSingleKeyCmd(t, helpCtr, tt.new(&rootFlags{dataSource: "live", noCache: true}))
				if helpErr != nil || !strings.Contains(help, "Usage:") {
					t.Fatalf("%s with no args: error = %v, output = %q; want help", tt.name, helpErr, help)
				}
				if helpCtr.requests != 0 {
					t.Errorf("%s help sent %d requests, want 0", tt.name, helpCtr.requests)
				}
			}
		})
	}
}

func TestAritySearchJoinsQueryTokens(t *testing.T) {
	var queries []string
	ctr := &readSingleKeyCounter{}
	ctr.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctr.requests++
		queries = append(queries, r.URL.Query().Get("q"))
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(ctr.server.Close)

	out, err := executeReadSingleKeyCmd(t, ctr, newSearchCmd(&rootFlags{asJSON: true, dataSource: "live", noCache: true}), "machine", "learning")
	if err != nil {
		t.Fatalf("search machine learning: %v (output=%q)", err, out)
	}
	if ctr.requests != 1 || len(queries) != 1 || queries[0] != "machine learning" {
		t.Errorf("live search: requests = %d, queries = %q; want one request with q=machine learning", ctr.requests, queries)
	}
}
