// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// sync must not roll back a write the read plane has not caught up with yet.

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

	"zotio/internal/store"
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

// syncPendingWriteAgeMarker pushes a marker's clock past the TTL so a sync has
// to treat it as expired.
func syncPendingWriteAgeMarker(t *testing.T, db *store.Store, id string) {
	t.Helper()
	if _, err := db.DB().Exec(`UPDATE pending_writes SET written_at = ? WHERE id = ?`,
		time.Now().Add(-2*pendingWriteTTL), id); err != nil {
		t.Fatalf("age marker %s: %v", id, err)
	}
}

// A marker's lifetime must not depend on the read plane happening to report its
// key. The row a marker guards is exactly the row the plane is not reporting
// yet, so a marker aged only against page members never ages at all and an
// abandoned FIELD write pinned its row forever. The sync must retire it while
// fetching a page that names only other keys.
func TestSyncRetiresAnExpiredFieldMarkerItNeverSeesInAPage(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	if err := db.RecordPendingWrite("items", "DRIFTED", []byte(`[{"field":"collections","add":"TARGET"}]`)); err != nil {
		t.Fatalf("record pending write: %v", err)
	}
	syncPendingWriteAgeMarker(t, db, "DRIFTED")

	// An ordinary incremental page: it reports an unrelated edit and never
	// mentions the marked key.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"OTHER","version":9,"data":{"key":"OTHER","itemType":"book","title":"Edited elsewhere"}}]`))
	}))
	defer server.Close()

	stderr := captureStderr(t, func() {
		if res := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 1, false, 1, false); res.Err != nil {
			t.Fatalf("sync: %v", res.Err)
		}
	})

	pending, err := db.PendingWriteCount()
	if err != nil {
		t.Fatalf("PendingWriteCount: %v", err)
	}
	if pending != 0 {
		t.Fatalf("markers outstanding after expiry = %d, want 0: a marker whose key is never reported would leak for good", pending)
	}
	if !strings.Contains(stderr, "DRIFTED") {
		t.Errorf("stderr = %q, want a warning naming the expired key: the suppression ended, so the row can come back", stderr)
	}
}

// The same pass must NOT retire a deletion marker on age, however old. The
// marker records a delete the write plane already confirmed, so a clock cannot
// be evidence against it — and the scenario where the TTL elapses is exactly
// the scenario the marker is for: a desktop that is not syncing, whose read
// plane keeps listing an item the cloud says is gone. Retiring here would put
// that item back into offline reads and search. The user is told instead.
func TestSyncKeepsAConfirmedDeletionMarkerPastItsTTLAndWarns(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	if _, err := db.UpsertKeyed("items", []string{"PURGED"},
		[]json.RawMessage{json.RawMessage(`{"key":"PURGED","version":1,"data":{"key":"PURGED","itemType":"book","title":"Purged"}}`)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.ReapMirroredItem("PURGED"); err != nil {
		t.Fatalf("ReapMirroredItem: %v", err)
	}
	syncPendingWriteAgeMarker(t, db, "PURGED")

	// The stale desktop still lists the purged item.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"PURGED","version":1,"data":{"key":"PURGED","itemType":"book","title":"Purged"}}]`))
	}))
	defer server.Close()

	stderr := captureStderr(t, func() {
		if res := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 1, false, 1, false); res.Err != nil {
			t.Fatalf("sync: %v", res.Err)
		}
	})

	pending, err := db.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if !pending["PURGED"].Deleted {
		t.Fatal("a sync retired a confirmed deletion marker on age; a clock must not overrule the write plane")
	}
	if mirroredItemKeys(t, db, "items")["PURGED"] {
		t.Error("the purged item was stored again because the TTL elapsed; offline reads and search would serve an item the cloud says is gone")
	}
	if !strings.Contains(stderr, "PURGED") {
		t.Errorf("stderr = %q, want a warning that the confirmed delete has not propagated: an indefinite suppression nobody is told about is its own bug", stderr)
	}
}

