// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"zotio/internal/store"
)

func synccoreKey(index int) string {
	const alphabet = "23456789ABCDEFGHIJKLMNPQRSTUVWXYZ"
	return "SYNCA" + string([]byte{alphabet[(index/1024)%32], alphabet[(index/32)%32], alphabet[index%32]})
}

func synccoreItem(index, version int, title string) json.RawMessage {
	key := synccoreKey(index)
	added := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(index) * time.Minute).Format(time.RFC3339)
	return json.RawMessage(fmt.Sprintf(`{"key":%q,"version":%d,"data":{"key":%q,"version":%d,"itemType":"book","title":%q,"dateAdded":%q,"collections":[]}}`, key, version, key, version, title, added))
}

type synccoreResumeWorld struct {
	mu             sync.Mutex
	removed        bool
	replaced       bool
	missingVersion bool
	rejectRestart  bool
	missingTotal   bool
	malformedTotal bool
	shiftInPass    bool
	version        int
	starts         []int
}

func (world *synccoreResumeWorld) serve(w http.ResponseWriter, r *http.Request) {
	world.mu.Lock()
	defer world.mu.Unlock()
	start, _ := strconv.Atoi(r.URL.Query().Get("start"))
	if world.rejectRestart && start == 0 {
		http.Error(w, "restart temporarily unavailable", http.StatusBadRequest)
		return
	}
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	world.starts = append(world.starts, start)
	if world.shiftInPass && start > 0 {
		world.removed = true
	}
	rows := make([]json.RawMessage, 0, 101)
	for i := range 101 {
		if (world.removed && i == 3) || since >= 60 {
			continue
		}
		rows = append(rows, synccoreItem(i, 60, "original"))
	}
	if world.replaced && since < 101 {
		rows = append(rows, synccoreItem(101, 101, "replacement"))
	}
	w.Header().Set("Content-Type", "application/json")
	if !world.missingVersion {
		w.Header().Set("Last-Modified-Version", strconv.Itoa(world.version))
	}
	if world.malformedTotal && start > 0 {
		w.Header().Set("Total-Results", "unknown")
		world.malformedTotal = false
	} else if !world.missingTotal {
		w.Header().Set("Total-Results", strconv.Itoa(len(rows)))
	}
	if start > len(rows) {
		start = len(rows)
	}
	end := min(start+100, len(rows))
	_ = json.NewEncoder(w).Encode(rows[start:end])
}

func TestSyncResumeRestartsUnverifiedOffsets(t *testing.T) {
	for _, mode := range []string{"shrunk", "missing total", "malformed total"} {
		t.Run(mode, func(t *testing.T) {
			syncTestWithHumanFriendly(t, false)
			world := &synccoreResumeWorld{version: 100}
			server := httptest.NewServer(http.HandlerFunc(world.serve))
			defer server.Close()
			c := syncTestClient(server.URL)
			db := syncTestOpenStore(t)
			defer db.Close()
			if err := db.SaveLibraryVersion("items", c.Plane(), 50); err != nil {
				t.Fatal(err)
			}
			if result := syncResource(context.Background(), c, db, "items", 0, false, 1, false); result.Err != nil || result.Incomplete == "" {
				t.Fatalf("capped pass = %+v, want a resumable incomplete pass", result)
			}
			cursor, scope, _, _, err := db.GetSyncResumeState("items")
			var provenance struct {
				Total *int `json:"total_results"`
			}
			if err != nil || cursor != "100" || json.Unmarshal([]byte(scope), &provenance) != nil || provenance.Total == nil || *provenance.Total != 101 {
				t.Fatalf("saved cursor=%q scope=%s err=%v, want offset 100 and total 101", cursor, scope, err)
			}
			world.mu.Lock()
			world.removed = true
			world.missingTotal = mode == "missing total"
			world.malformedTotal = mode == "malformed total"
			world.starts = nil
			world.mu.Unlock()

			if result := syncResource(context.Background(), c, db, "items", 0, false, 0, false); result.Err != nil {
				t.Fatalf("resumed pass: %v", result.Err)
			}
			if raw, err := db.Get("items", synccoreKey(100)); err != nil || len(raw) == 0 {
				t.Fatalf("last item after resume = %s, %v; shrinking the prefix must not skip it", raw, err)
			}
			if version, err := db.GetLibraryVersion("items", c.Plane()); err != nil || version != 100 {
				t.Fatalf("checkpoint=%d err=%v, want the original watermark 100", version, err)
			}
			world.mu.Lock()
			starts := append([]int(nil), world.starts...)
			world.malformedTotal = false
			world.mu.Unlock()
			if len(starts) < 2 || starts[0] != 100 || starts[1] != 0 {
				t.Fatalf("resume offsets = %v, want the unverified offset followed by a restart at zero", starts)
			}
			if result := syncResource(context.Background(), c, db, "items", 0, false, 0, false); result.Err != nil {
				t.Fatalf("next incremental pass: %v", result.Err)
			}
			if _, err := db.Get("items", synccoreKey(100)); err != nil {
				t.Fatalf("last item after next incremental pass: %v", err)
			}
		})
	}
}

