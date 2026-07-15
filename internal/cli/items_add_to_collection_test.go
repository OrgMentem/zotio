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
	collectionsGet            func(http.ResponseWriter, *http.Request)
	itemCollections           []string
	itemPatchCount            int
}

func newCollectionFilingTestServer(t *testing.T) *collectionFilingTestServer {
	t.Helper()
	ts := &collectionFilingTestServer{collectionKey: "", itemCollections: []string{"EXISTING"}}
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
				ts.collectionWriteTokens = append(ts.collectionWriteTokens, r.Header.Get("Zotero-Write-Token"))
				if ts.ambiguousCollectionCreate && ts.collectionCreates > 0 {
					http.Error(w, `{"error":"write token already submitted"}`, http.StatusPreconditionFailed)
					return
				}
				ts.collectionCreates++
				ts.collectionKey, ts.collectionName = "COLL0001", name
				if ts.ambiguousCollectionCreate {
					http.Error(w, "response lost after create", http.StatusInternalServerError)
					return
				}
				_, _ = fmt.Fprint(w, `{"success":{"0":"COLL0001"},"successful":{"0":{"key":"COLL0001"}}}`)
			default:
				http.Error(w, "unexpected collection method", http.StatusMethodNotAllowed)
			}
		case "/users/0/items/ITEM0001":
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Last-Modified-Version", "17")
				_, _ = fmt.Fprintf(w, `{"key":"ITEM0001","version":17,"data":{"collections":%s}}`, mustJSON(t, ts.itemCollections))
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

func TestItemsAddToCollectionCreatesOnceAndIsIdempotent(t *testing.T) {
	srv := newCollectionFilingTestServer(t)

	first := runItemsAddToCollectionTestCmd(t, srv, "ITEM0001", "--collection-name", "Inbox")
	if !first.OK || first.Mode != "apply" || first.Result == nil || first.Result.Summary.Applied != 1 {
		t.Fatalf("first filing = %+v", first)
	}
	if srv.collectionCreates != 1 || srv.collectionKey != "COLL0001" || srv.itemPatchCount != 1 {
		t.Fatalf("first calls: creates=%d key=%q patches=%d", srv.collectionCreates, srv.collectionKey, srv.itemPatchCount)
	}
	if !stringSliceContains(srv.itemCollections, "EXISTING") || !stringSliceContains(srv.itemCollections, "COLL0001") {
		t.Fatalf("item collections = %v", srv.itemCollections)
	}

	second := runItemsAddToCollectionTestCmd(t, srv, "ITEM0001", "--collection-name", "Inbox")
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
			_, _ = fmt.Fprint(w, `[{"key":"COLL0002","data":{"name":"Inbox"}}]`)
		default:
			http.Error(w, "unexpected collection page", http.StatusBadRequest)
		}
	}

	env := runItemsAddToCollectionTestCmd(t, srv, "ITEM0001", "--collection-name", "Inbox")
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("filing = %+v", env)
	}
	if srv.collectionCreates != 0 || srv.itemPatchCount != 1 {
		t.Fatalf("calls: creates=%d patches=%d", srv.collectionCreates, srv.itemPatchCount)
	}
	if !stringSliceContains(srv.itemCollections, "COLL0002") {
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
	srv := newCollectionFilingTestServer(t)
	srv.ambiguousCollectionCreate = true

	env := runItemsAddToCollectionTestCmd(t, srv, "ITEM0001", "--collection-name", "Inbox")
	if !env.OK || env.Result == nil || env.Result.Summary.Applied != 1 {
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