// A deletion marker inside its TTL must also survive a sync that never sees the
// key. Absence from an incremental page means unchanged, not that the delete
// landed, so retiring on absence would clear the marker on the first sync after
// a delete — exactly when the read plane is still lagging.
func TestSyncKeepsAnUnconfirmedDeletionMarkerItNeverSeesInAPage(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	if _, err := db.UpsertKeyed("items", []string{"PURGED"},
		[]json.RawMessage{json.RawMessage(`{"key":"PURGED","version":1,"data":{"key":"PURGED","itemType":"book","title":"Purged"}}`)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.ReapMirroredItem("PURGED"); err != nil {
		t.Fatalf("ReapMirroredItem: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"OTHER","version":9,"data":{"key":"OTHER","itemType":"book","title":"Edited elsewhere"}}]`))
	}))
	defer server.Close()

	if res := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 1, false, 1, false); res.Err != nil {
		t.Fatalf("sync: %v", res.Err)
	}

	pending, err := db.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if !pending["PURGED"].Deleted {
		t.Fatal("a sync retired a deletion marker on the strength of the key's absence; only a total-observation pass confirms")
	}
}

// The case that would have caught the leak. A sync with nothing to report
// returns an empty page and breaks out of the paging loop before
// upsertResourceBatch, so reconcilePendingWrites never runs. The age pass must
// therefore not be reached through the reconciliation.
func TestSyncRetiresAnExpiredFieldMarkerOnAnEmptyPage(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()

	if err := db.RecordPendingWrite("items", "DRIFTED", []byte(`[{"field":"collections","add":"TARGET"}]`)); err != nil {
		t.Fatalf("record pending write: %v", err)
	}
	syncPendingWriteAgeMarker(t, db, "DRIFTED")

	// Nothing changed since the cursor: the plane reports an empty page.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	if res := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 1, false, 1, false); res.Err != nil {
		t.Fatalf("sync: %v", res.Err)
	}

	pending, err := db.PendingWriteCount()
	if err != nil {
		t.Fatalf("PendingWriteCount: %v", err)
	}
	if pending != 0 {
		t.Fatalf("markers outstanding after an empty-page sync = %d, want 0: the age pass must not depend on a page being fetched", pending)
	}
}

// syncSweepGateSeed builds the mirror state both sweep-gate tests need: one
// row the read plane will NOT report on this pass, and one purged key holding
// an unconfirmed deletion marker.
func syncSweepGateSeed(t *testing.T, db *store.Store) {
	t.Helper()
	if _, err := db.UpsertKeyed("items", []string{"UNCHANGED"},
		[]json.RawMessage{json.RawMessage(`{"key":"UNCHANGED","version":1,"data":{"key":"UNCHANGED","itemType":"book","title":"Untouched for years"}}`)}); err != nil {
		t.Fatalf("seed unchanged row: %v", err)
	}
	if _, err := db.UpsertKeyed("items", []string{"PURGED"},
		[]json.RawMessage{json.RawMessage(`{"key":"PURGED","version":1,"data":{"key":"PURGED","itemType":"book","title":"Purged"}}`)}); err != nil {
		t.Fatalf("seed purged row: %v", err)
	}
	if err := db.ReapMirroredItem("PURGED"); err != nil {
		t.Fatalf("ReapMirroredItem: %v", err)
	}
}