func TestSyncTotalShiftDiscardsSavedOffset(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	world := &synccoreResumeWorld{version: 100, shiftInPass: true}
	server := httptest.NewServer(http.HandlerFunc(world.serve))
	defer server.Close()
	c := syncTestClient(server.URL)
	db := syncTestOpenStore(t)
	defer db.Close()
	if err := db.SaveLibraryVersion("items", c.Plane(), 50); err != nil {
		t.Fatal(err)
	}
	result := syncResource(context.Background(), c, db, "items", 0, false, 0, false)
	if !errors.Is(result.Err, errTotalResultsChanged) {
		t.Fatalf("shifted listing error = %v, want errTotalResultsChanged", result.Err)
	}
	if cursor, _, _, _, err := db.GetSyncResumeState("items"); err != nil || cursor != "0" {
		t.Fatalf("cursor after listing shift = %q, %v; want an unfinished pass restarting at zero", cursor, err)
	}
	if version, err := db.GetLibraryVersion("items", c.Plane()); err != nil || version != 50 {
		t.Fatalf("checkpoint after listing shift = %d, %v; want 50", version, err)
	}
	world.mu.Lock()
	world.shiftInPass = false
	world.starts = nil
	world.mu.Unlock()
	if result := syncResource(context.Background(), c, db, "items", 0, false, 0, false); result.Err != nil {
		t.Fatalf("restarted pass: %v", result.Err)
	}
	world.mu.Lock()
	defer world.mu.Unlock()
	if len(world.starts) == 0 || world.starts[0] != 0 {
		t.Fatalf("restart offsets = %v, want start=0", world.starts)
	}
	if _, err := db.Get("items", synccoreKey(100)); err != nil {
		t.Fatalf("last item after restart: %v", err)
	}
}

func TestSyncResumeDiscardsCursorWithoutTotal(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	world := &synccoreResumeWorld{version: 100, removed: true}
	server := httptest.NewServer(http.HandlerFunc(world.serve))
	defer server.Close()
	c := syncTestClient(server.URL)
	db := syncTestOpenStore(t)
	defer db.Close()
	if err := db.SaveLibraryVersion("items", c.Plane(), 50); err != nil {
		t.Fatal(err)
	}
	scope := fmt.Sprintf(`{"plane":%q,"mode":"incremental","since":50,"watermark":100}`, c.Plane())
	if err := db.SaveSyncResumeStateContext(context.Background(), "items", "100", scope, 100); err != nil {
		t.Fatal(err)
	}
	if result := syncResource(context.Background(), c, db, "items", 0, false, 0, false); result.Err != nil {
		t.Fatal(result.Err)
	}
	if _, err := db.Get("items", synccoreKey(100)); err != nil {
		t.Fatalf("last item after discarding a legacy cursor: %v", err)
	}
	world.mu.Lock()
	defer world.mu.Unlock()
	if len(world.starts) == 0 || world.starts[0] != 0 {
		t.Fatalf("legacy cursor offsets = %v, want start=0", world.starts)
	}
}

