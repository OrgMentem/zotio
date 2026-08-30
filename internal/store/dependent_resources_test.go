// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Exercises dependent-resource columns and the annotation/fulltext
// query helpers used by dependent-resource sync.

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	_ "modernc.org/sqlite"
)

func TestDependentResourceColumnsAndQueries(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// A Zotero hierarchy: top item -> PDF attachment -> annotation.
	items := []json.RawMessage{
		json.RawMessage(`{"key":"TOP1","version":1,"data":{"key":"TOP1","itemType":"journalArticle","title":"Paper"}}`),
		json.RawMessage(`{"key":"ATT1","version":1,"data":{"key":"ATT1","itemType":"attachment","parentItem":"TOP1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"AN1","version":1,"data":{"key":"AN1","itemType":"annotation","parentItem":"ATT1","annotationColor":"#ff0","annotationText":"highlight"}}`),
	}
	stored, _, err := s.UpsertBatch("items", items)
	if err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}
	if stored != 3 {
		t.Fatalf("stored = %d, want 3", stored)
	}

	// Indexed columns are populated from the nested data sub-object.
	var parentKey, itemType string
	if err := s.DB().QueryRow(`SELECT parent_key, item_type FROM resources WHERE id = ?`, "AN1").Scan(&parentKey, &itemType); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if parentKey != "ATT1" {
		t.Errorf("AN1 parent_key = %q, want ATT1", parentKey)
	}
	if itemType != "annotation" {
		t.Errorf("AN1 item_type = %q, want annotation", itemType)
	}

	// ItemsByType filters on the indexed column.
	annotations, err := s.ItemsByType("annotation", 0)
	if err != nil {
		t.Fatalf("ItemsByType: %v", err)
	}
	if len(annotations) != 1 {
		t.Fatalf("ItemsByType(annotation) = %d rows, want 1", len(annotations))
	}

	// AnnotationsForItem joins annotation -> attachment -> top item.
	forTop, err := s.AnnotationsForItem("TOP1")
	if err != nil {
		t.Fatalf("AnnotationsForItem: %v", err)
	}
	if len(forTop) != 1 {
		t.Fatalf("AnnotationsForItem(TOP1) = %d rows, want 1", len(forTop))
	}
	var got map[string]any
	if err := json.Unmarshal(forTop[0], &got); err != nil {
		t.Fatalf("decode annotation: %v", err)
	}
	if got["key"] != "AN1" {
		t.Errorf("AnnotationsForItem returned key %v, want AN1", got["key"])
	}

	// An unrelated top item resolves no annotations.
	none, err := s.AnnotationsForItem("OTHER")
	if err != nil {
		t.Fatalf("AnnotationsForItem(OTHER): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("AnnotationsForItem(OTHER) = %d rows, want 0", len(none))
	}
}

func TestFulltextRoundTrip(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if err := s.Upsert("fulltext", "ATT1", json.RawMessage(`{"content":"hello world","indexedChars":11}`)); err != nil {
		t.Fatalf("Upsert fulltext: %v", err)
	}
	data, ok, err := s.Fulltext("ATT1")
	if err != nil {
		t.Fatalf("Fulltext: %v", err)
	}
	if !ok {
		t.Fatal("Fulltext(ATT1) not found")
	}
	if len(data) == 0 {
		t.Fatal("Fulltext(ATT1) returned empty data")
	}
	if _, ok, _ := s.Fulltext("MISSING"); ok {
		t.Error("Fulltext(MISSING) reported found")
	}
}