// syncSweepGateServer reports one recently-changed item and nothing else,
// which is what a since-filtered plane returns.
func syncSweepGateServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"CHANGED","version":9,"data":{"key":"CHANGED","itemType":"book","title":"Edited"}}]`))
	}))
	t.Cleanup(server.Close)
	return server
}

// `--full --since N` keeps `full` true while every request carries a since
// filter, so the plane omits every unchanged object. Gating the sweep on the
// flag therefore read those absences as deletions: it reaped almost the whole
// mirror (a pre-existing bug at HEAD, independent of markers) and retired every
// deletion marker as confirmed, reopening the resurrection window through a
// documented flag combination. The pass must be judged by what was requested.
func TestFullSyncWithASinceFilterDoesNotSweep(t *testing.T) {
	// Human mode: the skipped-sweep notice takes the same two channels as every
	// other sync warning, stderr prose here and a sync_anomaly event under
	// --agent, so the prose assertion needs the human path.
	syncTestWithHumanFriendly(t, true)
	db := syncTestOpenStore(t)
	defer db.Close()
	syncSweepGateSeed(t, db)
	server := syncSweepGateServer(t)

	stderr := captureStderr(t, func() {
		if res := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 5, true, 1, false); res.Err != nil {
			t.Fatalf("sync: %v", res.Err)
		}
	})

	ids, err := db.ResourceIDs("items")
	if err != nil {
		t.Fatalf("ResourceIDs: %v", err)
	}
	if !ids["UNCHANGED"] {
		t.Error("a filtered full pass reaped a row the plane never reported; --since omits unchanged objects, so absence is not deletion")
	}
	pending, err := db.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if !pending["PURGED"].Deleted {
		t.Error("a filtered full pass retired the deletion marker as confirmed; the next stale page would resurrect the purged item")
	}
	if !strings.Contains(stderr, "skipped reaping") {
		t.Errorf("stderr = %q, want a warning that --full skipped the reap: a user who asked for --full and silently got no sweep is owed the reason", stderr)
	}
}

// The control. The gate must still sweep an unfiltered complete pass, or
// "reject filtered passes" degenerates into "never reap anything" and the
// mirror outlives its objects again.
func TestFullSyncWithoutASinceFilterStillSweeps(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()
	syncSweepGateSeed(t, db)
	server := syncSweepGateServer(t)

	if res := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 0, true, 1, false); res.Err != nil {
		t.Fatalf("sync: %v", res.Err)
	}

	ids, err := db.ResourceIDs("items")
	if err != nil {
		t.Fatalf("ResourceIDs: %v", err)
	}
	if ids["UNCHANGED"] {
		t.Error("an unfiltered complete pass did not reap a row the plane no longer reports")
	}
	pending, err := db.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if _, ok := pending["PURGED"]; ok {
		t.Error("an unfiltered complete pass did not retire the confirmed deletion marker; it would leak")
	}
}

// The other half of the sweep gate. Page truncation is not a request param, so
// the param check cannot see it: `--full --max-pages 1` over a multi-page
// resource issues perfectly unfiltered requests and still observes only a
// prefix. Treating that prefix as authoritative would reap almost the whole
// mirror and confirm every deletion marker, exactly like a filtered pass.
func TestFullSyncTruncatedByMaxPagesDoesNotSweep(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()
	syncSweepGateSeed(t, db)

	// A resource with more pages to come. Zotero array endpoints paginate via
	// start/limit with no Link body, so syncResource infers "there is more"
	// from a page that came back exactly full — which is why the response has
	// to be a whole page, not one item.
	page := determinePaginationDefaults()
	entries := make([]string, page.limit)
	for i := range entries {
		entries[i] = fmt.Sprintf(`{"key":"P%04d","version":9,"data":{"key":"P%04d","itemType":"book","title":"Page one"}}`, i, i)
	}
	body := "[" + strings.Join(entries, ",") + "]"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	// --full with a one-page cap: the pass is unfiltered but incomplete, so it
	// reports itself incomplete and must not have swept.
	res := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 0, true, 1, false)
	if res.Err == nil {
		t.Fatal("a truncated full pass reported success; --full must fail when it did not reach the end")
	}

	ids, err := db.ResourceIDs("items")
	if err != nil {
		t.Fatalf("ResourceIDs: %v", err)
	}
	if !ids["UNCHANGED"] {
		t.Error("a truncated full pass reaped a row that was simply on a page it never fetched")
	}
	pending, err := db.PendingWrites("items")
	if err != nil {
		t.Fatalf("PendingWrites: %v", err)
	}
	if !pending["PURGED"].Deleted {
		t.Error("a truncated full pass retired the deletion marker as confirmed; a prefix cannot confirm an absence")
	}
}
