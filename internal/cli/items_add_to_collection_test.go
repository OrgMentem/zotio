// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"zotio/internal/mutation"
)

type collectionFilingTestServer struct {
	server                    *httptest.Server
	collectionKey             string
	collectionName            string
	collectionCreates         int
	collectionWriteTokens     []string
	ambiguousCollectionCreate bool
	rejectReusedTokens        bool // answer 412 to a write token already used, as Zotero does
	usedTokens                map[string]bool
	collectionsGet            func(http.ResponseWriter, *http.Request)
	itemCollections           []string
	itemPatchCount            int
}

func newCollectionFilingTestServer(t *testing.T) *collectionFilingTestServer {
	t.Helper()
	ts := &collectionFilingTestServer{collectionKey: "", itemCollections: []string{"EXISTAB2"}}
	ts.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/0/collections":
			switch r.Method {
			case http.MethodGet:
				if ts.collectionsGet != nil {
					ts.collectionsGet(w, r)
					return
				}
				if ts.collectionKey == "" {
					_, _ = fmt.Fprint(w, "[]")
					return
				}
				_, _ = fmt.Fprintf(w, `[{"key":%q,"data":{"name":%q}}]`, ts.collectionKey, ts.collectionName)
			case http.MethodPost:
				// The live Zotero write API rejects non-array payloads with
				// HTTP 400 "Uploaded data must be a JSON array" and answers
				// with the array-write envelope; enforce both here.
				var body []map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, "Uploaded data must be a JSON array", http.StatusBadRequest)
					return
				}
				if len(body) != 1 {
					http.Error(w, "expected exactly one collection", http.StatusBadRequest)
					return
				}
				name, _ := body[0]["name"].(string)
				token := r.Header.Get("Zotero-Write-Token")
				ts.collectionWriteTokens = append(ts.collectionWriteTokens, token)
				if ts.rejectReusedTokens {
					if ts.usedTokens[token] {
						http.Error(w, `{"error":"write token already submitted"}`, http.StatusPreconditionFailed)
						return
					}
					if ts.usedTokens == nil {
						ts.usedTokens = map[string]bool{}
					}
					ts.usedTokens[token] = true
				}
				if ts.ambiguousCollectionCreate && ts.collectionCreates > 0 {
					http.Error(w, `{"error":"write token already submitted"}`, http.StatusPreconditionFailed)
					return
				}
				ts.collectionCreates++
				ts.collectionKey, ts.collectionName = "CLCTAB23", name
				if ts.ambiguousCollectionCreate {
					http.Error(w, "response lost after create", http.StatusInternalServerError)
					return
				}
				_, _ = fmt.Fprint(w, `{"success":{"0":"CLCTAB23"},"successful":{"0":{"key":"CLCTAB23"}}}`)
			default:
				http.Error(w, "unexpected collection method", http.StatusMethodNotAllowed)
			}
		case "/users/0/items/ITEMAB23":
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Last-Modified-Version", "17")
				_, _ = fmt.Fprintf(w, `{"key":"ITEMAB23","version":17,"data":{"collections":%s}}`, mustJSON(t, ts.itemCollections))
			case http.MethodPatch:
				var body struct {
					Collections []string `json:"collections"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode item patch: %v", err)
				}
				ts.itemPatchCount++
				ts.itemCollections = append([]string(nil), body.Collections...)
				w.WriteHeader(http.StatusNoContent)
			default:
				http.Error(w, "unexpected item method", http.StatusMethodNotAllowed)
			}
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.server.Close)
	return ts
}

func runItemsAddToCollectionTestCmd(t *testing.T, srv *collectionFilingTestServer, args ...string) mutation.Envelope {
	t.Helper()
	env, err := executeItemsAddToCollectionTestCmd(t, srv, args...)
	if err != nil {
		t.Fatalf("items add-to-collection %v: %v", args, err)
	}
	return env
}

func executeItemsAddToCollectionTestCmd(t *testing.T, srv *collectionFilingTestServer, args ...string) (mutation.Envelope, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newItemsAddToCollectionCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		return mutation.Envelope{}, err
	}
	var env mutation.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		return mutation.Envelope{}, fmt.Errorf("decoding mutation envelope %q: %w", out.String(), err)
	}
	return env, nil
}

// executeItemsAddToCollectionTestCmdWithFlags runs the command with caller-supplied
// flags and returns the raw stdout, undecoded, so callers can inspect output
// that accompanies a failure.
func executeItemsAddToCollectionTestCmdWithFlags(t *testing.T, srv *collectionFilingTestServer, flags *rootFlags, args ...string) (string, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", srv.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	cmd := newItemsAddToCollectionCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestItemsAddToCollectionCreatesOnceAndIsIdempotent(t *testing.T) {
	srv := newCollectionFilingTestServer(t)

	first := runItemsAddToCollectionTestCmd(t, srv, "ITEMAB23", "--collection-name", "Inbox")
	if !first.OK || first.Mode != "apply" || first.Result == nil || first.Result.Summary.Applied != 2 {
		t.Fatalf("first filing = %+v, want the collection create and the membership both applied", first)
	}
	if srv.collectionCreates != 1 || srv.collectionKey != "CLCTAB23" || srv.itemPatchCount != 1 {
		t.Fatalf("first calls: creates=%d key=%q patches=%d", srv.collectionCreates, srv.collectionKey, srv.itemPatchCount)
	}
	if !stringSliceContains(srv.itemCollections, "EXISTAB2") || !stringSliceContains(srv.itemCollections, "CLCTAB23") {
		t.Fatalf("item collections = %v", srv.itemCollections)
	}

	second := runItemsAddToCollectionTestCmd(t, srv, "ITEMAB23", "--collection-name", "Inbox")
	if !second.OK || second.Mode != "apply" || second.Result == nil || second.Result.Summary.NoOp != 1 {
		t.Fatalf("second filing = %+v", second)
	}
	if srv.collectionCreates != 1 || srv.itemPatchCount != 1 {
		t.Fatalf("idempotent calls: creates=%d patches=%d", srv.collectionCreates, srv.itemPatchCount)
	}
}

func TestItemsAddToCollectionFindsCollectionOnLaterPage(t *testing.T) {
	srv := newCollectionFilingTestServer(t)
	srv.collectionsGet = func(w http.ResponseWriter, r *http.Request) {
		start, err := strconv.Atoi(r.URL.Query().Get("start"))
		if err != nil {
			t.Errorf("invalid start %q: %v", r.URL.Query().Get("start"), err)
			http.Error(w, "invalid start", http.StatusBadRequest)
			return
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit = %q, want 100", got)
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		switch start {
		case 0:
			rows := make([]map[string]any, 100)
			for i := range rows {
				rows[i] = map[string]any{
					"key":  fmt.Sprintf("OTHER%04d", i),
					"data": map[string]string{"name": fmt.Sprintf("Other %d", i)},
				}
			}
			if err := json.NewEncoder(w).Encode(rows); err != nil {
				t.Errorf("encoding first collection page: %v", err)
			}
		case 100:
			_, _ = fmt.Fprint(w, `[{"key":"CLCTAB24","data":{"name":"Inbox"}}]`)
		default:
			http.Error(w, "unexpected collection page", http.StatusBadRequest)
		}
	}

	env := runItemsAddToCollectionTestCmd(t, srv, "ITEMAB23", "--collection-name", "Inbox")
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("filing = %+v", env)
	}
	if srv.collectionCreates != 0 || srv.itemPatchCount != 1 {
		t.Fatalf("calls: creates=%d patches=%d", srv.collectionCreates, srv.itemPatchCount)
	}
	if !stringSliceContains(srv.itemCollections, "CLCTAB24") {
		t.Fatalf("item collections = %v", srv.itemCollections)
	}
}

func TestItemsAddToCollectionDoesNotCreateForInvalidItem(t *testing.T) {
	srv := newCollectionFilingTestServer(t)

	if _, err := executeItemsAddToCollectionTestCmd(t, srv, "MISSING", "--collection-name", "Inbox"); err == nil {
		t.Fatal("expected invalid item error")
	}
	if srv.collectionCreates != 0 {
		t.Fatalf("created %d collection(s) for an invalid item", srv.collectionCreates)
	}
}

func TestItemsAddToCollectionReconcilesWriteTokenRetry(t *testing.T) {
	fastRetryBackoff(t)
	srv := newCollectionFilingTestServer(t)
	srv.ambiguousCollectionCreate = true

	env := runItemsAddToCollectionTestCmd(t, srv, "ITEMAB23", "--collection-name", "Inbox")
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 2 {
		t.Fatalf("filing = %+v", env)
	}
	if srv.collectionCreates != 1 || srv.itemPatchCount != 1 {
		t.Fatalf("calls: creates=%d patches=%d", srv.collectionCreates, srv.itemPatchCount)
	}
	if len(srv.collectionWriteTokens) != 2 {
		t.Fatalf("write tokens = %v, want retry with same token", srv.collectionWriteTokens)
	}
	if srv.collectionWriteTokens[0] == "" || srv.collectionWriteTokens[0] != srv.collectionWriteTokens[1] {
		t.Fatalf("write tokens = %v, want non-empty deterministic token", srv.collectionWriteTokens)
	}
}

// Zotero answers any reuse of a write token with 412, deleted object or not.
// Re-creating a collection the operator deleted must not be refused as a
// "version conflict" that no re-run can clear.
func TestItemsAddToCollectionCreateAfterDeleteMintsANewToken(t *testing.T) {
	srv := newCollectionFilingTestServer(t)
	srv.rejectReusedTokens = true

	first := runItemsAddToCollectionTestCmd(t, srv, "ITEMAB23", "--collection-name", "Inbox")
	if !first.OK || srv.collectionCreates != 1 {
		t.Fatalf("first filing = %+v, creates=%d", first, srv.collectionCreates)
	}
	// The collection (and the membership it carried) is deleted in Zotero.
	srv.collectionKey, srv.collectionName = "", ""
	srv.itemCollections = []string{"EXISTAB2"}

	second := runItemsAddToCollectionTestCmd(t, srv, "ITEMAB23", "--collection-name", "Inbox")
	if !second.OK || second.Result == nil || second.Result.Summary.Applied != 2 {
		t.Fatalf("re-create filing = %+v, want the collection create and membership applied", second)
	}
	if srv.collectionCreates != 2 {
		t.Fatalf("collection creates = %d, want the deleted collection re-created", srv.collectionCreates)
	}
	if n := len(srv.collectionWriteTokens); n != 3 || srv.collectionWriteTokens[1] != srv.collectionWriteTokens[0] || srv.collectionWriteTokens[2] == srv.collectionWriteTokens[0] {
		t.Fatalf("write tokens = %v, want the original replayed once (412) and then a new token", srv.collectionWriteTokens)
	}
}

// decodeAddToCollectionEnvelope decodes the mutation envelope every
// add-to-collection run prints, preview and apply alike.
func decodeAddToCollectionEnvelope(t *testing.T, out string) mutation.Envelope {
	t.Helper()
	var env mutation.Envelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decoding mutation envelope %q: %v", out, err)
	}
	return env
}

// TestAddToCollectionPreviewsWithoutWriting: bare, --dry-run and --agent
// invocations print the mutation envelope as a preview and issue zero write
// requests, whether the named collection would be created or an existing one
// reused.
func TestAddToCollectionPreviewsWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags *rootFlags
	}{
		{"default", &rootFlags{asJSON: true, maxChanges: -1}},
		{"dry-run", &rootFlags{asJSON: true, yes: true, dryRun: true, maxChanges: -1}},
		{"agent", &rootFlags{asJSON: true, agent: true, maxChanges: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCollectionFilingTestServer(t)
			out, err := executeItemsAddToCollectionTestCmdWithFlags(t, srv, tc.flags, "ITEMAB23", "--collection-name", "Inbox")
			if err != nil {
				t.Fatalf("preview: %v; out=%s", err, out)
			}
			if srv.collectionCreates != 0 || srv.itemPatchCount != 0 {
				t.Fatalf("preview issued writes: creates=%d patches=%d", srv.collectionCreates, srv.itemPatchCount)
			}
			env := decodeAddToCollectionEnvelope(t, out)
			if !env.OK || env.Mode != "preview" || env.Result != nil || env.Operation != "items.add-to-collection" {
				t.Fatalf("preview envelope = %+v, want ok preview with no result", env)
			}
			if env.Plan.Summary.Planned != 2 || len(env.Plan.Operations) != 2 || env.Plan.Operations[0].Kind != "collection_create" {
				t.Fatalf("plan = %+v, want a collection create then the membership change", env.Plan)
			}
		})
	}

	// The collection already exists: the plan holds only the membership
	// change and names the existing key.
	existingSrv := newCollectionFilingTestServer(t)
	existingSrv.collectionKey, existingSrv.collectionName = "CLCTAB23", "Inbox"
	existingOut, err := executeItemsAddToCollectionTestCmdWithFlags(t, existingSrv, &rootFlags{asJSON: true, maxChanges: -1}, "ITEMAB23", "--collection-name", "Inbox")
	if err != nil {
		t.Fatalf("existing-collection preview: %v; out=%s", err, existingOut)
	}
	if existingSrv.collectionCreates != 0 || existingSrv.itemPatchCount != 0 {
		t.Fatalf("existing-collection preview issued writes: creates=%d patches=%d", existingSrv.collectionCreates, existingSrv.itemPatchCount)
	}
	reuse := decodeAddToCollectionEnvelope(t, existingOut)
	if reuse.Plan.Summary.Planned != 1 || len(reuse.Plan.Operations) != 1 || len(reuse.Plan.Operations[0].Changes) != 1 || reuse.Plan.Operations[0].Changes[0].Add != "CLCTAB23" {
		t.Fatalf("reuse plan = %+v, want one membership change adding CLCTAB23", reuse.Plan)
	}
}

// A preview that apply would refuse must say so, with the same exit code.
func TestAddToCollectionPreviewReportsGateRefusal(t *testing.T) {
	srv := newCollectionFilingTestServer(t)
	out, err := executeItemsAddToCollectionTestCmdWithFlags(t, srv, &rootFlags{asJSON: true, maxChanges: 1}, "ITEMAB23", "--collection-name", "Inbox")
	if err == nil {
		t.Fatalf("preview of a plan above --max-changes succeeded; out=%s", out)
	}
	env := decodeAddToCollectionEnvelope(t, out)
	if env.OK || env.Mode != "preview" || env.Error == nil || env.Error.Code != "max_changes_exceeded" {
		t.Fatalf("preview envelope = %+v, want ok:false with max_changes_exceeded", env)
	}
	if srv.collectionCreates != 0 || srv.itemPatchCount != 0 {
		t.Fatalf("refused preview issued writes: creates=%d patches=%d", srv.collectionCreates, srv.itemPatchCount)
	}
}

// A missing --collection-name is a usage error (exit 2), with or without --dry-run.
func TestAddToCollectionRequiresCollectionName(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		srv := newCollectionFilingTestServer(t)
		_, err := executeItemsAddToCollectionTestCmdWithFlags(t, srv, &rootFlags{asJSON: true, dryRun: dryRun, maxChanges: -1}, "ITEMAB23")
		if err == nil || ExitCode(err) != 2 {
			t.Fatalf("dryRun=%v: error = %v, exit=%d; want usage error exit 2", dryRun, err, ExitCode(err))
		}
	}
}

// TestAddToCollectionJournalsCollectionCreate: one invocation that creates the
// collection runs one mutation with two operations, and the journal records
// both writes in a single entry.
func TestAddToCollectionJournalsCollectionCreate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	srv := newCollectionFilingTestServer(t)
	out, err := executeItemsAddToCollectionTestCmdWithFlags(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, "ITEMAB23", "--collection-name", "Inbox")
	if err != nil {
		t.Fatalf("apply: %v; out=%s", err, out)
	}
	if srv.collectionCreates != 1 || srv.itemPatchCount != 1 {
		t.Fatalf("apply calls: creates=%d patches=%d", srv.collectionCreates, srv.itemPatchCount)
	}

	entries, err := mutation.ListEntries(helpersTestJournalDir(t))
	if err != nil {
		t.Fatalf("list journal entries: %v", err)
	}
	if len(entries) != 1 || entries[0].Operation != "items.add-to-collection" || entries[0].Summary.Applied != 2 || len(entries[0].Ops) != 2 {
		t.Fatalf("journal entries = %+v, want one items.add-to-collection entry with two applied ops", entries)
	}
	member := entries[0].Ops[1]
	if len(member.Revert) != 1 || member.Revert[0].Remove != "CLCTAB23" {
		t.Fatalf("membership op revert = %+v, want removal of the created collection key", member.Revert)
	}
}

// TestAddToCollectionHonorsMaxChanges guards the fix for the reviewer-flagged gap
// from the gate side: the direct PostWithHeaders call never consulted CheckGates,
// so --max-changes had no effect on the on-demand collection create. Routing it
// through runMutation must make a cap of zero refuse before any POST is issued —
// and before the item is ever moved.
func TestAddToCollectionHonorsMaxChanges(t *testing.T) {
	srv := newCollectionFilingTestServer(t)
	out, err := executeItemsAddToCollectionTestCmdWithFlags(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: 0}, "ITEMAB23", "--collection-name", "Inbox")
	if err == nil {
		t.Fatalf("expected max-changes refusal; out=%s", out)
	}
	if srv.collectionCreates != 0 {
		t.Fatalf("created %d collection(s) despite --max-changes 0", srv.collectionCreates)
	}
	if srv.itemPatchCount != 0 {
		t.Fatalf("patched the item despite --max-changes 0 refusing the collection create")
	}
}
