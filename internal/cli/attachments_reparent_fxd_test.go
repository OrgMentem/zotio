// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"zotio/internal/mutation"
)

// fxdReaperFake serves both planes for reapAbandonedTemporaryParents: recent
// top-level items, each item's body, its children, and the trash PATCH.
type fxdReaperFake struct {
	mu sync.Mutex
	// items maps a key to its `data` object; every item is listed by /items/top.
	items map[string]map[string]any
	// noteAfterParentRead makes a live note appear under the parent once its
	// body is read, which is the read that begins the trash: another actor
	// adding a note after the reaper's empty-child checks.
	noteAfterParentRead bool
	noteAdded           bool
	trashed             []string
}

func (f *fxdReaperFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Last-Modified-Version", "100")
		path := strings.TrimPrefix(r.URL.Path, "/users/0/items/")
		switch {
		case r.URL.Path == "/users/0/items/top":
			rows := []map[string]any{}
			for key, data := range f.items {
				rows = append(rows, map[string]any{"key": key, "version": 100, "data": data})
			}
			_ = json.NewEncoder(w).Encode(rows)
		case strings.HasSuffix(path, "/children"):
			rows := []map[string]any{}
			if f.noteAdded && r.URL.Query().Get("itemType") == "" {
				rows = append(rows, map[string]any{"key": "NOTE0001", "version": 100, "data": map[string]any{
					"key": "NOTE0001", "itemType": "note", "note": "added by someone else",
				}})
			}
			_ = json.NewEncoder(w).Encode(rows)
		case r.Method == http.MethodPatch:
			body, _ := readAllBody(r)
			if strings.Contains(body, "deleted") {
				f.trashed = append(f.trashed, path)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet:
			data, ok := f.items[path]
			if !ok {
				http.Error(w, `{"message":"Not found"}`, http.StatusNotFound)
				return
			}
			if f.noteAfterParentRead {
				f.noteAdded = true
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"key": path, "version": 100, "data": data})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fxdReaperFake) trashCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.trashed...)
}

func fxdOldDocument(abstract string) map[string]any {
	return map[string]any{
		"itemType":     "document",
		"title":        "Real Paper",
		"abstractNote": abstract,
		"dateAdded":    time.Now().Add(-connectorTempParentAbandonedAfter - time.Hour).UTC().Format(time.RFC3339),
	}
}

func fxdReap(t *testing.T, fake *fxdReaperFake) ([]string, error) {
	t.Helper()
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, err := flags.newWriteClient()
	if err != nil {
		t.Fatalf("newWriteClient: %v", err)
	}
	return reapAbandonedTemporaryParents(context.Background(), flags, c, "TARGET01", time.Now())
}

// The reaper trashes old childless documents it believes zotio created. An
// ordinary document whose abstract merely quotes the marker prefix and
// "for item <target>", or carries the marker text around a malformed nonce,
// was never written by zotio and must never be trashed.
func TestFxdReaperTrashesOnlyTheExactMarker(t *testing.T) {
	fake := &fxdReaperFake{items: map[string]map[string]any{
		"DOC00001": fxdOldDocument("Field notes on the " + connectorTempParentPrefix + " for item TARGET01 pattern seen in other tools."),
		"DOC00002": fxdOldDocument(connectorTempParentMarker("not-a-nonce", "TARGET01")),
		"TEMP0001": fxdOldDocument(connectorTempParentMarker("0123456789abcdef", "TARGET01")),
	}}

	reaped, err := fxdReap(t, fake)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if !slices.Equal(reaped, []string{"TEMP0001"}) || !slices.Equal(fake.trashCalls(), []string{"TEMP0001"}) {
		t.Fatalf("reaped = %v, trash PATCHes = %v; want only the exact zotio marker trashed", reaped, fake.trashCalls())
	}
}

