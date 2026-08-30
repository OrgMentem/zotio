// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"zotio/internal/client"
)

// The cleanup guards in this file all defend one fact about Zotero's server:
// trashing a parent does NOT trash its children. Measured on 2026-08-30, where
// three probe runs each left a live attachment beneath a trashed parent, still
// holding its stored bytes, and only a revert detector noticed.
//
// Absence of a cascade is also what makes the re-parent route safe, so the same
// fact obliges the route to clean up both objects by hand. These tests pin the
// obligation at the chokepoint every path funnels through.

// cleanupFake serves the two reads the cleanup guards make: the item itself and
// its attachment children. Each is programmable per key so a test can express
// one interleaving without describing the whole route.
type cleanupFake struct {
	// items maps a key to its `data` object. A missing key 404s.
	items map[string]map[string]any
	// children maps a parent key to its child rows. A key absent from this map
	// 404s on /children, which is what a parent still propagating looks like.
	children map[string][]map[string]any
	// itemVisibleAfter makes a key 404 for its first N reads.
	itemVisibleAfter map[string]int
	itemGets         map[string]int
	// patched records every PATCH as "<key>:<body>".
	patched []string
	version int
}

func (f *cleanupFake) start(t *testing.T) *httptest.Server {
	t.Helper()
	if f.version == 0 {
		f.version = 500
	}
	if f.itemGets == nil {
		f.itemGets = map[string]int{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/users/0/items/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/users/0/items/")
		w.Header().Set("Last-Modified-Version", fmt.Sprint(f.version))

		if key, ok := strings.CutSuffix(path, "/children"); ok {
			rows, present := f.children[key]
			if !present {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not found"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(rows)
			return
		}

		if r.Method == http.MethodPatch {
			body, _ := readAllBody(r)
			f.patched = append(f.patched, path+":"+body)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		f.itemGets[path]++
		if f.itemGets[path] <= f.itemVisibleAfter[path] {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not found"}`))
			return
		}
		data, ok := f.items[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"key": path, "version": f.version, "data": data})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *cleanupFake) client(t *testing.T, srv *httptest.Server) *client.Client {
	t.Helper()
	flags := reparentFlags(t, srv)
	c, err := flags.newWriteClient()
	if err != nil {
		t.Fatalf("newWriteClient: %v", err)
	}
	return c
}

// tempParent builds a temporary-parent item body carrying the run marker, which
// the identity check requires before it will trash anything.
func tempParent(nonce, targetKey string, trashed bool) map[string]any {
	data := map[string]any{
		"itemType":     "document",
		"title":        "Real Paper",
		"abstractNote": connectorTempParentMarker(nonce, targetKey),
	}
	if trashed {
		data["deleted"] = 1
	}
	return data
}

func attachmentRow(key, parent, md5 string, deleted int) map[string]any {
	return map[string]any{
		"key":     key,
		"version": 500,
		"data": map[string]any{
			"key": key, "itemType": "attachment", "parentItem": parent,
			"md5": md5, "deleted": deleted,
		},
	}
}

const cleanupNonce = "abcdef0123456789"

// TestTrashTemporaryParentRefusesWhileALiveChildRemains is the core guard. A
// caller that forgot to move or trash the attachment first would otherwise
// orphan it, and the failure is invisible: the PATCH succeeds and the child
// silently stays live.
func TestTrashTemporaryParentRefusesWhileALiveChildRemains(t *testing.T) {
	f := &cleanupFake{
		items:    map[string]map[string]any{"TEMP0001": tempParent(cleanupNonce, "TARGET01", false)},
		children: map[string][]map[string]any{"TEMP0001": {attachmentRow("ATTACH01", "TEMP0001", "deadbeef", 0)}},
	}
	srv := f.start(t)
	c := f.client(t, srv)

	err := trashTemporaryParent(context.Background(), c, "TEMP0001", cleanupNonce, "TARGET01")
	if err == nil {
		t.Fatal("trashTemporaryParent trashed a parent still holding a live attachment")
	}
	if !strings.Contains(err.Error(), "ATTACH01") {
		t.Errorf("error = %v, want it to name the attachment that would be orphaned", err)
	}
	if len(f.patched) != 0 {
		t.Errorf("patched = %v, want none: the refusal must happen before any write", f.patched)
	}
}

// TestTrashTemporaryParentIgnoresATrashedChild pins the other half: a child
// already in the trash is reversible and reachable, so it must not block
// cleanup. Without this the route could never finish after its own cleanup.
func TestTrashTemporaryParentIgnoresATrashedChild(t *testing.T) {
	f := &cleanupFake{
		items:    map[string]map[string]any{"TEMP0001": tempParent(cleanupNonce, "TARGET01", false)},
		children: map[string][]map[string]any{"TEMP0001": {attachmentRow("ATTACH01", "TEMP0001", "deadbeef", 1)}},
	}
	srv := f.start(t)
	c := f.client(t, srv)

	if err := trashTemporaryParent(context.Background(), c, "TEMP0001", cleanupNonce, "TARGET01"); err != nil {
		t.Fatalf("trashTemporaryParent refused despite the only child being trashed: %v", err)
	}
	if len(f.patched) != 1 || !strings.Contains(f.patched[0], "TEMP0001") {
		t.Errorf("patched = %v, want exactly the temporary parent trashed", f.patched)
	}
}

// TestTrashTemporaryParentFailsClosedWhenChildrenCannotBeListed pins that a
// read it cannot complete is not read as "no children". A 404 on /children can
// mean the edge is still propagating, and treating that as permission to trash
// is how a child surfaces afterwards beneath a trashed parent.
func TestTrashTemporaryParentFailsClosedWhenChildrenCannotBeListed(t *testing.T) {
	f := &cleanupFake{
		items: map[string]map[string]any{"TEMP0001": tempParent(cleanupNonce, "TARGET01", false)},
		// No entry for TEMP0001, so /children 404s.
		children: map[string][]map[string]any{},
	}
	srv := f.start(t)
	c := f.client(t, srv)

	err := trashTemporaryParent(context.Background(), c, "TEMP0001", cleanupNonce, "TARGET01")
	if err == nil {
		t.Fatal("trashTemporaryParent trashed a parent whose children it could not list")
	}
	if !strings.Contains(err.Error(), "cannot confirm") {
		t.Errorf("error = %v, want it to say the check could not be confirmed", err)
	}
	if len(f.patched) != 0 {
		t.Errorf("patched = %v, want none", f.patched)
	}
}

// TestTrashTemporaryParentDoesNotReportSuccessForATrashedParentWithALiveChild
// pins the ordering of the guard against the already-trashed short-circuit.
// Returning early there would report success over exactly the broken state this
// guard exists to detect: a live attachment beneath a trashed parent, which is
// unreachable from the trash and still holds its bytes.
func TestTrashTemporaryParentDoesNotReportSuccessForATrashedParentWithALiveChild(t *testing.T) {
	f := &cleanupFake{
		items:    map[string]map[string]any{"TEMP0001": tempParent(cleanupNonce, "TARGET01", true)},
		children: map[string][]map[string]any{"TEMP0001": {attachmentRow("ATTACH01", "TEMP0001", "deadbeef", 0)}},
	}
	srv := f.start(t)
	c := f.client(t, srv)

	err := trashTemporaryParent(context.Background(), c, "TEMP0001", cleanupNonce, "TARGET01")
	if err == nil {
		t.Fatal("trashTemporaryParent reported success for an already-trashed parent still holding a live child")
	}
	if !strings.Contains(err.Error(), "ATTACH01") {
		t.Errorf("error = %v, want it to name the orphaned attachment", err)
	}
}

// TestTrashRedundantAttachmentPollsThroughPropagationLag pins that a 404 is
// treated as lag, not as "already gone". The connector creates the attachment on
// the desktop and the write plane sees it seconds later; a cleanup that read the
// 404 as absence would release the temporary parent for trashing and let the
// attachment surface afterwards as a live orphan.
func TestTrashRedundantAttachmentPollsThroughPropagationLag(t *testing.T) {
	f := &cleanupFake{
		items:            map[string]map[string]any{"ATTACH01": {"itemType": "attachment", "parentItem": "TEMP0001", "md5": "deadbeef"}},
		itemVisibleAfter: map[string]int{"ATTACH01": 2},
	}
	srv := f.start(t)
	c := f.client(t, srv)

	if err := trashRedundantAttachment(context.Background(), c, "ATTACH01", "TEMP0001", "deadbeef"); err != nil {
		t.Fatalf("trashRedundantAttachment gave up on a propagating attachment: %v", err)
	}
	if len(f.patched) != 1 || !strings.Contains(f.patched[0], "ATTACH01") {
		t.Errorf("patched = %v, want the attachment trashed once it appeared", f.patched)
	}
}

// TestTrashRedundantAttachmentReportsAnAttachmentThatNeverAppears pins that
// exhausting the window is an error, not a silent success. The caller must be
// able to warn the operator and leave the temporary parent alone.
func TestTrashRedundantAttachmentReportsAnAttachmentThatNeverAppears(t *testing.T) {
	f := &cleanupFake{
		items:            map[string]map[string]any{"ATTACH01": {"itemType": "attachment", "parentItem": "TEMP0001"}},
		itemVisibleAfter: map[string]int{"ATTACH01": 1 << 30},
	}
	srv := f.start(t)
	c := f.client(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	err := trashRedundantAttachment(ctx, c, "ATTACH01", "TEMP0001", "deadbeef")
	if err == nil {
		t.Fatal("trashRedundantAttachment reported success for an attachment it never saw")
	}
	if !strings.Contains(err.Error(), "TEMP0001") {
		t.Errorf("error = %v, want it to name the temporary parent the attachment may surface beneath", err)
	}
	if len(f.patched) != 0 {
		t.Errorf("patched = %v, want none", f.patched)
	}
}

// TestTrashRedundantAttachmentRefusesAForeignParent pins the identity check. An
// attachment that escaped onto another parent belongs to somebody else's route,
// and trashing it would destroy work this run did not do.
func TestTrashRedundantAttachmentRefusesAForeignParent(t *testing.T) {
	f := &cleanupFake{
		items: map[string]map[string]any{"ATTACH01": {"itemType": "attachment", "parentItem": "SOMEBODY", "md5": "deadbeef"}},
	}
	srv := f.start(t)
	c := f.client(t, srv)

	err := trashRedundantAttachment(context.Background(), c, "ATTACH01", "TEMP0001", "deadbeef")
	if err == nil {
		t.Fatal("trashRedundantAttachment trashed an attachment under a foreign parent")
	}
	if len(f.patched) != 0 {
		t.Errorf("patched = %v, want none", f.patched)
	}
}

// TestTrashRedundantAttachmentAcceptsAnUnregisteredHash pins a behavior driven
// by live measurement: the desktop writes md5 and mtime a moment AFTER the
// connector creates the attachment, so at cleanup time the field is often unset.
// Refusing on an empty hash would leave litter in the common case; only a hash
// that is present and DIFFERENT proves the file belongs to someone else.
func TestTrashRedundantAttachmentAcceptsAnUnregisteredHash(t *testing.T) {
	f := &cleanupFake{
		items: map[string]map[string]any{"ATTACH01": {"itemType": "attachment", "parentItem": "TEMP0001"}},
	}
	srv := f.start(t)
	c := f.client(t, srv)

	if err := trashRedundantAttachment(context.Background(), c, "ATTACH01", "TEMP0001", "deadbeef"); err != nil {
		t.Fatalf("trashRedundantAttachment refused an attachment whose hash is not registered yet: %v", err)
	}
	if len(f.patched) != 1 {
		t.Errorf("patched = %v, want the attachment trashed", f.patched)
	}
}

// TestTrashRedundantAttachmentRefusesAMismatchedHash is the negative control for
// the case above: a hash that is present and different must still refuse.
func TestTrashRedundantAttachmentRefusesAMismatchedHash(t *testing.T) {
	f := &cleanupFake{
		items: map[string]map[string]any{"ATTACH01": {"itemType": "attachment", "parentItem": "TEMP0001", "md5": "0ther0ther"}},
	}
	srv := f.start(t)
	c := f.client(t, srv)

	err := trashRedundantAttachment(context.Background(), c, "ATTACH01", "TEMP0001", "deadbeef")
	if err == nil {
		t.Fatal("trashRedundantAttachment trashed an attachment whose registered hash differs")
	}
	if len(f.patched) != 0 {
		t.Errorf("patched = %v, want none", f.patched)
	}
}
