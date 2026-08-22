// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
//
// Pins the connector re-parent route: the only local route for attaching a file
// to an item that ALREADY exists, used when Zotero keeps its files somewhere
// other than its own cloud storage. The route and every fact it rests on were
// measured against a real WebDAV-backed library first; see
// dev/field-report-2026-08-22-papio-round2.md.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/connector"
)

// reparentFake stands in for both planes at once: Zotero's desktop connector
// and api.zotero.org. It records the ORDER of the writes, because the route's
// correctness depends on it — the attachment must leave the temporary parent
// before that parent is trashed.
type reparentFake struct {
	mu sync.Mutex

	// tempParentKey is what the connector's recovery lookup will report.
	tempParentKey string
	// attachChildren is returned for the temporary parent's children.
	attachChildren []string
	// childrenOfTarget is returned for the destination item's children.
	childrenOfTarget []map[string]any
	// targetItem is the destination item's JSON body.
	targetItem map[string]any

	// webVisibleAfter delays write-plane visibility of the attachment by this
	// many GETs, reproducing the 15-20s propagation lag from the desktop.
	webVisibleAfter int
	webGets         int

	// reparentStatus lets a test make the PATCH fail.
	reparentStatus int

	// calls records every mutating step in order.
	calls []string
	// patched records the bodies and version headers of each PATCH.
	patched []map[string]string
	// version is handed out for If-Unmodified-Since-Version checks.
	version int
	// createdTitle is the title the route actually generated, captured from
	// the saveItems body so the recovery lookup can match on it.
	createdTitle string
	// createdAbstract is the temporary parent's abstractNote, which carries the
	// self-describing marker once the title is borrowed from the target.
	createdAbstract string
	// attachMeta and attachBytes record what saveAttachment actually received.
	attachMeta  string
	attachBytes int
	// strandedParentKey simulates a previous run's orphan, for resume tests.
	strandedParentKey string
	// strandedChildMD5 is the content held under that orphan.
	strandedChildMD5 string
	// blockUntilCancelled makes item reads hang so a deadline can be exercised.
	blockUntilCancelled bool
	// targetGainsMD5After makes the target acquire targetGainedMD5 after N
	// children reads, simulating another run winning mid-route.
	targetGainsMD5After int
	targetGainedMD5     string
	targetChildReads    int
}

