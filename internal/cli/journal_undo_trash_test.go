// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// journal undo for `items delete` (trash): the inverse is a restore PATCH.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"zotio/internal/store"
)

type trashPlane struct {
	mu      sync.Mutex
	version int
	deleted int
	patches []map[string]any
	headers []string
}

func (p *trashPlane) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Last-Modified-Version", strconv.Itoa(p.version))
		_ = json.NewEncoder(w).Encode(map[string]any{"key": "K", "version": p.version, "data": map[string]any{"deleted": p.deleted}})
	case http.MethodPatch:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.patches = append(p.patches, body)
		p.headers = append(p.headers, r.Header.Get("If-Unmodified-Since-Version"))
		p.deleted = int(body["deleted"].(float64))
		p.version++
		w.WriteHeader(http.StatusNoContent)
	}
}

func TestJournalUndoReversesItemsDelete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_API_KEY", "")
	p := &trashPlane{version: 10}
	srv := httptest.NewServer(http.HandlerFunc(p.serve))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL)

	db, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{json.RawMessage(`{"key":"K","version":10,"data":{"key":"K","itemType":"book","title":"T"}}`)}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	mutationJournalRecorder = recordMutationJournal
	t.Cleanup(func() { mutationJournalRecorder = nil })
	mirrorWriteThrough = applyMirrorWriteThrough
	t.Cleanup(func() { mirrorWriteThrough = nil })

	del := newItemsDeleteCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	del.SilenceErrors, del.SilenceUsage = true, true
	var out bytes.Buffer
	del.SetOut(&out)
	del.SetErr(&bytes.Buffer{})
	del.SetArgs([]string{"K"})
	if err := del.Execute(); err != nil {
		t.Fatalf("items delete: %v", err)
	}
	var denv struct {
		Journal map[string]any `json:"journal"`
	}
	if err := json.Unmarshal(out.Bytes(), &denv); err != nil {
		t.Fatal(err)
	}
	runID, _ := denv.Journal["run_id"].(string)
	if runID == "" {
		t.Fatalf("no run id: %s", out.String())
	}
	trashCount := func() int {
		d, err := store.OpenWithContext(context.Background(), helpersTestDefaultDBPath(t, "zotio"))
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		rows, err := localQueryStore{d}.QueryRaw("SELECT id FROM resources WHERE resource_type='items-trash' AND id=?", "K")
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	if trashCount() != 1 {
		t.Fatal("mirror should list K in items-trash after delete")
	}

	// Preview sends nothing.
	prev := newJournalCmd(&rootFlags{asJSON: true, maxChanges: -1})
	prev.SilenceErrors, prev.SilenceUsage = true, true
	prev.SetOut(&bytes.Buffer{})
	prev.SetErr(&bytes.Buffer{})
	prev.SetArgs([]string{"undo", runID})
	if err := prev.Execute(); err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(p.patches) != 1 {
		t.Fatalf("preview wrote: patches=%v", p.patches)
	}

	p.mu.Lock()
	p.version = 25 // moved since the trash: the guard must use the fresh version
	p.mu.Unlock()
	env := mustUndoJournalRun(t, runID)
	if env.Result == nil || env.Result.Summary.Applied != 1 {
		t.Fatalf("undo env = %+v", env)
	}
	if len(p.patches) != 2 || p.patches[1]["deleted"] != float64(0) || p.headers[1] != "25" {
		t.Fatalf("restore patch = %v headers=%v, want deleted=0 with version 25", p.patches, p.headers)
	}
	if trashCount() != 0 {
		t.Fatal("mirror should no longer list K in items-trash after undo")
	}

	undoJournal, _ := env.Journal.(map[string]any)
	earlierUndo, _ := undoJournal["run_id"].(string)
	if earlierUndo == "" {
		t.Fatalf("undo journal = %#v, want recorded run ID", env.Journal)
	}
	again := newJournalCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	again.SetArgs([]string{"undo", runID})
	again.SetOut(&bytes.Buffer{})
	again.SetErr(&bytes.Buffer{})
	if err := again.Execute(); ExitCode(err) != 9 || !bytes.Contains([]byte(err.Error()), []byte(earlierUndo)) {
		t.Fatalf("second undo error = %v, want exit 9 naming %s", err, earlierUndo)
	}
	if len(p.patches) != 2 {
		t.Fatalf("second undo sent a PATCH: %v", p.patches)
	}
}