// A note added to an abandoned parent after the reaper found it empty belongs
// to someone else. Trashing the parent then would leave that note live under a
// trashed parent, so the reaper must refuse instead.
func TestFxdReaperRefusesANoteAddedAfterItsEmptyCheck(t *testing.T) {
	fake := &fxdReaperFake{
		items: map[string]map[string]any{
			"TEMP0001": fxdOldDocument(connectorTempParentMarker("0123456789abcdef", "TARGET01")),
		},
		noteAfterParentRead: true,
	}

	reaped, err := fxdReap(t, fake)
	if err == nil {
		t.Fatalf("reap succeeded (reaped %v), want a refusal naming the new child", reaped)
	}
	if len(reaped) != 0 || len(fake.trashCalls()) != 0 {
		t.Fatalf("reaped = %v, trash PATCHes = %v; want nothing trashed", reaped, fake.trashCalls())
	}
}

// SaveItems can commit the temporary parent and still answer 500. When neither
// the create route nor the marker lookup can settle it, the attachment route
// must report a committed conflict carrying the connector session, and the
// journal must record it, so a rerun is not treated as a clean first attempt.
func TestFxdConnectorReparentUnresolvedTempParentIsJournaledConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })

	var mu sync.Mutex
	topReads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "100")
		switch r.URL.Path {
		case "/connector/ping":
			w.WriteHeader(http.StatusOK)
		case "/connector/saveItems":
			http.Error(w, `{"error":"save failed"}`, http.StatusInternalServerError)
		case "/users/0/items/top":
			mu.Lock()
			topReads++
			first := topReads == 1
			mu.Unlock()
			if first {
				// The resume lookup before the create finds nothing to adopt.
				_, _ = w.Write([]byte(`[]`))
				return
			}
			// Every lookup after SaveItems fails, so nothing settles it.
			http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)
		case "/users/0/items/TARGET01/children":
			_, _ = w.Write([]byte(`[]`))
		case "/users/0/items/TARGET01":
			_, _ = w.Write([]byte(`{"key":"TARGET01","version":100,"data":{"key":"TARGET01","itemType":"journalArticle","title":"Real Paper"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	flags := reparentFlags(t, srv)
	flags.asJSON, flags.yes, flags.maxChanges = true, true, -1
	c, err := flags.newWriteClient()
	if err != nil {
		t.Fatalf("newWriteClient: %v", err)
	}
	req := reparentRequest(t, "TARGET01")
	ops := []mutation.Op{{
		ID:      "attachments.add:001:stored",
		Key:     "TARGET01",
		Kind:    "attachment_upload",
		Changes: []mutation.Change{{Field: "attachment", Add: "stored via desktop connector"}},
		Apply: func() (string, any, error) {
			return applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, req)
		},
	}}
	env, runErr := runMutation(context.Background(), flags, "attachments.add", ops)
	if runErr == nil {
		t.Fatal("route succeeded, want the unresolved temporary parent reported")
	}
	if env.Result == nil || len(env.Result.Items) != 1 {
		t.Fatalf("result = %+v, want one item", env.Result)
	}
	item := env.Result.Items[0]
	reason, _ := item.Reason.(map[string]any)
	session, _ := reason["session"].(string)
	connKey, _ := reason["connector_key"].(string)
	if item.Status != "conflict" || reason["committed"] != true || session == "" || connKey == "" {
		t.Fatalf("item = %+v, want a committed conflict with connector session evidence", item)
	}

	dir, err := journalDir()
	if err != nil {
		t.Fatalf("journalDir: %v", err)
	}
	entries, err := mutation.ListEntries(dir)
	if err != nil || len(entries) != 1 || len(entries[0].Ops) != 1 {
		t.Fatalf("journal entries = %+v (err %v), want the one unresolved create", entries, err)
	}
	op := entries[0].Ops[0]
	recorded, _ := op.Reason.(map[string]any)
	if op.Status != "conflict" || recorded["committed"] != true || recorded["session"] != session {
		t.Fatalf("journal op = %+v, want the committed conflict and its session", op)
	}
}