func (f *reparentFake) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *reparentFake) sequence() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *reparentFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	if f.version == 0 {
		f.version = 100
	}
	if f.targetItem == nil {
		f.targetItem = map[string]any{"itemType": "journalArticle", "title": "Real Paper"}
	}
	mux := http.NewServeMux()

	// --- desktop connector -------------------------------------------------
	mux.HandleFunc("/connector/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/connector/saveItems", func(w http.ResponseWriter, r *http.Request) {
		f.record("connector.saveItems")
		// Capture the title the route generated. confirmConnectorCreate matches
		// the recovery lookup on it, so the fake must echo back the real one
		// rather than a guess.
		var payload struct {
			Items []struct {
				Title        string `json:"title"`
				AbstractNote string `json:"abstractNote"`
			} `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil && len(payload.Items) > 0 {
			f.mu.Lock()
			f.createdTitle = payload.Items[0].Title
			f.createdAbstract = payload.Items[0].AbstractNote
			f.mu.Unlock()
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/connector/saveAttachment", func(w http.ResponseWriter, r *http.Request) {
		// Record what was actually sent. Counting the call alone let a wrong
		// session, a wrong parent, or an empty payload pass as success.
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.attachMeta = r.Header.Get("X-Metadata")
		f.attachBytes = len(body)
		f.mu.Unlock()
		f.record("connector.saveAttachment")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})

	// --- Zotero API (both planes point here) -------------------------------
	// findRecentlyAddedItemKey reads /items/top to recover the key of an item
	// the connector created without reporting one.
	mux.HandleFunc("/users/0/items/top", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Last-Modified-Version", fmt.Sprint(f.version))
		if f.tempParentKey == "" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		added := time.Now().UTC().Format("2006-01-02T15:04:05Z")
		rows := []map[string]any{{
			"key":       f.tempParentKey,
			"version":   f.version,
			"title":     f.currentTitle(),
			"itemType":  "document",
			"dateAdded": added,
			"data": map[string]any{
				"key":      f.tempParentKey,
				"itemType": "document",
				"title":    f.currentTitle(),
				// The marker is identity. Without it here, the marker lookup
				// cannot find the parent this run created.
				"abstractNote": f.currentAbstract(),
				"dateAdded":    added,
			},
		}}
		if f.strandedParentKey != "" {
			rows = append(rows, map[string]any{
				"key":     f.strandedParentKey,
				"version": f.version,
				"data": map[string]any{
					"key":      f.strandedParentKey,
					"itemType": "document",
					"title":    "Real Paper",
					// A PREVIOUS run's marker: different nonce, same target.
					"abstractNote": connectorTempParentMarker("deadbeefdeadbeef", "TARGET01"),
					"dateAdded":    added,
				},
			})
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	mux.HandleFunc("/users/0/items/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/users/0/items/")
		w.Header().Set("Last-Modified-Version", fmt.Sprint(f.version))

		switch {
		case strings.HasSuffix(path, "/children"):
			key := strings.TrimSuffix(path, "/children")
			if key == f.strandedParentKey && f.strandedParentKey != "" {
				_ = json.NewEncoder(w).Encode([]map[string]any{{
					"key":     "STRANDED1",
					"version": f.version,
					"data": map[string]any{
						"key": "STRANDED1", "itemType": "attachment",
						"md5": f.strandedChildMD5, "parentItem": f.strandedParentKey,
					},
				}})
				return
			}
			if key == f.tempParentKey {
				rows := make([]map[string]any, 0, len(f.attachChildren))
				for _, k := range f.attachChildren {
					rows = append(rows, map[string]any{
						"key":     k,
						"version": f.version,
						"data":    map[string]any{"key": k, "itemType": "attachment"},
					})
				}
				_ = json.NewEncoder(w).Encode(rows)
				return
			}
			if key != "TARGET01" {
				// An unknown parent must 404, or a wrong-key regression looks
				// like a valid read.
				f.record("children.unknown:" + key)
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not found"}`))
				return
			}
			f.mu.Lock()
			f.targetChildReads++
			reads := f.targetChildReads
			f.mu.Unlock()
			if f.targetGainedMD5 != "" && reads > f.targetGainsMD5After {
				_ = json.NewEncoder(w).Encode([]map[string]any{{
					"key":     "WINNER01",
					"version": f.version,
					"data": map[string]any{
						"key": "WINNER01", "itemType": "attachment", "md5": f.targetGainedMD5,
					},
				}})
				return
			}
			if f.childrenOfTarget == nil {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_ = json.NewEncoder(w).Encode(f.childrenOfTarget)
			return

		case r.Method == http.MethodPatch:
			body, _ := readAllBody(r)
			f.mu.Lock()
			f.patched = append(f.patched, map[string]string{
				"key":     path,
				"body":    body,
				"version": r.Header.Get("If-Unmodified-Since-Version"),
			})
			f.mu.Unlock()
			switch {
			case strings.Contains(body, "parentItem"):
				if f.reparentStatus != 0 {
					f.record("web.reparent.failed")
					w.WriteHeader(f.reparentStatus)
					return
				}
				f.record("web.reparent")
			case strings.Contains(body, "deleted"):
				f.record("web.trash:" + path)
			}
			w.WriteHeader(http.StatusNoContent)
			return

		case r.Method == http.MethodGet && f.blockUntilCancelled:
			<-r.Context().Done()
			return

		case r.Method == http.MethodGet:
			// The attachment is invisible on the write plane until the
			// propagation window closes.
			isAttachment := len(f.attachChildren) > 0 && path == f.attachChildren[0]
			if isAttachment {
				f.mu.Lock()
				f.webGets++
				seen := f.webGets
				f.mu.Unlock()
				if seen <= f.webVisibleAfter {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"message":"Not found"}`))
					return
				}
			}
			data := f.targetItem
			if isAttachment {
				data = map[string]any{
					"itemType": "attachment", "linkMode": "imported_url",
					"parentItem": f.tempParentKey,
				}
			} else if path == f.tempParentKey {
				// Carry the real marker: identity checks must pass for the item
				// this run created, and only for it.
				data = map[string]any{
					"itemType":     "document",
					"title":        f.currentTitle(),
					"abstractNote": f.currentAbstract(),
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"key":     path,
				"version": f.version,
				"data":    data,
			})
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *reparentFake) currentTitle() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createdTitle
}

func sameSequence(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func (f *reparentFake) savedAttachment() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attachMeta, f.attachBytes
}

func (f *reparentFake) currentAbstract() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createdAbstract
}

func readAllBody(r *http.Request) (string, error) {
	// io.ReadAll, not a single Read: a short read would silently misclassify a
	// PATCH and quietly weaken the ordering assertion this file exists to make.
	body, err := io.ReadAll(r.Body)
	return string(body), err
}

func reparentFlags(t *testing.T, srv *httptest.Server) *rootFlags {
	t.Helper()
	// Exercise the polling loop without spending real seconds in it.
	oldInterval := connectorReparentPollInterval
	connectorReparentPollInterval = time.Millisecond
	t.Cleanup(func() { connectorReparentPollInterval = oldInterval })
	old := connectorForCreate
	connectorForCreate = func(*rootFlags) (*connector.Client, error) {
		return connector.New(srv.URL+"/connector", 5*time.Second), nil
	}
	t.Cleanup(func() { connectorForCreate = old })
	return &rootFlags{
		configPath: testConfigFile(t, srv.URL+"/users/0"),
		via:        "connector",
		timeout:    5 * time.Second,
	}
}

// reparentCmd gives the route a command whose stderr the test can discard:
// the route warns there when the temporary parent cannot be trashed.
func reparentCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return cmd
}

func reparentRequest(t *testing.T, target string) storedUploadRequest {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "paper.pdf")
	if err := os.WriteFile(path, []byte("%PDF-1.4\nprobe\n%%EOF\n"), 0o600); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	req, err := newStoredUploadRequest(target, path, "")
	if err != nil {
		t.Fatalf("newStoredUploadRequest: %v", err)
	}
	return req
}

// TestConnectorReparentMovesAttachmentThenTrashesParent is the happy path, and
// it asserts the ORDER of the writes. Trashing the temporary parent before the
// attachment has left it risks taking the operator's file into the trash.
func TestConnectorReparentMovesAttachmentThenTrashesParent(t *testing.T) {
	fake := &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01"}}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, err := flags.newWriteClient()
	if err != nil {
		t.Fatalf("newWriteClient: %v", err)
	}

	status, detail, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, reparentRequest(t, "TARGET01"))
	if err != nil {
		t.Fatalf("route failed: %v (detail %v)", err, detail)
	}
	if status != "applied" {
		t.Fatalf("status = %q, want applied", status)
	}

	got := fake.sequence()
	want := []string{"connector.saveItems", "connector.saveAttachment", "web.reparent", "web.trash:TEMP0001"}
	if len(got) != len(want) {
		t.Fatalf("call sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call sequence = %v, want %v (differs at %d)", got, want, i)
		}
	}

	m, ok := detail.(map[string]any)
	if !ok {
		t.Fatalf("detail = %T, want map", detail)
	}
	if m["item_key"] != "ATTACH01" || m["parent_key"] != "TARGET01" {
		t.Errorf("detail = %v, want the attachment and target keys", m)
	}
	if m["temp_parent_trashed"] != true {
		t.Errorf("temp_parent_trashed = %v, want true", m["temp_parent_trashed"])
	}
}

// TestConnectorReparentGuardsThePatchWithAVersion pins the precondition header.
// Without it Zotero would accept a blind overwrite of an item that changed under
// us, and items_delete.go already records what an unguarded write costs.
func TestConnectorReparentGuardsThePatchWithAVersion(t *testing.T) {
	fake := &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01"}, version: 4242}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	if _, _, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, reparentRequest(t, "TARGET01")); err != nil {
		t.Fatalf("route failed: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	var sawReparent bool
	for _, p := range fake.patched {
		if !strings.Contains(p["body"], "parentItem") {
			continue
		}
		sawReparent = true
		if p["version"] != "4242" {
			t.Errorf("re-parent If-Unmodified-Since-Version = %q, want 4242", p["version"])
		}
		if !strings.Contains(p["body"], `"parentItem":"TARGET01"`) {
			t.Errorf("re-parent body = %q, want parentItem TARGET01", p["body"])
		}
	}
	if !sawReparent {
		t.Fatal("no re-parent PATCH was sent")
	}
}

// TestConnectorReparentWaitsForWritePlaneVisibility covers the lag this repo has
// already been bitten by: a connector-created key 404s on api.zotero.org for
// 15-20s. items_delete.go:78 records key SDLDFA9W being reported deleted inside
// that window and then materialising untrashed. The route must poll, not assume.
func TestConnectorReparentWaitsForWritePlaneVisibility(t *testing.T) {
	fake := &reparentFake{
		tempParentKey:   "TEMP0001",
		attachChildren:  []string{"ATTACH01"},
		webVisibleAfter: 2, // 404 twice, then appear
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	status, _, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, reparentRequest(t, "TARGET01"))
	if err != nil {
		t.Fatalf("route failed despite the item appearing: %v", err)
	}
	if status != "applied" {
		t.Fatalf("status = %q, want applied", status)
	}
	if fake.webGets <= 2 {
		t.Errorf("write-plane GETs = %d, want more than the 2 that 404ed", fake.webGets)
	}
}

// TestConnectorReparentRefusesAmbiguousAttachment pins the refusal to guess.
// Picking one of several candidates could attach a file to the wrong paper,
// which is exactly the failure confirmConnectorCreate also refuses to risk.
func TestConnectorReparentRefusesAmbiguousAttachment(t *testing.T) {
	fake := &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01", "ATTACH02"}}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	status, detail, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, reparentRequest(t, "TARGET01"))
	if err == nil {
		t.Fatal("route succeeded with two candidate attachments, want a refusal")
	}
	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	if !strings.Contains(err.Error(), "refusing to guess") {
		t.Errorf("error = %q, want it to refuse to guess", err)
	}
	// The temporary parent must still be named, or its litter is unfindable.
	m, _ := detail.(map[string]any)
	if m["temp_parent_key"] != "TEMP0001" {
		t.Errorf("detail = %v, want the temporary parent key so it can be cleaned up", m)
	}
	for _, call := range fake.sequence() {
		if strings.HasPrefix(call, "web.trash") {
			t.Error("trashed the temporary parent despite refusing to move the attachment")
		}
	}
}

// TestConnectorReparentKeepsParentWhenMoveFails pins the recovery contract: if
// the move fails the temporary parent must NOT be trashed, because the file is
// still its child and trashing it would hide the operator's file.
func TestConnectorReparentKeepsParentWhenMoveFails(t *testing.T) {
	fake := &reparentFake{
		tempParentKey:  "TEMP0001",
		attachChildren: []string{"ATTACH01"},
		reparentStatus: http.StatusPreconditionFailed,
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	_, detail, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, reparentRequest(t, "TARGET01"))
	if err == nil {
		t.Fatal("route succeeded despite a failing re-parent")
	}
	for _, call := range fake.sequence() {
		if strings.HasPrefix(call, "web.trash") {
			t.Fatal("trashed the temporary parent while the file was still its child")
		}
	}
	m, _ := detail.(map[string]any)
	if m["temp_parent_key"] != "TEMP0001" || m["item_key"] != "ATTACH01" {
		t.Errorf("detail = %v, want both keys so the operator can finish by hand", m)
	}
}

// TestConnectorReparentNoOpsWhenAlreadyAttached pins retry-safety against the
// TARGET, not the temporary parent. A re-run must not leave a second throwaway
// item and a duplicate file behind.
func TestConnectorReparentNoOpsWhenAlreadyAttached(t *testing.T) {
	req := reparentRequest(t, "TARGET01")
	fake := &reparentFake{
		tempParentKey:  "TEMP0001",
		attachChildren: []string{"ATTACH01"},
		// The live shape this route actually produces: Zotero renamed the file
		// after the PARENT's title, and the connector made it "imported_url".
		// Matching on filename or linkMode made every retry duplicate.
		childrenOfTarget: []map[string]any{{
			"key": "EXISTING1",
			"data": map[string]any{
				"key": "EXISTING1", "itemType": "attachment", "linkMode": "imported_url",
				"filename": "Real Paper.pdf", "md5": req.MD5,
			},
		}},
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	status, detail, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, req)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if status != "no_op" {
		t.Fatalf("status = %q, want no_op for an identical retry", status)
	}
	if m, _ := detail.(map[string]any); m["item_key"] != "EXISTING1" {
		t.Errorf("detail = %v, want the existing attachment key", m)
	}
	if calls := fake.sequence(); len(calls) != 0 {
		t.Errorf("calls = %v, want none: a retry must create nothing", calls)
	}
}

// TestConnectorReparentAttachesDifferentContentAlongside pins that a DIFFERENT
// file is not mistaken for a retry. This route never overwrites and the stored
// filename is derived from the target's title, so two different PDFs on one item
// is a legitimate outcome, not a conflict.
func TestConnectorReparentAttachesDifferentContentAlongside(t *testing.T) {
	req := reparentRequest(t, "TARGET01")
	fake := &reparentFake{
		tempParentKey:  "TEMP0001",
		attachChildren: []string{"ATTACH01"},
		childrenOfTarget: []map[string]any{{
			"key": "OTHER001",
			"data": map[string]any{
				"key": "OTHER001", "itemType": "attachment", "linkMode": "imported_url",
				"filename": "Real Paper.pdf", "md5": "ffffffffffffffffffffffffffffffff",
			},
		}},
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	status, detail, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, req)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if status != "applied" {
		t.Fatalf("status = %q, want applied: different content is a new attachment", status)
	}
	// A status alone cannot show the file was actually created and moved.
	want := []string{"connector.saveItems", "connector.saveAttachment", "web.reparent", "web.trash:TEMP0001"}
	if got := fake.sequence(); !sameSequence(got, want) {
		t.Errorf("call sequence = %v, want %v", got, want)
	}
	if m, _ := detail.(map[string]any); m["item_key"] != "ATTACH01" || m["parent_key"] != "TARGET01" {
		t.Errorf("detail = %v, want the new attachment on the target", m)
	}
}

// TestConnectorReparentIgnoresTrashedSibling pins that a trashed attachment does
// not count as "already attached": the operator removed it, so re-attaching is
// the requested outcome.
func TestConnectorReparentIgnoresTrashedSibling(t *testing.T) {
	req := reparentRequest(t, "TARGET01")
	fake := &reparentFake{
		tempParentKey:  "TEMP0001",
		attachChildren: []string{"ATTACH01"},
		childrenOfTarget: []map[string]any{{
			"key": "TRASHED1",
			"data": map[string]any{
				"key": "TRASHED1", "itemType": "attachment", "linkMode": "imported_url",
				"md5": req.MD5, "deleted": 1,
			},
		}},
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	status, detail, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, req)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if status != "applied" {
		t.Fatalf("status = %q, want applied: a trashed sibling is not already attached", status)
	}
	want := []string{"connector.saveItems", "connector.saveAttachment", "web.reparent", "web.trash:TEMP0001"}
	if got := fake.sequence(); !sameSequence(got, want) {
		t.Errorf("call sequence = %v, want %v", got, want)
	}
	if m, _ := detail.(map[string]any); m["item_key"] != "ATTACH01" {
		t.Errorf("detail = %v, want a newly created attachment", m)
	}
}

// TestVerifyReparentTargetRejectsIllegalParents pins the pre-create check.
// Zotero requires a regular top-level item, and discovering otherwise after the
// connector has written the file would leave an orphan.
func TestVerifyReparentTargetRejectsIllegalParents(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string]any
		want string
	}{
		{"attachment", map[string]any{"itemType": "attachment", "title": "T"}, "cannot hold child attachments"},
		{"note", map[string]any{"itemType": "note", "title": "T"}, "cannot hold child attachments"},
		{"child item", map[string]any{"itemType": "journalArticle", "parentItem": "OWNER001"}, "itself a child"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01"}, targetItem: tc.data}
			srv := fake.server(t)
			flags := reparentFlags(t, srv)
			c, _ := flags.newWriteClient()

			_, _, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, reparentRequest(t, "TARGET01"))
			if err == nil {
				t.Fatal("route succeeded against an illegal parent")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			if calls := fake.sequence(); len(calls) != 0 {
				t.Errorf("calls = %v, want none: the target is checked before anything is created", calls)
			}
		})
	}
}

// TestReparentAttachmentRefusesWithoutAVersion pins that the guard cannot be
// skipped by an empty version.
func TestReparentAttachmentRefusesWithoutAVersion(t *testing.T) {
	fake := &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01"}}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	if err := reparentAttachment(c, "ATTACH01", "TARGET01", 0); err == nil {
		t.Fatal("reparentAttachment accepted version 0, want a refusal")
	}
	if calls := fake.sequence(); len(calls) != 0 {
		t.Errorf("calls = %v, want none", calls)
	}
}

// TestConnectorReparentNamesTempParentAfterTarget pins the fix for a defect
// found by smoke-testing this route against a real library: Zotero renames a
// saved file after its PARENT item's title, at save time and never
// retroactively. A zotio-marker title therefore left the operator's PDF stored
// on disk as "[zotio] temporary attachment parent - safe to delete (...).pdf".
// Borrowing the target's title makes the stored filename match what a direct
// attach would have produced, and the marker moves to abstractNote so an
// orphaned temporary parent still explains itself.
func TestConnectorReparentNamesTempParentAfterTarget(t *testing.T) {
	fake := &reparentFake{
		tempParentKey:  "TEMP0001",
		attachChildren: []string{"ATTACH01"},
		targetItem:     map[string]any{"itemType": "journalArticle", "title": "Attention Is All You Need"},
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	if _, _, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, reparentRequest(t, "TARGET01")); err != nil {
		t.Fatalf("route failed: %v", err)
	}

	title := fake.currentTitle()
	if title != "Attention Is All You Need" {
		t.Errorf("temporary parent title = %q, want the target's title so the stored filename is right", title)
	}
	if strings.Contains(title, connectorTempParentPrefix) {
		t.Errorf("temporary parent title = %q, must not carry the marker: Zotero names the stored file from it", title)
	}
	note := fake.currentAbstract()
	if !strings.Contains(note, connectorTempParentPrefix) || !strings.Contains(note, "TARGET01") {
		t.Errorf("abstractNote = %q, want the marker and the target key so an orphan explains itself", note)
	}
}

// TestConnectorReparentRefusesToTrashAnUnmarkedItem is the backstop that makes
// the whole route safe. Reviewers found that borrowing the target's title let
// key recovery resolve to the TARGET, after which the route trashed the
// operator's real paper and reported "applied". The trash now refuses any item
// that does not carry this route's marker.
func TestConnectorReparentRefusesToTrashAnUnmarkedItem(t *testing.T) {
	fake := &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01"}}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	// An item with no marker: exactly what a mis-recovered key would name.
	err := trashTemporaryParent(context.Background(), c, "TARGET01", "somenonce", "OTHER001")
	if err == nil {
		t.Fatal("trashed an item that carries no marker")
	}
	if !strings.Contains(err.Error(), "marker") {
		t.Errorf("error = %q, want it to name the missing marker", err)
	}
	for _, call := range fake.sequence() {
		if strings.HasPrefix(call, "web.trash") {
			t.Fatal("a trash was issued for an unmarked item")
		}
	}
}

// TestConnectorReparentNeverTrashesTheTarget pins the belt-and-braces check: the
// target key can never be trashed by this route, whatever recovery returned.
func TestConnectorReparentNeverTrashesTheTarget(t *testing.T) {
	fake := &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01"}}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	err := trashTemporaryParent(context.Background(), c, "TARGET01", "n", "TARGET01")
	if err == nil || !strings.Contains(err.Error(), "target item") {
		t.Fatalf("error = %v, want a refusal naming the target", err)
	}
}

// TestConnectorReparentResolvesByMarkerNotTitle covers the identity change. The
// temporary parent's title is the target's, so only the marker distinguishes
// them.
func TestConnectorReparentResolvesByMarkerNotTitle(t *testing.T) {
	fake := &reparentFake{
		tempParentKey:  "TEMP0001",
		attachChildren: []string{"ATTACH01"},
		targetItem:     map[string]any{"itemType": "journalArticle", "title": "Shared Title"},
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	key, matched, err := findTemporaryParentByMarker(context.Background(), flags, "no-such-nonce", "TARGET01")
	if err != nil {
		t.Fatalf("marker lookup: %v", err)
	}
	if key != "" || matched != 0 {
		t.Errorf("lookup for an absent nonce returned (%q,%d), want no match", key, matched)
	}

	// Now run the route so the fake records the real nonce, and confirm the
	// lookup finds it by marker.
	if _, _, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, reparentRequest(t, "TARGET01")); err != nil {
		t.Fatalf("route failed: %v", err)
	}
	abstract := fake.currentAbstract()
	if !strings.Contains(abstract, connectorTempParentPrefix) || !strings.Contains(abstract, "for item TARGET01") {
		t.Errorf("abstractNote = %q, want the marker and the target key", abstract)
	}
	if title := fake.currentTitle(); title != "Shared Title" {
		t.Errorf("title = %q, want the target's title (Zotero names the stored file from it)", title)
	}
}

// TestConnectorReparentSendsTheFileToTheSession checks the connector call
// itself, not merely that it happened: a wrong session or an empty payload used
// to pass as success.
func TestConnectorReparentSendsTheFileToTheSession(t *testing.T) {
	fake := &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01"}}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()
	req := reparentRequest(t, "TARGET01")

	if _, _, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, req); err != nil {
		t.Fatalf("route failed: %v", err)
	}
	meta, n := fake.savedAttachment()
	if n == 0 {
		t.Error("saveAttachment received an empty payload")
	}
	if int64(n) != req.Size {
		t.Errorf("saveAttachment received %d bytes, want %d", n, req.Size)
	}
	if !strings.Contains(meta, "sessionID") {
		t.Errorf("X-Metadata = %q, want it to name the session", meta)
	}
}

// TestConnectorReparentAbandonsWhenAnotherRunWins pins the last-moment
// reconciliation. Two runs can both pass the opening check; the loser must not
// add a duplicate, and must take its own copy away with it.
func TestConnectorReparentAbandonsWhenAnotherRunWins(t *testing.T) {
	req := reparentRequest(t, "TARGET01")
	fake := &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01"}}
	// The target is empty at the opening check and gains the file mid-route.
	fake.targetGainsMD5After = 1
	fake.targetGainedMD5 = req.MD5
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	status, detail, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, req)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if status != "no_op" {
		t.Fatalf("status = %q, want no_op when another run won", status)
	}
	for _, call := range fake.sequence() {
		if call == "web.reparent" {
			t.Error("moved a duplicate onto the target after losing the race")
		}
	}
	if m, _ := detail.(map[string]any); m["temp_parent_trashed"] != true {
		t.Errorf("detail = %v, want the redundant temporary parent trashed", m)
	}
}

// TestConnectorReparentHonoursACancelledContext pins that a hung desktop cannot
// keep the route alive, and that the temporary parent stays findable when it is
// cut short.
func TestConnectorReparentHonoursACancelledContext(t *testing.T) {
	fake := &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01"}}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: no loop may proceed

	start := time.Now()
	_, _, err := applyConnectorReparentUpload(ctx, reparentCmd(t), flags, c, reparentRequest(t, "TARGET01"))
	if err == nil {
		t.Fatal("route succeeded with a cancelled context")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("took %s to honour cancellation", elapsed)
	}
	for _, call := range fake.sequence() {
		if strings.HasPrefix(call, "web.trash") {
			t.Error("trashed something while cancelled")
		}
	}
}

// TestConnectorRouteWaiverIsScoped pins the storage-guard waiver. The guard
// stops bytes being silently uploaded into Zotero's cloud and billed to the
// operator's plan; the connector route is waived from it because it never uses
// the Web uploader. Reviewers noted the waiver had no test at all, and that the
// predicate deciding it lived in two files that had to stay in step.
func TestConnectorRouteWaiverIsScoped(t *testing.T) {
	for _, tc := range []struct {
		name, mode, via string
		want            bool
	}{
		{"stored via connector takes the route", "stored", "connector", true},
		{"stored via web does not", "stored", "web", false},
		{"stored via auto does not: the route is opt-in", "stored", "auto", false},
		{"linked-file via connector does not: no upload at all", "linked-file", "connector", false},
		{"linked-file via web does not", "linked-file", "web", false},
		{"whitespace is not a route", "stored", " ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := usesConnectorReparentRoute(tc.mode, &rootFlags{via: tc.via})
			if got != tc.want {
				t.Errorf("usesConnectorReparentRoute(%q, via=%q) = %v, want %v", tc.mode, tc.via, got, tc.want)
			}
		})
	}
}