func TestFullSyncKeepsItemEditedDuringWalk(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()
	const editedIndex = 220
	key := synccoreKey(editedIndex)
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{synccoreItem(editedIndex, 60, "original")}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordPendingWrite("items", key, []byte(`[{"field":"collections","add":"ABCD2345"}]`)); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		stable := r.URL.Query().Get("sort") == "dateAdded" && r.URL.Query().Get("direction") == "asc"
		edited := requests > 1
		indices := make([]int, 0, 250)
		if edited && !stable {
			indices = append(indices, editedIndex)
		}
		for i := range 250 {
			if !edited || stable || i != editedIndex {
				indices = append(indices, i)
			}
		}
		rows := make([]json.RawMessage, 0, 100)
		for _, index := range indices[min(start, len(indices)):min(start+100, len(indices))] {
			version, title := 60, "original"
			if edited && index == editedIndex {
				version, title = 61, "edited during walk"
			}
			rows = append(rows, synccoreItem(index, version, title))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Total-Results", "250")
		w.Header().Set("Last-Modified-Version", "60")
		_ = json.NewEncoder(w).Encode(rows)
	}))
	defer server.Close()
	if result := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 0, true, 0, false); result.Err != nil {
		t.Fatalf("full pass: %v", result.Err)
	}
	raw, err := db.Get("items", key)
	if err != nil || !bytes.Contains(raw, []byte("edited during walk")) || !bytes.Contains(raw, []byte("ABCD2345")) {
		t.Fatalf("edited item = %s, %v; want its edit and pending collection write intact", raw, err)
	}
	pending, err := db.PendingWrites("items")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := pending[key]; !present {
		t.Fatal("full pass dropped the live item's pending-write marker")
	}
}

func TestFullSyncSkipsSweepWhenStableOrderUnsupported(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()
	key := synccoreKey(0)
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{synccoreItem(0, 60, "keep")}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("sort") != "" {
			http.Error(w, "sort=dateAdded unsupported", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Total-Results", "0")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	var events bytes.Buffer
	ctx := context.WithValue(context.Background(), syncEventWriterContextKey{}, &events)
	if result := syncResource(ctx, syncTestClient(server.URL), db, "items", 0, true, 0, false); result.Err != nil {
		t.Fatal(result.Err)
	}
	if _, err := db.Get("items", key); err != nil {
		t.Fatalf("item after unsupported stable sort: %v", err)
	}
	if !strings.Contains(events.String(), `"reason":"sweep_skipped_unstable_order"`) {
		t.Fatalf("events = %s, want the unstable-order skip reason", events.String())
	}
}

func TestSyncVersionRegressionResetsCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		full   bool
		header bool
	}{
		{name: "full with header", full: true, header: true},
		{name: "full desktop body version", full: true},
		{name: "incremental detects lower header", header: true},
		{name: "incremental detects restored desktop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			syncTestWithHumanFriendly(t, false)
			db := syncTestOpenStore(t)
			defer db.Close()
			var mu sync.Mutex
			version := 60
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				since, _ := strconv.Atoi(r.URL.Query().Get("since"))
				w.Header().Set("Content-Type", "application/json")
				if tc.header {
					w.Header().Set("Last-Modified-Version", strconv.Itoa(version))
				}
				if r.URL.Query().Get("format") == "versions" {
					w.Header().Set("Total-Results", "1")
					_ = json.NewEncoder(w).Encode(map[string]int{synccoreKey(0): version})
					return
				}
				if since >= version {
					w.Header().Set("Total-Results", "0")
					_, _ = w.Write([]byte(`[]`))
					return
				}
				w.Header().Set("Total-Results", "1")
				_ = json.NewEncoder(w).Encode([]json.RawMessage{synccoreItem(0, version, fmt.Sprintf("version %d", version))})
			}))
			defer server.Close()
			c := syncTestClient(server.URL)
			if _, _, err := db.UpsertBatch("items", []json.RawMessage{synccoreItem(0, 500, "old library")}); err != nil {
				t.Fatal(err)
			}
			if err := db.SaveLibraryVersion("items", c.Plane(), 500); err != nil {
				t.Fatal(err)
			}
			if result := syncResource(context.Background(), c, db, "items", 0, tc.full, 0, false); result.Err != nil {
				t.Fatalf("reset pass: %v", result.Err)
			}
			if checkpoint, err := db.GetLibraryVersion("items", c.Plane()); err != nil || checkpoint != 60 {
				t.Fatalf("checkpoint after reset = %d, %v; want 60", checkpoint, err)
			}
			if raw, err := db.Get("items", synccoreKey(0)); err != nil || !bytes.Contains(raw, []byte("version 60")) {
				t.Fatalf("restored item = %s, %v; stale row versions must not block the full pass", raw, err)
			}
			mu.Lock()
			version = 61
			mu.Unlock()
			if result := syncResource(context.Background(), c, db, "items", 0, false, 0, false); result.Err != nil {
				t.Fatalf("incremental pass after reset: %v", result.Err)
			}
			if raw, err := db.Get("items", synccoreKey(0)); err != nil || !bytes.Contains(raw, []byte("version 61")) {
				t.Fatalf("edited item after reset = %s, %v; want the version 61 edit", raw, err)
			}
		})
	}
}