// UpsertKeyed persists caller-keyed payloads (no
// id in the body) in one transaction and round-trips through Fulltext.
func TestUpsertKeyed(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ids := []string{"ATT1", "ATT2"}
	data := []json.RawMessage{
		json.RawMessage(`{"content":"alpha","indexedChars":5}`),
		json.RawMessage(`{"content":"beta","indexedChars":4}`),
	}
	if _, err := s.UpsertKeyed("fulltext", ids, data); err != nil {
		t.Fatalf("UpsertKeyed: %v", err)
	}
	for _, id := range ids {
		got, ok, err := s.Fulltext(id)
		if err != nil {
			t.Fatalf("Fulltext(%s): %v", id, err)
		}
		if !ok || len(got) == 0 {
			t.Fatalf("Fulltext(%s) not stored", id)
		}
	}

	// Re-keying the same id replaces rather than duplicates.
	if _, err := s.UpsertKeyed("fulltext", []string{"ATT1"}, []json.RawMessage{json.RawMessage(`{"content":"alpha2"}`)}); err != nil {
		t.Fatalf("UpsertKeyed replace: %v", err)
	}
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM resources WHERE resource_type='fulltext'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("fulltext rows = %d, want 2 (replace, not insert)", count)
	}

	// Length mismatch is rejected; empty input is a no-op.
	if _, err := s.UpsertKeyed("fulltext", []string{"X"}, nil); err == nil {
		t.Error("UpsertKeyed mismatch: want error, got nil")
	}
	if _, err := s.UpsertKeyed("fulltext", nil, nil); err != nil {
		t.Errorf("UpsertKeyed empty: %v", err)
	}
}

// AnnotationsForItems returns the same rows as
// per-item AnnotationsForItem but grouped, in a single query.
func TestAnnotationsForItems(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	items := []json.RawMessage{
		json.RawMessage(`{"key":"TOP1","version":1,"data":{"key":"TOP1","itemType":"journalArticle","title":"A"}}`),
		json.RawMessage(`{"key":"ATT1","version":1,"data":{"key":"ATT1","itemType":"attachment","parentItem":"TOP1","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"AN1","version":1,"data":{"key":"AN1","itemType":"annotation","parentItem":"ATT1"}}`),
		json.RawMessage(`{"key":"TOP2","version":1,"data":{"key":"TOP2","itemType":"journalArticle","title":"B"}}`),
		json.RawMessage(`{"key":"ATT2","version":1,"data":{"key":"ATT2","itemType":"attachment","parentItem":"TOP2","contentType":"application/pdf"}}`),
		json.RawMessage(`{"key":"AN2","version":1,"data":{"key":"AN2","itemType":"annotation","parentItem":"ATT2"}}`),
		json.RawMessage(`{"key":"AN3","version":1,"data":{"key":"AN3","itemType":"annotation","parentItem":"ATT2"}}`),
	}
	if _, _, err := s.UpsertBatch("items", items); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}

	grouped, err := s.AnnotationsForItems([]string{"TOP1", "TOP2", "OTHER"})
	if err != nil {
		t.Fatalf("AnnotationsForItems: %v", err)
	}
	if len(grouped["TOP1"]) != 1 {
		t.Errorf("TOP1 annotations = %d, want 1", len(grouped["TOP1"]))
	}
	if len(grouped["TOP2"]) != 2 {
		t.Errorf("TOP2 annotations = %d, want 2", len(grouped["TOP2"]))
	}
	if _, ok := grouped["OTHER"]; ok {
		t.Errorf("OTHER should be absent, got %d", len(grouped["OTHER"]))
	}

	// Empty input returns an empty (non-nil) map.
	empty, err := s.AnnotationsForItems(nil)
	if err != nil {
		t.Fatalf("AnnotationsForItems(nil): %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("AnnotationsForItems(nil) = %v, want empty map", empty)
	}
}

