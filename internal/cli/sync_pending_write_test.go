// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// sync must not roll back a write the read plane has not caught up with yet.

package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Field report #1 finding 2 / report #2 finding 6b. Write-through already
// replays an applied Web API write onto the mirror, so a local read is correct
// immediately. But sync pulls from the *local desktop API*, which has not yet
// received that write from zotero.org, so it overwrote the mirror row with the
// pre-write copy — silently rolling the write back in the store the reads use.
//
// The mirror must keep the local write until the read plane demonstrably carries
// it, while still accepting every other change the read plane reports.
func TestSyncDoesNotRollBackAnUnconfirmedLocalWrite(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	// The mirror as write-through leaves it: the item is in TARGET, applied
	// against the Web API moments ago.
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"K1","data":{"key":"K1","itemType":"journalArticle","title":"Paper","collections":["TARGET"]}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if err := db.RecordPendingWrite("items", "K1", []byte(`[{"field":"collections","add":"TARGET"}]`)); err != nil {
		t.Fatalf("record pending write: %v", err)
	}

	// The local desktop API still reports the pre-write state, and separately
	// carries a title edit made in the Zotero UI.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"K1","data":{"key":"K1","itemType":"journalArticle","title":"Paper (edited in Zotero)","collections":[]}}]`))
	}))
	defer server.Close()

	if res := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 0, false, 1, false); res.Err != nil {
		t.Fatalf("sync error: %v", res.Err)
	}

	rows, err := (localQueryStore{db}).QueryRaw(
		`SELECT json_extract(data,'$.data.collections') AS cols, json_extract(data,'$.data.title') AS title
		 FROM resources WHERE resource_type='items' AND id='K1'`)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read back: rows=%v err=%v", rows, err)
	}
	var cols []string
	if raw := sqlStringValue(rows[0]["cols"]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cols); err != nil {
			t.Fatalf("decode collections %q: %v", raw, err)
		}
	}
	// The write survives...
	if len(cols) != 1 || cols[0] != "TARGET" {
		t.Errorf("collections after sync = %v, want [TARGET]: sync rolled back an unconfirmed local write", cols)
	}
	// ...and the read plane's unrelated edit still lands.
	if got := sqlStringValue(rows[0]["title"]); got != "Paper (edited in Zotero)" {
		t.Errorf("title after sync = %q, want the read plane's edit: pending writes must not freeze the whole row", got)
	}
}

// Once the read plane reports the written state, the marker must clear so the
// row is never pinned forever.
func TestSyncClearsPendingWriteOnceConfirmed(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"K1","data":{"key":"K1","itemType":"journalArticle","collections":["TARGET"]}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if err := db.RecordPendingWrite("items", "K1", []byte(`[{"field":"collections","add":"TARGET"}]`)); err != nil {
		t.Fatalf("record pending write: %v", err)
	}

	// The desktop has now pulled the write from zotero.org.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"K1","data":{"key":"K1","itemType":"journalArticle","collections":["TARGET"]}}]`))
	}))
	defer server.Close()

	if res := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 0, false, 1, false); res.Err != nil {
		t.Fatalf("sync error: %v", res.Err)
	}

	pending, err := db.PendingWriteCount()
	if err != nil {
		t.Fatalf("PendingWriteCount: %v", err)
	}
	if pending != 0 {
		t.Fatalf("pending writes after confirmation = %d, want 0: the marker would pin this row forever", pending)
	}
}

// A marker must never pin a row to a local copy indefinitely. Replaying a change
// is not guaranteed to converge (a tag whose manual/automatic type disagrees
// between the planes never matches the recorded Remove), so an expired marker
// yields to the read plane rather than freezing the row forever.
func TestSyncDropsAnExpiredPendingWrite(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		json.RawMessage(`{"key":"K1","data":{"key":"K1","itemType":"journalArticle","collections":["TARGET"]}}`),
	}); err != nil {
		t.Fatalf("seed mirror: %v", err)
	}
	if err := db.RecordPendingWrite("items", "K1", []byte(`[{"field":"collections","add":"TARGET"}]`)); err != nil {
		t.Fatalf("record pending write: %v", err)
	}
	// Age the marker past its TTL.
	if _, err := db.DB().Exec(`UPDATE pending_writes SET written_at = ? WHERE id = 'K1'`,
		time.Now().Add(-2*pendingWriteTTL)); err != nil {
		t.Fatalf("age pending write: %v", err)
	}

	// The read plane still disagrees, and now it wins.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"K1","data":{"key":"K1","itemType":"journalArticle","collections":[]}}]`))
	}))
	defer server.Close()

	if res := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 0, false, 1, false); res.Err != nil {
		t.Fatalf("sync error: %v", res.Err)
	}

	pending, err := db.PendingWriteCount()
	if err != nil {
		t.Fatalf("PendingWriteCount: %v", err)
	}
	if pending != 0 {
		t.Fatalf("expired marker survived: %d pending", pending)
	}
	rows, err := (localQueryStore{db}).QueryRaw(
		`SELECT json_extract(data,'$.data.collections') AS cols FROM resources WHERE resource_type='items' AND id='K1'`)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read back: rows=%v err=%v", rows, err)
	}
	if got := sqlStringValue(rows[0]["cols"]); got != "[]" {
		t.Fatalf("collections after expiry = %s, want the read plane's []", got)
	}
}
