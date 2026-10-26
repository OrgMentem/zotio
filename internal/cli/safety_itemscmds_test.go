package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/mutation"
)

func TestItemsCmdsReplacementAndDeletePreviews(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected write: %s", r.Method)
		}
		w.Header().Set("Last-Modified-Version", "7")
		_, _ = io.WriteString(w, `{"key":"ABCD2345","version":7,"data":{"title":"Target title","name":"Target collection","tags":[{"tag":"old"}],"collections":["EFGH6789"]},"meta":{"numChildren":2,"numItems":4,"numCollections":3}}`)
	}))
	defer server.Close()
	t.Setenv("ZOTERO_BASE_URL", server.URL+"/users/0")
	t.Setenv("ZOTERO_API_KEY", "test-key")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cases := []struct {
		name    string
		makeCmd func(*rootFlags) *cobra.Command
		args    []string
		removed map[string]any
	}{
		{"replace", newItemsUpdateCmd, []string{"ABCD2345", "--tags", `[{"tag":"new"}]`, "--collections", `[]`}, map[string]any{"tags_set": []any{map[string]any{"tag": "old"}}, "collections_set": []any{"EFGH6789"}}},
		{"item", newItemsDeleteCmd, []string{"ABCD2345", "--permanent"}, map[string]any{"title": "Target title", "children": float64(2)}},
		{"collection", newCollectionsDeleteCmd, []string{"ABCD2345"}, map[string]any{"name": "Target collection", "items": float64(4), "subcollections": float64(3)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := tc.makeCmd(&rootFlags{asJSON: true, maxChanges: -1, allowDestructive: true})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			var env mutation.Envelope
			if err := json.Unmarshal(out.Bytes(), &env); err != nil {
				t.Fatalf("%v: %s", err, out.String())
			}
			if len(env.Plan.Operations) != 1 {
				t.Fatalf("plan: %+v", env.Plan)
			}
			for field, want := range tc.removed {
				found := false
				for _, change := range env.Plan.Operations[0].Changes {
					if change.Field == field {
						gotJSON, _ := json.Marshal(change.Remove)
						wantJSON, _ := json.Marshal(want)
						if string(gotJSON) != string(wantJSON) {
							t.Errorf("%s removed %s, want %s", field, gotJSON, wantJSON)
						}
						found = true
					}
				}
				if !found {
					t.Errorf("missing removal %s", field)
				}
			}
		})
	}
}

func TestItemsCmdsPDFRequiresEmail(t *testing.T) {
	t.Setenv("UNPAYWALL_EMAIL", "")
	for _, flag := range []string{"--missing-pdf", "--repair-pdf"} {
		cmd := newItemsEnrichCmd(&rootFlags{maxChanges: -1})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{flag, "--keys", "ABCD2345"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--email") || !strings.Contains(err.Error(), "UNPAYWALL_EMAIL") {
			t.Fatalf("%s: %v", flag, err)
		}
	}
}

func TestItemsCmdsCreatorNotFound(t *testing.T) {
	seedCreatorAuditFixStore(t, []json.RawMessage{json.RawMessage(`{"key":"ABCD2345","version":3,"data":{"key":"ABCD2345","itemType":"journalArticle","creators":[{"name":"Someone Else","creatorType":"author"}]}}`)})
	env, err := runCreatorsRenameTestCmd(t, &rootFlags{asJSON: true, maxChanges: -1}, "http://127.0.0.1:1", "--from", "Nobody Here", "--to", "New Name")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(env)
	if !bytes.Contains(raw, []byte("creator_not_found")) || !bytes.Contains(raw, []byte("Nobody Here")) {
		t.Fatalf("%s", raw)
	}
}

type itemsCmdsTransport func(*http.Request) (*http.Response, error)

func (f itemsCmdsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestItemsCmdsORCIDCancellationAndFailure(t *testing.T) {
	for _, cancelRequest := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancel", false: "provider"}[cancelRequest], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			old := http.DefaultTransport
			http.DefaultTransport = itemsCmdsTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if cancelRequest {
					cancel()
					return nil, context.Canceled
				}
				return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("rate limited")), Request: r}, nil
			})
			defer func() { http.DefaultTransport = old }()
			occ := &creatorOccurrence{ItemKey: "ABCD2345", DOI: "10.1234/test"}
			groups := []creatorVariantGroup{{Tier: creatorVariantTierInitials, members: []*creatorNameVariant{{Occurrences: []*creatorOccurrence{occ}}}}}
			summary, err := captureCreatorAuditORCIDs(ctx, localQueryStore{}, []*creatorOccurrence{occ}, groups, time.Second, false)
			if cancelRequest {
				if !errors.Is(err, context.Canceled) || calls != 1 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
				return
			}
			if err != nil || summary.Failed != 1 {
				t.Fatalf("summary=%+v err=%v", summary, err)
			}
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)
			err = renderCreatorsAuditResult(cmd, &rootFlags{asJSON: true}, creatorsAuditReport{Summary: creatorsAuditSummary{ORCID: &summary}})
			var exit *cliError
			if !errors.As(err, &exit) || exit.code != 13 {
				t.Fatalf("want exit 13: %v", err)
			}
			var report creatorsAuditReport
			if decodeErr := json.Unmarshal(out.Bytes(), &report); decodeErr != nil || report.Summary.ORCID == nil || report.Summary.ORCID.Failed != 1 {
				t.Fatalf("report: %s (%v)", out.String(), decodeErr)
			}
		})
	}
}

func TestItemsCmdsPreprintRerunNoOp(t *testing.T) {
	match := crossrefMatch{DOI: "10.1234/published", Venue: "Journal"}
	extra := appendPreprintCheckFixProvenance("User note", match)
	if got := appendPreprintCheckFixProvenance(extra, match); got != extra {
		t.Fatalf("duplicate provenance: %s", got)
	}
	item := map[string]any{"key": "ABCD2345", "data": map[string]any{"title": "Preprint", "extra": extra, "url": "https://arxiv.org/abs/1234.5678"}}
	proposal, skip, ok := preprintCheckFixProposalForItem(item, match)
	if !ok {
		t.Fatalf("unexpected skip: %+v", skip)
	}
	ops := preprintCheckFixPlannedOps(context.Background(), []preprintCheckFixProposal{proposal}, func() apiMutator { t.Fatal("no-op requested write client"); return nil })
	if len(ops) != 1 || len(ops[0].Changes) != 0 || ops[0].NoOpReason == nil {
		t.Fatalf("ops: %+v", ops)
	}
}