func TestFullSyncVersionRegressionKeepsCheckpointWhenIncomplete(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	world := &synccoreResumeWorld{version: 60}
	server := httptest.NewServer(http.HandlerFunc(world.serve))
	defer server.Close()
	c := syncTestClient(server.URL)
	db := syncTestOpenStore(t)
	defer db.Close()
	if err := db.SaveLibraryVersion("items", c.Plane(), 500); err != nil {
		t.Fatal(err)
	}
	if result := syncResource(context.Background(), c, db, "items", 0, true, 1, false); result.Err == nil {
		t.Fatal("capped full pass succeeded")
	}
	if checkpoint, err := db.GetLibraryVersion("items", c.Plane()); err != nil || checkpoint != 500 {
		t.Fatalf("checkpoint after incomplete full pass = %d, %v; want 500", checkpoint, err)
	}
	if _, err := db.Get("items", synccoreKey(100)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unfetched last item error = %v, want not found", err)
	}
}

func TestSyncQuietDesktopVersionSnapshotCannotAdvanceCheckpoint(t *testing.T) {
	for _, version := range []int{500, 501} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			syncTestWithHumanFriendly(t, false)
			db := syncTestOpenStore(t)
			defer db.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Query().Get("format") == "versions" {
					w.Header().Set("Total-Results", "1")
					_ = json.NewEncoder(w).Encode(map[string]int{synccoreKey(0): version})
					return
				}
				if r.URL.Query().Get("since") != "500" {
					t.Errorf("quiet library triggered a full pass: %s", r.URL.RequestURI())
				}
				w.Header().Set("Total-Results", "0")
				_, _ = w.Write([]byte(`[]`))
			}))
			defer server.Close()
			c := syncTestClient(server.URL)
			if err := db.SaveLibraryVersion("items", c.Plane(), 500); err != nil {
				t.Fatal(err)
			}
			if result := syncResource(context.Background(), c, db, "items", 0, false, 0, false); result.Err != nil {
				t.Fatal(result.Err)
			}
			if checkpoint, err := db.GetLibraryVersion("items", c.Plane()); err != nil || checkpoint != 500 {
				t.Fatalf("checkpoint after a version-only snapshot = %d, %v; want 500", checkpoint, err)
			}
		})
	}
}

