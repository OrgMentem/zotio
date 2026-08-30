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

	// saveAttachmentStatus makes the connector's saveAttachment reply fail with
	// this status while the child it committed is controlled by attachChildren,
	// which is what a lost reply after a successful commit looks like.
	saveAttachmentStatus int

	// childDetached and childTrashed make the fake model what a re-parent and a
	// trash actually do to the temporary parent's child list. Without them the
	// fake reported the attachment as a live child forever, including after the
	// route moved it away, so a caller could not tell a cleaned-up parent from
	// one still holding the operator's file.
	childDetached map[string]bool
	childTrashed  map[string]bool

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
	// targetVisibleAfter makes the target 404 for its first N reads, as a
	// connector-created item does until it reaches the write plane.
	targetVisibleAfter int
	targetGets         int
	// targetGainsMD5After makes the target acquire targetGainedMD5 after N
	// children reads, simulating another run winning mid-route.
	targetGainsMD5After int
	targetGainedMD5     string
	// winnerParent is the parent the fake reports for WINNER01. Empty means the
	// target, i.e. the winner is where it was found. Setting it elsewhere models
	// another actor re-parenting the winner away between discovery and the
	// route's revalidation.
	winnerParent string
	// winnerTrashed makes the fake report WINNER01 as trashed.
	winnerTrashed    int
	targetChildReads int
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
		// saveAttachmentStatus models the reply being lost or refused. The child
		// it commits is controlled separately, because the whole hazard is that
		// the desktop commits BEFORE it answers: a failed reply says nothing
		// about whether the file landed.
		if f.saveAttachmentStatus != 0 {
			w.WriteHeader(f.saveAttachmentStatus)
			return
		}
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
			if key == "TARGET01" && f.targetVisibleAfter > 0 {
				f.mu.Lock()
				f.targetGets++
				seen := f.targetGets
				f.mu.Unlock()
				if seen <= f.targetVisibleAfter {
					// A target that has not propagated 404s on its children too,
					// not just on itself.
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"message":"Not found"}`))
					return
				}
			}
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
				f.mu.Lock()
				rows := make([]map[string]any, 0, len(f.attachChildren))
				for _, k := range f.attachChildren {
					if f.childDetached[k] {
						// Moved onto another parent, so no longer a child here.
						continue
					}
					data := map[string]any{"key": k, "itemType": "attachment"}
					if f.childTrashed[k] {
						data["deleted"] = 1
					}
					rows = append(rows, map[string]any{"key": k, "version": f.version, "data": data})
				}
				f.mu.Unlock()
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
				// The attachment now belongs to another parent, so it leaves the
				// temporary parent's child list. Modelling this is what lets a
				// test tell a cleaned-up parent from one still holding a file.
				f.mu.Lock()
				if f.childDetached == nil {
					f.childDetached = map[string]bool{}
				}
				f.childDetached[path] = true
				f.mu.Unlock()
				f.record("web.reparent")
			case strings.Contains(body, "deleted"):
				f.mu.Lock()
				if f.childTrashed == nil {
					f.childTrashed = map[string]bool{}
				}
				f.childTrashed[path] = true
				f.mu.Unlock()
				f.record("web.trash:" + path)
			}
			w.WriteHeader(http.StatusNoContent)
			return

		case r.Method == http.MethodGet && f.blockUntilCancelled:
			<-r.Context().Done()
			return

		case r.Method == http.MethodGet:
			if path == "TARGET01" {
				f.mu.Lock()
				f.targetGets++
				seen := f.targetGets
				f.mu.Unlock()
				if seen <= f.targetVisibleAfter {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"message":"Not found"}`))
					return
				}
			}
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
			} else if path == "WINNER01" {
				// The winner is an attachment on the TARGET, and the route now
				// re-asserts that parentage before destroying its own copy. A
				// fake that omitted parentItem made every abandon look like a
				// winner that had moved away. winnerParent lets a test model
				// exactly that drift.
				winnerParent := f.winnerParent
				if winnerParent == "" {
					winnerParent = "TARGET01"
				}
				data = map[string]any{
					"itemType": "attachment", "linkMode": "imported_url",
					"parentItem": winnerParent,
					"md5":        f.targetGainedMD5,
					"deleted":    f.winnerTrashed,
				}
			} else if path == f.strandedParentKey && f.strandedParentKey != "" {
				data = map[string]any{
					"itemType":     "document",
					"title":        "Real Paper",
					"abstractNote": connectorTempParentMarker("deadbeefdeadbeef", "TARGET01"),
				}
			} else if path == "STRANDED1" {
				data = map[string]any{
					"itemType": "attachment", "linkMode": "imported_url",
					"parentItem": f.strandedParentKey,
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

// TestConnectorReparentRestoresCallerContext pins client ownership. The route
// installs its own 5-minute deadline on the caller's write client so the polls'
// HTTP calls are bounded, then cancels it. Without restoring what it found, the
// op hands the caller back a client permanently bound to a CANCELLED context:
// every later request on it fails with context.Canceled, which looks like an
// unrelated transient error. Latent while `attachments add` builds one client
// per command; a second op in the same run, or a reconcile re-read added to
// runMutation, would break immediately.
func TestConnectorReparentRestoresCallerContext(t *testing.T) {
	// A value-bearing context, so the assertion is IDENTITY, not merely "some
	// context that is not cancelled": restoring context.Background() instead of
	// what the caller installed would also leave a usable client.
	type ctxKey struct{}
	for _, tc := range []struct {
		name        string
		fake        *reparentFake
		wantRouteOK bool
	}{
		{
			name:        "successful route",
			fake:        &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01"}},
			wantRouteOK: true,
		},
		{
			// The borrow is installed BEFORE runConnectorReparent, so a route
			// that fails partway must still hand the context back.
			name: "failing route",
			fake: &reparentFake{
				tempParentKey:  "TEMP0001",
				attachChildren: []string{"ATTACH01"},
				reparentStatus: http.StatusPreconditionFailed,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.fake.server(t)
			flags := reparentFlags(t, srv)
			c, err := flags.newWriteClient()
			if err != nil {
				t.Fatalf("newWriteClient: %v", err)
			}
			callerCtx := context.WithValue(context.Background(), ctxKey{}, "caller")
			c.SetContext(callerCtx)

			_, _, routeErr := applyConnectorReparentUpload(callerCtx, reparentCmd(t), flags, c, reparentRequest(t, "TARGET01"))
			if tc.wantRouteOK && routeErr != nil {
				t.Fatalf("route failed: %v", routeErr)
			}
			if !tc.wantRouteOK && routeErr == nil {
				t.Fatal("route succeeded, but this case must fail to exercise the error path")
			}

			if got := c.Context(); got != callerCtx {
				t.Fatalf("client base context = %v, want the caller's own context back (identity)", got)
			}
			if err := c.Context().Err(); err != nil {
				t.Fatalf("client base context is %v after the route; the borrow was never given back", err)
			}
			// The real proof: the client still works for a follow-up request.
			if _, _, err := c.GetWithVersion("/items/TARGET01", nil); err != nil {
				t.Fatalf("follow-up request on the caller's client failed: %v", err)
			}
		})
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

	if err := reparentAttachment(c, "ATTACH01", "TARGET01", "TEMP0001", "deadbeef", 0); err == nil {
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
	_, err := trashTemporaryParent(context.Background(), c, "TARGET01", "somenonce", "OTHER001")
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

	_, err := trashTemporaryParent(context.Background(), c, "TARGET01", "n", "TARGET01")
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

// TestConnectorReparentEmitsAnExplicitAttachmentKey pins a consumer contract
// papio depends on. It prefers detail.attachment_key and falls back to item_key;
// if a fallback key could ever name the TARGET, papio would record an item as
// its own attachment. So the attachment key is emitted explicitly, on both the
// applied and the no-op paths.
func TestConnectorReparentEmitsAnExplicitAttachmentKey(t *testing.T) {
	t.Run("applied", func(t *testing.T) {
		fake := &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01"}}
		srv := fake.server(t)
		flags := reparentFlags(t, srv)
		c, _ := flags.newWriteClient()

		_, detail, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, reparentRequest(t, "TARGET01"))
		if err != nil {
			t.Fatalf("route failed: %v", err)
		}
		m, _ := detail.(map[string]any)
		if m["attachment_key"] != "ATTACH01" {
			t.Errorf("attachment_key = %v, want ATTACH01", m["attachment_key"])
		}
		if m["attachment_key"] == m["parent_key"] {
			t.Error("attachment_key equals parent_key: a consumer would record the item as its own attachment")
		}
	})

	t.Run("no_op", func(t *testing.T) {
		req := reparentRequest(t, "TARGET01")
		fake := &reparentFake{
			tempParentKey:  "TEMP0001",
			attachChildren: []string{"ATTACH01"},
			childrenOfTarget: []map[string]any{{
				"key": "EXISTING1",
				"data": map[string]any{
					"key": "EXISTING1", "itemType": "attachment",
					"linkMode": "imported_url", "md5": req.MD5,
				},
			}},
		}
		srv := fake.server(t)
		flags := reparentFlags(t, srv)
		c, _ := flags.newWriteClient()

		status, detail, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, req)
		if err != nil || status != "no_op" {
			t.Fatalf("status=%q err=%v, want no_op", status, err)
		}
		m, _ := detail.(map[string]any)
		if m["attachment_key"] != "EXISTING1" {
			t.Errorf("attachment_key = %v, want EXISTING1 on the no-op too", m["attachment_key"])
		}
	})
}

// TestConnectorReparentResumesAnInterruptedRun covers the case papio asked the
// reviewers to consider: it kills zotio at a 120s deadline, so a run can die
// between the connector save and the move. That leaves a temporary parent
// holding real bytes. Content-hash reconciliation against the TARGET cannot see
// them, because they are attached to the orphan — so without adoption every
// retry would create another temporary parent and another copy.
func TestConnectorReparentResumesAnInterruptedRun(t *testing.T) {
	req := reparentRequest(t, "TARGET01")
	fake := &reparentFake{
		tempParentKey:     "TEMP0001",
		attachChildren:    []string{"ATTACH01"},
		strandedParentKey: "ORPHAN01",
		strandedChildMD5:  req.MD5,
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	status, detail, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, req)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if status != "applied" {
		t.Fatalf("status = %q, want applied: the orphan's file is moved, not re-created", status)
	}
	m, _ := detail.(map[string]any)
	if m["resumed"] != true {
		t.Errorf("detail = %v, want resumed=true", m)
	}
	if m["attachment_key"] != "STRANDED1" {
		t.Errorf("attachment_key = %v, want the orphan's existing child STRANDED1", m["attachment_key"])
	}
	if m["temp_parent_key"] != "ORPHAN01" {
		t.Errorf("temp_parent_key = %v, want the adopted orphan", m["temp_parent_key"])
	}
	// The decisive assertion: nothing new was created.
	for _, call := range fake.sequence() {
		if strings.HasPrefix(call, "connector.") {
			t.Errorf("called %s: a resumed run must not create a second parent or copy", call)
		}
	}
}

// TestConnectorReparentToleratesAFreshlyCreatedTarget covers a defect found by
// smoke-testing the release binary against a real library, in exactly the shape
// a consumer uses: create an item, then attach its PDF. An item created through
// the desktop connector takes 15-20s to reach the write plane, so the first read
// of a brand-new target legitimately 404s. Failing immediately made
// create-then-attach unusable.
func TestConnectorReparentToleratesAFreshlyCreatedTarget(t *testing.T) {
	fake := &reparentFake{
		tempParentKey:  "TEMP0001",
		attachChildren: []string{"ATTACH01"},
		// The target 404s twice, as a just-created item does, then appears.
		targetVisibleAfter: 2,
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, _ := flags.newWriteClient()

	status, _, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, reparentRequest(t, "TARGET01"))
	if err != nil {
		t.Fatalf("route failed on a target that was merely propagating: %v", err)
	}
	if status != "applied" {
		t.Fatalf("status = %q, want applied", status)
	}
	if fake.targetGets <= 2 {
		t.Errorf("target reads = %d, want more than the 2 that 404ed", fake.targetGets)
	}
}

// TestConfirmConnectorCreatePollsForItsKey covers a defect papio reported and
// this session reproduced: `items create --via connector` returned key null on a
// successful apply. SaveItems had already committed, so the item existed; it had
// simply not surfaced in /items/top yet, and the recovery looked exactly once.
//
// A consumer that treats an empty key as a failed apply re-derives the write and
// duplicates the item, so the one-shot lookup was a duplicate generator.
func TestConfirmConnectorCreatePollsForItsKey(t *testing.T) {
	oldWindow, oldInterval := connectorCreateRecoveryWindow, connectorCreateRecoveryInterval
	connectorCreateRecoveryWindow, connectorCreateRecoveryInterval = 2*time.Second, time.Millisecond
	t.Cleanup(func() {
		connectorCreateRecoveryWindow, connectorCreateRecoveryInterval = oldWindow, oldInterval
	})

	var gets int
	mux := http.NewServeMux()
	mux.HandleFunc("/users/0/items/top", func(w http.ResponseWriter, _ *http.Request) {
		gets++
		w.Header().Set("Last-Modified-Version", "1")
		if gets <= 2 {
			// Not surfaced yet: exactly what produced the empty key.
			_, _ = w.Write([]byte(`[]`))
			return
		}
		added := time.Now().UTC().Format("2006-01-02T15:04:05Z")
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"key":       "LATEKEY1",
			"title":     "Delayed Item",
			"itemType":  "document",
			"dateAdded": added,
			"data": map[string]any{
				"key": "LATEKEY1", "title": "Delayed Item",
				"itemType": "document", "dateAdded": added,
			},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	flags := &rootFlags{configPath: testConfigFile(t, srv.URL+"/users/0"), timeout: 5 * time.Second}
	item := map[string]any{"title": "Delayed Item", "itemType": "document"}

	key, matched, err := confirmConnectorCreate(t.Context(), flags, item, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("confirmConnectorCreate: %v", err)
	}
	if key != "LATEKEY1" || matched != 1 {
		t.Fatalf("key=%q matched=%d, want LATEKEY1/1 after the item surfaced", key, matched)
	}
	if gets <= 2 {
		t.Errorf("lookups = %d, want more than the 2 that returned empty", gets)
	}
}

// TestConfirmConnectorCreateDoesNotRetryAmbiguity pins that ambiguity returns at
// once. More than one match will not resolve itself, and waiting only delays a
// refusal that is already correct.
func TestConfirmConnectorCreateDoesNotRetryAmbiguity(t *testing.T) {
	oldWindow, oldInterval := connectorCreateRecoveryWindow, connectorCreateRecoveryInterval
	connectorCreateRecoveryWindow, connectorCreateRecoveryInterval = time.Minute, 50*time.Millisecond
	t.Cleanup(func() {
		connectorCreateRecoveryWindow, connectorCreateRecoveryInterval = oldWindow, oldInterval
	})

	var gets int
	mux := http.NewServeMux()
	mux.HandleFunc("/users/0/items/top", func(w http.ResponseWriter, _ *http.Request) {
		gets++
		w.Header().Set("Last-Modified-Version", "1")
		added := time.Now().UTC().Format("2006-01-02T15:04:05Z")
		row := func(k string) map[string]any {
			return map[string]any{
				"key": k, "title": "Twin", "itemType": "document", "dateAdded": added,
				"data": map[string]any{"key": k, "title": "Twin", "itemType": "document", "dateAdded": added},
			}
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{row("TWIN0001"), row("TWIN0002")})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	flags := &rootFlags{configPath: testConfigFile(t, srv.URL+"/users/0"), timeout: 5 * time.Second}
	start := time.Now()
	key, matched, err := confirmConnectorCreate(t.Context(), flags, map[string]any{"title": "Twin", "itemType": "document"}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("confirmConnectorCreate: %v", err)
	}
	if key != "" || matched != 2 {
		t.Fatalf("key=%q matched=%d, want no key and 2 matches", key, matched)
	}
	if gets != 1 {
		t.Errorf("lookups = %d, want exactly 1: ambiguity must not be retried", gets)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s: ambiguity must return at once", elapsed)
	}
}

// TestConfirmConnectorCreateHonorsCancellationDuringSleep pins the poll to the
// caller's context. The inter-probe sleep used context.Background(), so a
// cancellation arriving mid-sleep was only noticed on the NEXT probe: on the CLI
// Ctrl-C was delayed one interval per iteration, and under the MCP server a
// cancelled command_run still held its mirrored-command slot, blocking every
// later mirrored command behind acquireMirroredSlot.
func TestConfirmConnectorCreateHonorsCancellationDuringSleep(t *testing.T) {
	oldWindow, oldInterval := connectorCreateRecoveryWindow, connectorCreateRecoveryInterval
	connectorCreateRecoveryWindow, connectorCreateRecoveryInterval = time.Minute, 10*time.Second
	t.Cleanup(func() {
		connectorCreateRecoveryWindow, connectorCreateRecoveryInterval = oldWindow, oldInterval
	})

	// Cancel only AFTER the first probe has been fully served, so the run is
	// provably inside the inter-probe sleep. A fixed delay is not enough: if
	// cancellation lands before or during the first GET, the client's own
	// context aborts that request and even a context.Background() sleep returns
	// promptly, so the regression would slip through.
	probed := make(chan struct{})
	var probedOnce sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/users/0/items/top", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Last-Modified-Version", "1")
		// Never matches, so the loop always reaches the sleep.
		_, _ = w.Write([]byte(`[]`))
		probedOnce.Do(func() { close(probed) })
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan time.Duration, 1)
	go func() {
		flags := &rootFlags{configPath: testConfigFile(t, srv.URL+"/users/0"), timeout: 5 * time.Second}
		start := time.Now()
		_, _, err := confirmConnectorCreate(ctx, flags, map[string]any{"title": "Never", "itemType": "document"}, time.Now().Add(-time.Minute))
		if err == nil {
			t.Error("confirmConnectorCreate returned nil error after cancellation")
		}
		done <- time.Since(start)
	}()

	select {
	case <-probed:
	case <-time.After(30 * time.Second):
		t.Fatal("the first probe never reached the server")
	}
	// The handler has written its response, so the loop is in (or entering) the
	// 10s sleep. Give the client a moment to finish reading it, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancelledAt := time.Now()
	cancel()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("confirmConnectorCreate did not return; the sleep ignored the cancelled context")
	}
	// A cancellation-aware sleep wakes immediately. A context.Background() sleep
	// would run out the remainder of the 10s interval. Half an interval is a
	// wide margin that still fails the regression.
	if since := time.Since(cancelledAt); since >= connectorCreateRecoveryInterval/2 {
		t.Fatalf("returned %v after cancellation, want well under the %v interval: the sleep ignored the cancelled context", since, connectorCreateRecoveryInterval)
	}
}

// TestConnectorReparentAdoptsAChildAfterALostSaveReply is the regression test
// for the defect this route shipped with: the connector protocol has no
// endpoint that closes a save session, so a reply lost AFTER the desktop
// committed the child was reported as a plain failure. That stranded a live
// attachment under a live temporary parent, and the obvious retry attached the
// same bytes a second time onto the operator's item.
func TestConnectorReparentAdoptsAChildAfterALostSaveReply(t *testing.T) {
	// The reply fails, and the child exists anyway. That combination IS the bug.
	fake := &reparentFake{
		tempParentKey:        "TEMP0001",
		attachChildren:       []string{"ATTACH01"},
		saveAttachmentStatus: http.StatusInternalServerError,
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, err := flags.newWriteClient()
	if err != nil {
		t.Fatalf("newWriteClient: %v", err)
	}

	status, detail, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, reparentRequest(t, "TARGET01"))
	if err != nil {
		t.Fatalf("route failed after a lost save reply: %v (detail %v)", err, detail)
	}
	if status != "applied" {
		t.Fatalf("status = %q, want applied: the file was committed, so the run succeeded", status)
	}

	// The route must still complete the move and the cleanup, exactly once.
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

	d, ok := detail.(map[string]any)
	if !ok {
		t.Fatalf("detail is %T, want map[string]any", detail)
	}
	if d["attachment_key"] != "ATTACH01" {
		t.Errorf("attachment_key = %v, want ATTACH01: the adopted child must be named", d["attachment_key"])
	}
	// Adopting silently would hide a real desktop fault, so it must be reported.
	if d["save_reply_lost"] != true {
		t.Errorf("save_reply_lost = %v, want true: an adopted save must be visible", d["save_reply_lost"])
	}
}

// TestConnectorReparentReportsAGenuinelyFailedSave pins the other half: when
// nothing was committed, the original save error is the whole truth and must
// not be replaced by a reconcile error or masked as success.
func TestConnectorReparentReportsAGenuinelyFailedSave(t *testing.T) {
	// Reply fails and NO child exists — the save really did not land.
	fake := &reparentFake{
		tempParentKey:        "TEMP0001",
		attachChildren:       nil,
		saveAttachmentStatus: http.StatusInternalServerError,
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, err := flags.newWriteClient()
	if err != nil {
		t.Fatalf("newWriteClient: %v", err)
	}

	status, detail, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, reparentRequest(t, "TARGET01"))
	if err == nil {
		t.Fatalf("route succeeded with no file attached (status %q, detail %v)", status, detail)
	}
	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	// The operator needs the real cause, not the reconcile's own wording.
	if !strings.Contains(err.Error(), "did not attach") {
		t.Errorf("error = %q, want the original save failure", err)
	}

	// Nothing may be moved or trashed when no file was committed.
	for _, call := range fake.sequence() {
		if strings.HasPrefix(call, "web.reparent") || strings.HasPrefix(call, "web.trash") {
			t.Errorf("sequence = %v, want no move or trash after a failed save", fake.sequence())
			break
		}
	}

	d, ok := detail.(map[string]any)
	if !ok {
		t.Fatalf("detail is %T, want map[string]any", detail)
	}
	// The temporary parent must still be named so the litter is findable.
	if d["temp_parent_key"] != "TEMP0001" {
		t.Errorf("temp_parent_key = %v, want TEMP0001", d["temp_parent_key"])
	}
	if d["save_reply_lost"] == true {
		t.Error("save_reply_lost = true, but nothing was adopted")
	}
}

// TestAdoptLostConnectorSaveRefusesMultipleChildren pins that adoption never
// guesses. Two live children means something else wrote into this item, and
// picking one could move the wrong file onto the operator's paper.
func TestAdoptLostConnectorSaveRefusesMultipleChildren(t *testing.T) {
	fake := &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01", "ATTACH02"}}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)

	got, err := adoptLostConnectorSave(context.Background(), flags, "TEMP0001")
	if err == nil {
		t.Fatalf("adopted %q from two children, want a refusal", got)
	}
	if got != "" {
		t.Errorf("returned key %q alongside an error, want empty", got)
	}
	if !strings.Contains(err.Error(), "refusing to guess") {
		t.Errorf("error = %q, want it to name the refusal", err)
	}
}

// TestAdoptLostConnectorSaveIgnoresATrashedChild pins that an earlier run's
// cleaned-up litter is not mistaken for this run's file. Adopting a trashed
// attachment would move an item the operator had already discarded.
func TestAdoptLostConnectorSaveIgnoresATrashedChild(t *testing.T) {
	fake := &reparentFake{
		tempParentKey:  "TEMP0001",
		attachChildren: []string{"ATTACH01"},
		childTrashed:   map[string]bool{"ATTACH01": true},
	}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)

	oldInterval := connectorReparentPollInterval
	connectorReparentPollInterval = time.Millisecond
	t.Cleanup(func() { connectorReparentPollInterval = oldInterval })

	got, err := adoptLostConnectorSave(context.Background(), flags, "TEMP0001")
	if err != nil {
		t.Fatalf("adoptLostConnectorSave: %v", err)
	}
	if got != "" {
		t.Errorf("adopted trashed child %q, want no adoption", got)
	}
}

// TestAbandonFailsWhenTheWinnerLeftTheTarget is the regression test for the
// data-loss half of the abandon defect. The route discovers a winner by reading
// the TARGET's children, so the winner is on the target at that moment. Its
// revalidation then re-read the attachment standalone and checked only trashed
// and md5 — so a winner another actor re-parented elsewhere still validated, and
// the route trashed this run's copy on the strength of content that had already
// left the target, leaving the operator's item with nothing.
func TestAbandonFailsWhenTheWinnerLeftTheTarget(t *testing.T) {
	fake := &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01"}}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, err := flags.newWriteClient()
	if err != nil {
		t.Fatalf("newWriteClient: %v", err)
	}
	req := reparentRequest(t, "TARGET01")
	// The target is empty at the opening check and gains the file mid-route, so
	// the route reaches the abandon rather than short-circuiting at the top.
	fake.targetGainsMD5After = 1
	fake.targetGainedMD5 = req.MD5
	// The winner has moved off the target by the time the route revalidates.
	fake.winnerParent = "ELSEWHERE"

	status, detail, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, req)
	if err == nil {
		t.Fatalf("route reported success though the target lost the content (status %q, detail %v)", status, detail)
	}
	if status != "failed" {
		t.Fatalf("status = %q, want failed: the target holds no copy and this run did not put one there", status)
	}

	// Our own copy must survive: it is the only copy of the operator's file.
	for _, call := range fake.sequence() {
		if strings.HasPrefix(call, "web.trash") {
			t.Errorf("sequence = %v, want no trash: our copy is the only one left", fake.sequence())
			break
		}
	}

	d, ok := detail.(map[string]any)
	if !ok {
		t.Fatalf("detail is %T, want map[string]any", detail)
	}
	// The envelope must name OUR object, not the winner that walked away.
	if d["attachment_key"] != "ATTACH01" {
		t.Errorf("attachment_key = %v, want ATTACH01: the surviving object must be named", d["attachment_key"])
	}
	if d["temp_parent_key"] != "TEMP0001" {
		t.Errorf("temp_parent_key = %v, want TEMP0001", d["temp_parent_key"])
	}
}

// TestAbandonFailsWhenTheWinnerIsTrashed pins the same contract for the other
// way a winner can stop holding the content.
func TestAbandonFailsWhenTheWinnerIsTrashed(t *testing.T) {
	fake := &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01"}}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, err := flags.newWriteClient()
	if err != nil {
		t.Fatalf("newWriteClient: %v", err)
	}
	req := reparentRequest(t, "TARGET01")
	fake.targetGainsMD5After = 1
	fake.targetGainedMD5 = req.MD5
	fake.winnerTrashed = 1

	status, _, err := applyConnectorReparentUpload(context.Background(), reparentCmd(t), flags, c, req)
	if err == nil {
		t.Fatalf("route reported success though the winner was trashed (status %q)", status)
	}
	if status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
	for _, call := range fake.sequence() {
		if strings.HasPrefix(call, "web.trash") {
			t.Errorf("sequence = %v, want no trash after an aborted abandon", fake.sequence())
			break
		}
	}
}

// TestAbandonToWinnerRefusesAnUnnamedWinner pins the defensive branch. Its
// comment says it is never reached, which is an argument for keeping the guard,
// not for reporting success from it: trashing this run's copy with no winner
// leaves the operator's item empty.
func TestAbandonToWinnerRefusesAnUnnamedWinner(t *testing.T) {
	fake := &reparentFake{tempParentKey: "TEMP0001", attachChildren: []string{"ATTACH01"}}
	srv := fake.server(t)
	flags := reparentFlags(t, srv)
	c, err := flags.newWriteClient()
	if err != nil {
		t.Fatalf("newWriteClient: %v", err)
	}
	out := connectorReparentResult{AttachmentKey: "ATTACH01", TempParentKey: "TEMP0001"}

	got, err := abandonToWinner(context.Background(), reparentCmd(t), c, reparentRequest(t, "TARGET01"), out, "nonce", "")
	if err == nil {
		t.Fatal("abandonToWinner accepted an unnamed winner, want a refusal")
	}
	if got.RaceLost {
		t.Error("RaceLost = true with no winner named")
	}
	if got.AttachmentKey != "ATTACH01" {
		t.Errorf("AttachmentKey = %q, want ATTACH01: this run's own object must stay named", got.AttachmentKey)
	}
	for _, call := range fake.sequence() {
		if strings.HasPrefix(call, "web.trash") {
			t.Errorf("sequence = %v, want nothing trashed", fake.sequence())
			break
		}
	}
}