// TestAnnotationsIgnoreTrashMirrorDuplicates pins the resource_type predicate on
// the annotation joins. Ids are unique only WITHIN a resource_type, and
// mirrorTrashedItem (internal/cli/write_through.go) deliberately leaves a
// trashed attachment in BOTH 'items' and 'items-trash' until the read plane
// catches up. Without the predicate the attachment alias matched twice and every
// annotation under it was emitted twice, into `items summarize`, vault sync,
// `collections bundle`, and the MCP annotation resource.
func TestAnnotationsIgnoreTrashMirrorDuplicates(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	attachment := json.RawMessage(`{"key":"ATT1","version":1,"data":{"key":"ATT1","itemType":"attachment","parentItem":"TOP1","contentType":"application/pdf"}}`)
	items := []json.RawMessage{
		json.RawMessage(`{"key":"TOP1","version":1,"data":{"key":"TOP1","itemType":"journalArticle","title":"A"}}`),
		attachment,
		json.RawMessage(`{"key":"AN1","version":1,"data":{"key":"AN1","itemType":"annotation","parentItem":"ATT1"}}`),
	}
	if _, _, err := s.UpsertBatch("items", items); err != nil {
		t.Fatalf("UpsertBatch items: %v", err)
	}
	// Exactly what `items delete ATT1` leaves behind: the same id in both
	// partitions for the whole write-through window. This duplicate exercises
	// the `att.resource_type` predicate.
	if _, err := s.UpsertKeyed("items-trash", []string{"ATT1"}, []json.RawMessage{attachment}); err != nil {
		t.Fatalf("UpsertKeyed items-trash: %v", err)
	}
	// A trashed annotation under the SAME live attachment exercises the OTHER
	// predicate, `a.resource_type`. Without it this row joins the live
	// attachment and is emitted as if it were current.
	if _, err := s.UpsertKeyed("items-trash", []string{"AN2"}, []json.RawMessage{
		json.RawMessage(`{"key":"AN2","version":1,"data":{"key":"AN2","itemType":"annotation","parentItem":"ATT1"}}`),
	}); err != nil {
		t.Fatalf("UpsertKeyed trashed annotation: %v", err)
	}

	single, err := s.AnnotationsForItem("TOP1")
	if err != nil {
		t.Fatalf("AnnotationsForItem: %v", err)
	}
	if len(single) != 1 {
		t.Errorf("AnnotationsForItem returned %d rows, want 1 (only the live AN1)", len(single))
	}
	if len(single) > 0 && !bytes.Contains(single[0], []byte(`"AN1"`)) {
		t.Errorf("AnnotationsForItem returned %s, want the live annotation AN1", single[0])
	}

	// Ask across two batches as well, so the chunked query path is covered with
	// the duplicate present.
	grouped, err := s.AnnotationsForItems([]string{"TOP1", "TOP_ABSENT"})
	if err != nil {
		t.Fatalf("AnnotationsForItems: %v", err)
	}
	if len(grouped["TOP1"]) != 1 {
		t.Errorf("AnnotationsForItems returned %d rows for TOP1, want 1", len(grouped["TOP1"]))
	}
	if len(grouped["TOP1"]) > 0 && !bytes.Contains(grouped["TOP1"][0], []byte(`"AN1"`)) {
		t.Errorf("AnnotationsForItems returned %s for TOP1, want the live annotation AN1", grouped["TOP1"][0])
	}
}

// UpsertKeyed reports rows that landed, like UpsertBatch. sync's
// upsertResourceBatchWithExtractedIDs feeds that number straight into the sync
// total, so counting the ids it offered would overstate the pass whenever the
// version-monotonic guard retains a newer stored row.
func TestUpsertKeyedCountsOnlyRowsThatLanded(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	newer := json.RawMessage(`{"key":"K1","version":10,"data":{"key":"K1","itemType":"book","title":"newer"}}`)
	if stored, err := s.UpsertKeyed("items", []string{"K1"}, []json.RawMessage{newer}); err != nil || stored != 1 {
		t.Fatalf("seed UpsertKeyed = (%d, %v), want (1, nil)", stored, err)
	}

	older := json.RawMessage(`{"key":"K1","version":5,"data":{"key":"K1","itemType":"book","title":"older"}}`)
	fresh := json.RawMessage(`{"key":"K2","version":1,"data":{"key":"K2","itemType":"book","title":"fresh"}}`)
	stored, err := s.UpsertKeyed("items", []string{"K1", "K2"}, []json.RawMessage{older, fresh})
	if err != nil {
		t.Fatalf("UpsertKeyed: %v", err)
	}
	if stored != 1 {
		t.Errorf("stored = %d, want 1: K1 was retained at the newer version, only K2 landed", stored)
	}

	kept, err := s.Get("items", "K1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Contains(kept, []byte(`"newer"`)) {
		t.Errorf("stored row = %s, want the version-10 payload retained", kept)
	}
}