func TestSyncRejectsUnverifiedDesktopVersionSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name, body, total string
	}{
		{name: "missing total", body: `{"ABCD2345":60}`},
		{name: "short map", body: `{"ABCD2345":60}`, total: "2"},
		{name: "null", body: `null`, total: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			syncTestWithHumanFriendly(t, false)
			db := syncTestOpenStore(t)
			defer db.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Query().Get("format") == "versions" {
					if tc.total != "" {
						w.Header().Set("Total-Results", tc.total)
					}
					_, _ = w.Write([]byte(tc.body))
					return
				}
				w.Header().Set("Total-Results", "0")
				_, _ = w.Write([]byte(`[]`))
			}))
			defer server.Close()
			c := syncTestClient(server.URL)
			if err := db.SaveLibraryVersion("items", c.Plane(), 500); err != nil {
				t.Fatal(err)
			}
			if result := syncResource(context.Background(), c, db, "items", 0, false, 0, false); result.Err == nil {
				t.Fatal("sync accepted an unverified version snapshot")
			}
			if checkpoint, err := db.GetLibraryVersion("items", c.Plane()); err != nil || checkpoint != 500 {
				t.Fatalf("checkpoint after an unverified snapshot = %d, %v; want 500", checkpoint, err)
			}
		})
	}
}