// TestUpsertBatchCountsOnlyRowsThatLanded pins `stored` to writes, not offers.
// The version-monotonic guard retains a newer stored row and writes nothing, so
// counting prepared items instead overstated sync totals and softened the
// stored-vs-consumed diagnostic that F4b relies on.
func TestUpsertBatchCountsOnlyRowsThatLanded(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	newer := json.RawMessage(`{"key":"K1","version":10,"data":{"key":"K1","itemType":"book","title":"newer"}}`)
	if stored, _, err := s.UpsertBatch("items", []json.RawMessage{newer}); err != nil || stored != 1 {
		t.Fatalf("seed UpsertBatch = (%d, %v), want (1, nil)", stored, err)
	}

	older := json.RawMessage(`{"key":"K1","version":5,"data":{"key":"K1","itemType":"book","title":"older"}}`)
	stored, extractFailures, err := s.UpsertBatch("items", []json.RawMessage{older})
	if err != nil {
		t.Fatalf("UpsertBatch older: %v", err)
	}
	if stored != 0 {
		t.Errorf("stored = %d, want 0: the older version was retained, so nothing was written", stored)
	}
	if extractFailures != 0 {
		t.Errorf("extractFailures = %d, want 0: a retained row is not an extraction failure", extractFailures)
	}

	kept, err := s.Get("items", "K1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Contains(kept, []byte(`"newer"`)) {
		t.Errorf("stored row = %s, want the version-10 payload retained", kept)
	}
}

func TestOpenWithContextPrivateFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not portable on windows")
	}
	dir := filepath.Join(t.TempDir(), "store")
	dbPath := filepath.Join(dir, "data.db")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod dir setup: %v", err)
	}
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil { // #nosec G306 -- deliberately lax pre-existing file; the test asserts OpenWithContext re-chmods it to 0600
		t.Fatalf("write db setup: %v", err)
	}
	if err := os.Chmod(dbPath, 0o644); err != nil {
		t.Fatalf("chmod db setup: %v", err)
	}
	s, err := OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	assertMode(t, dir, 0o700)
	assertMode(t, dbPath, 0o600)
}

func TestAnnotationsForItemsChunksSQLiteVariables(t *testing.T) {
	s, err := OpenWithContext(context.Background(), filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	const itemCount = 1500
	items := make([]json.RawMessage, 0, itemCount*3)
	keys := make([]string, 0, itemCount)
	for i := range itemCount {
		suffix := strconv.Itoa(i)
		top := "TOP" + suffix
		att := "ATT" + suffix
		ann := "ANN" + suffix
		keys = append(keys, top)
		items = append(items,
			json.RawMessage(`{"key":"`+top+`","version":1,"data":{"key":"`+top+`","itemType":"journalArticle","title":"Paper `+suffix+`"}}`),
			json.RawMessage(`{"key":"`+att+`","version":1,"data":{"key":"`+att+`","itemType":"attachment","parentItem":"`+top+`","contentType":"application/pdf"}}`),
			json.RawMessage(`{"key":"`+ann+`","version":1,"data":{"key":"`+ann+`","itemType":"annotation","parentItem":"`+att+`"}}`),
		)
	}
	if stored, _, err := s.UpsertBatch("items", items); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	} else if stored != len(items) {
		t.Fatalf("stored = %d, want %d", stored, len(items))
	}

	grouped, err := s.AnnotationsForItems(keys)
	if err != nil {
		t.Fatalf("AnnotationsForItems(%d keys): %v", len(keys), err)
	}
	if len(grouped) != itemCount {
		t.Fatalf("grouped keys = %d, want %d", len(grouped), itemCount)
	}
	for i, key := range keys {
		rows := grouped[key]
		if len(rows) != 1 {
			t.Fatalf("%s annotations = %d, want 1", key, len(rows))
		}
		var got map[string]any
		if err := json.Unmarshal(rows[0], &got); err != nil {
			t.Fatalf("decode %s annotation: %v", key, err)
		}
		if want := "ANN" + strconv.Itoa(i); got["key"] != want {
			t.Fatalf("%s annotation key = %v, want %s", key, got["key"], want)
		}
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode() & os.ModePerm; got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}