func TestFullSyncSkipsSweepForUnstableMultiPageResource(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	db := syncTestOpenStore(t)
	defer db.Close()
	key := synccoreKey(220)
	if _, err := db.UpsertKeyed("collections", []string{key}, []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"key":%q,"data":{"key":%q,"name":"Keep"}}`, key, key))}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		rows := make([]json.RawMessage, 0, 100)
		for index := start; index < min(start+100, 101); index++ {
			key := synccoreKey(index)
			rows = append(rows, json.RawMessage(fmt.Sprintf(`{"key":%q,"data":{"key":%q,"name":"Listed"}}`, key, key)))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Total-Results", "101")
		_ = json.NewEncoder(w).Encode(rows)
	}))
	defer server.Close()
	var events bytes.Buffer
	ctx := context.WithValue(context.Background(), syncEventWriterContextKey{}, &events)
	if result := syncResource(ctx, syncTestClient(server.URL), db, "collections", 0, true, 0, false); result.Err != nil {
		t.Fatal(result.Err)
	}
	if _, err := db.Get("collections", key); err != nil {
		t.Fatalf("collection after an unstable multi-page walk: %v", err)
	}
	if !strings.Contains(events.String(), `"reason":"sweep_skipped_unstable_order"`) {
		t.Fatalf("events = %s, want the unstable-order skip reason", events.String())
	}
}

func TestSyncResumeRestartsEqualTotalReplacement(t *testing.T) {
	for _, mode := range []string{"changed header", "missing first header", "missing resumed header", "missing both headers"} {
		t.Run(mode, func(t *testing.T) {
			syncTestWithHumanFriendly(t, false)
			world := &synccoreResumeWorld{
				version:        100,
				missingVersion: mode == "missing first header" || mode == "missing both headers",
			}
			watermark := 100
			if world.missingVersion {
				watermark = 60
			}
			server := httptest.NewServer(http.HandlerFunc(world.serve))
			defer server.Close()
			c := syncTestClient(server.URL)
			db := syncTestOpenStore(t)
			defer db.Close()
			if err := db.SaveLibraryVersion("items", c.Plane(), 50); err != nil {
				t.Fatal(err)
			}
			if result := syncResource(context.Background(), c, db, "items", 0, false, 1, false); result.Err != nil || result.Incomplete == "" {
				t.Fatalf("initial capped pass = %+v", result)
			}
			world.mu.Lock()
			world.removed, world.replaced = true, true
			world.version = 101
			world.missingVersion = mode == "missing resumed header" || mode == "missing both headers"
			world.starts = nil
			world.mu.Unlock()
			if result := syncResource(context.Background(), c, db, "items", 0, false, 0, false); result.Err != nil {
				t.Fatal(result.Err)
			}
			if _, err := db.Get("items", synccoreKey(100)); err != nil {
				t.Fatalf("equal-total replacement skipped the shifted item: %v", err)
			}
			if _, err := db.Get("items", synccoreKey(101)); err != nil {
				t.Fatalf("replacement item did not arrive: %v", err)
			}
			if version, err := db.GetLibraryVersion("items", c.Plane()); err != nil || version != watermark {
				t.Fatalf("checkpoint = %d, %v; want original watermark %d", version, err, watermark)
			}
			world.mu.Lock()
			defer world.mu.Unlock()
			if len(world.starts) < 2 || world.starts[0] != 100 || world.starts[1] != 0 {
				t.Fatalf("resume offsets = %v, want rejection of 100 followed by zero", world.starts)
			}
		})
	}
}

func TestSyncFailedRestartKeepsIncompleteStateAndWatermark(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	world := &synccoreResumeWorld{version: 100}
	server := httptest.NewServer(http.HandlerFunc(world.serve))
	defer server.Close()
	c := syncTestClient(server.URL)
	db := syncTestOpenStore(t)
	defer db.Close()
	if err := db.SaveLibraryVersion("items", c.Plane(), 50); err != nil {
		t.Fatal(err)
	}
	if result := syncResource(context.Background(), c, db, "items", 0, false, 1, false); result.Err != nil {
		t.Fatal(result.Err)
	}
	_, _, before, _, err := db.GetSyncResumeState("items")
	if err != nil {
		t.Fatal(err)
	}
	world.mu.Lock()
	world.removed, world.replaced, world.rejectRestart = true, true, true
	world.version = 101
	world.mu.Unlock()
	if result := syncResource(context.Background(), c, db, "items", 0, false, 0, false); result.Err == nil {
		t.Fatal("restart request unexpectedly succeeded")
	}
	cursor, _, after, _, err := db.GetSyncResumeState("items")
	if err != nil || cursor != "0" || !after.Equal(before) {
		t.Fatalf("failed restart state = cursor %q, timestamp %v, error %v; want zero and unchanged timestamp %v", cursor, after, err, before)
	}
	world.mu.Lock()
	world.rejectRestart = false
	world.starts = nil
	world.mu.Unlock()
	if result := syncResource(context.Background(), c, db, "items", 0, false, 0, false); result.Err != nil {
		t.Fatal(result.Err)
	}
	if version, err := db.GetLibraryVersion("items", c.Plane()); err != nil || version != 100 {
		t.Fatalf("checkpoint after restart recovery = %d, %v; want original watermark 100", version, err)
	}
	if _, err := db.Get("items", synccoreKey(100)); err != nil {
		t.Fatalf("shifted item after restart recovery: %v", err)
	}
}

func TestSyncRejectedInitialPassRefusesMirrorConsumers(t *testing.T) {
	for _, args := range [][]string{
		{"library", "prisma"},
		{"library", "health", "--for", "citation", "--fail-on", "high"},
		{"import", "scan"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root, flags, out, _ := newPreflightTestRoot(t)
			flags.maxChanges = -1
			syncTestWithHumanFriendly(t, false)
			dbPath := helpersTestDefaultDBPath(t, "zotio")
			if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
				t.Fatal(err)
			}
			db, err := store.OpenWithContext(context.Background(), dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			world := &synccoreResumeWorld{version: 100, shiftInPass: true}
			server := httptest.NewServer(http.HandlerFunc(world.serve))
			defer server.Close()
			result := syncResource(context.Background(), syncTestClient(server.URL), db, "items", 0, false, 0, false)
			if !errors.Is(result.Err, errTotalResultsChanged) {
				t.Fatalf("initial pass error = %v, want changed total", result.Err)
			}
			if ready, detail, err := importItemsMirrorReady(context.Background(), db); err != nil || ready {
				t.Fatalf("initial partial mirror ready=%v detail=%q err=%v; want not ready", ready, detail, err)
			}
			commandArgs := append([]string{"--json"}, args...)
			if args[0] == "import" {
				commandArgs = append(commandArgs, t.TempDir())
			}
			root.SetArgs(commandArgs)
			if err := root.Execute(); ExitCode(err) != 9 {
				t.Fatalf("partial-mirror consumer exit = %d (%v), output=%s; want precondition exit 9", ExitCode(err), err, out.String())
			}
		})
	}
}
