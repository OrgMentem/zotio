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
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zotio/internal/store"
)

// Wire fixtures use the real Zotero wire shapes: format=keys bodies are
// newline-separated text (text/plain, no trailing newline, empty body for an
// empty set — see Zotero's server_localAPI.js makeDataObjectResponse), while
// object listings stay JSON.

// Regression test for zotio-e647c89d4ad919eb: an incremental sync must reap a
// collection erased upstream, refresh the members that listed it (at an
// unchanged version the items pass never refetches), and reap a purged trash
// row. Without the reconciliation the mirror keeps all three, and only
// `sync --full` heals it.
func TestSyncIncrementalReapsDeletedCollectionAndMembers(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	fastRetryBackoff(t)

	const (
		collKey  = "CCCC0001"
		itemKey  = "MMMM0001"
		trashKey = "TTTT0001"
	)
	collJSON := `{"key":"` + collKey + `","version":5,"data":{"key":"` + collKey + `","version":5,"name":"Gone"}}`
	memberListed := `{"key":"` + itemKey + `","version":5,"data":{"key":"` + itemKey + `","version":5,"itemType":"book","title":"Member","collections":["` + collKey + `"]}}`
	memberUnfiled := `{"key":"` + itemKey + `","version":5,"data":{"key":"` + itemKey + `","version":5,"itemType":"book","title":"Member","collections":[]}}`
	trashJSON := `{"key":"` + trashKey + `","version":6,"data":{"key":"` + trashKey + `","version":6,"itemType":"book","title":"Trash","deleted":1}}`

	var phase atomic.Int32 // 0 = mirror with C and member M; 1 = C erased, M unfiled, trash purged
	var keysListings atomic.Int64
	var memberGets atomic.Int64
	var keysParams atomic.Value // last query params seen on a keys listing
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		erased := phase.Load() == 1
		since := r.URL.Query().Get("since")
		changedOnly := since != ""
		version := "10"
		if erased {
			version = "11"
		}
		w.Header().Set("Last-Modified-Version", version)
		switch r.URL.Path {
		case "/users/0/collections":
			if r.URL.Query().Get("format") == "keys" {
				keysListings.Add(1)
				keysParams.Store(r.URL.Query().Encode())
				w.Header().Set("Content-Type", "text/plain")
				if erased {
					w.Header().Set("Total-Results", "0")
					_, _ = w.Write([]byte(``))
				} else {
					w.Header().Set("Total-Results", "1")
					_, _ = w.Write([]byte(collKey))
				}
				return
			}
			if changedOnly && erased {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[` + collJSON + `]`))
		case "/users/0/items":
			if changedOnly && erased {
				// M is unchanged at version 5: the items pass never refetches it.
				_, _ = w.Write([]byte(`[]`))
				return
			}
			if erased {
				_, _ = w.Write([]byte(`[` + memberUnfiled + `]`))
				return
			}
			_, _ = w.Write([]byte(`[` + memberListed + `]`))
		case "/users/0/items/" + itemKey:
			memberGets.Add(1)
			if erased {
				_, _ = w.Write([]byte(memberUnfiled))
				return
			}
			_, _ = w.Write([]byte(memberListed))
		case "/users/0/items/trash":
			if r.URL.Query().Get("format") == "keys" {
				keysListings.Add(1)
				keysParams.Store(r.URL.Query().Encode())
				w.Header().Set("Content-Type", "text/plain")
				if erased {
					w.Header().Set("Total-Results", "0")
					_, _ = w.Write([]byte(``))
				} else {
					w.Header().Set("Total-Results", "1")
					_, _ = w.Write([]byte(trashKey))
				}
				return
			}
			if changedOnly && erased {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			if erased {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[` + trashJSON + `]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "sync.db")
	runSync := func(t *testing.T) {
		t.Helper()
		cmd := newSyncCmd(&rootFlags{
			configPath: testConfigFile(t, srv.URL+"/users/0"),
			timeout:    30 * time.Second,
		})
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--resources", "collections,items,items-trash", "--db", dbPath})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("sync error = %v", err)
		}
	}

	// Phase 0: mirror collection C, member M listing C, and trash row T.
	runSync(t)

	// Phase 1: C erased upstream, M unfiled at an unchanged version, T purged.
	phase.Store(1)
	keysListings.Store(0)
	memberGets.Store(0)
	runSync(t)

	// The reconciliation costs exactly two key listings plus one member
	// refetch — bounded, and zero when nothing was reaped.
	if got := keysListings.Load(); got != 2 {
		t.Errorf("key-listing requests = %d, want 2 (collections + trash)", got)
	}
	if got := memberGets.Load(); got != 1 {
		t.Errorf("member refetch requests = %d, want 1 (only M listed C)", got)
	}
	// The listings are single unpaginated fetches: no limit/start slicing,
	// so one response is one consistent snapshot and concurrent changes
	// cannot shift offsets between pages.
	if params, ok := keysParams.Load().(string); !ok || params != "format=keys" {
		t.Errorf("keys listing query = %q, want exactly \"format=keys\"", params)
	}

	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("opening mirror: %v", err)
	}
	defer db.Close()

	if ids, err := db.ResourceIDs("collections"); err != nil {
		t.Fatalf("ResourceIDs(collections) error = %v", err)
	} else if len(ids) != 0 {
		t.Errorf("mirrored collections = %v, want none (C erased upstream)", ids)
	}
	raw, err := db.Get("items", itemKey)
	if err != nil {
		t.Fatalf("Get(items/%s) error = %v", itemKey, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal member: %v", err)
	}
	data, _ := obj["data"].(map[string]any)
	colls, _ := data["collections"].([]any)
	if len(colls) != 0 {
		t.Errorf("member data.collections = %v, want [] (plane reports M unfiled)", colls)
	}
	if n, err := db.Count("items-trash"); err != nil {
		t.Fatalf("Count(items-trash) error = %v", err)
	} else if n != 0 {
		t.Errorf("mirrored items-trash rows = %d, want 0 (T purged upstream)", n)
	}
}

// A trashed collection is not an erased one: the desktop omits it from
// /collections but keeps member links for restore (trash() only inserts into
// deletedCollections; erase deletes the collectionItems rows). The
// reconciliation must therefore spare an absent collection while any live
// member still names it on the plane, and a later restore must need no
// repair at all.
func TestSyncIncrementalSparesTrashedCollectionAndRestoreNeedsNoRepair(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	fastRetryBackoff(t)

	const (
		collKey  = "CCCC0002"
		itemKey  = "MMMM0002"
		trashKey = "TTTT0002"
	)
	collJSON := func(version string) string {
		return `{"key":"` + collKey + `","version":` + version + `,"data":{"key":"` + collKey + `","version":` + version + `,"name":"Back"}}`
	}
	memberListed := `{"key":"` + itemKey + `","version":5,"data":{"key":"` + itemKey + `","version":5,"itemType":"book","title":"Member","collections":["` + collKey + `"]}}`
	trashJSON := `{"key":"` + trashKey + `","version":6,"data":{"key":"` + trashKey + `","version":6,"itemType":"book","title":"Trash","deleted":1}}`

	var phase atomic.Int32 // 0 = mirror; 1 = C trashed; 2 = C restored
	var keysListings atomic.Int64
	var memberGets atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		trashed := phase.Load() == 1
		restored := phase.Load() == 2
		w.Header().Set("Last-Modified-Version", "10")
		switch r.URL.Path {
		case "/users/0/collections":
			if r.URL.Query().Get("format") == "keys" {
				keysListings.Add(1)
				w.Header().Set("Content-Type", "text/plain")
				if trashed {
					// Trashed collections are omitted from the listing …
					w.Header().Set("Total-Results", "0")
					_, _ = w.Write([]byte(``))
				} else {
					w.Header().Set("Total-Results", "1")
					_, _ = w.Write([]byte(collKey))
				}
				return
			}
			if r.URL.Query().Get("since") != "" {
				if restored {
					_, _ = w.Write([]byte(`[` + collJSON("6") + `]`))
					return
				}
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[` + collJSON("5") + `]`))
		case "/users/0/items":
			if r.URL.Query().Get("since") != "" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[` + memberListed + `]`))
		case "/users/0/items/" + itemKey:
			memberGets.Add(1)
			// … but the plane keeps the member link while C is trashed.
			_, _ = w.Write([]byte(memberListed))
		case "/users/0/items/trash":
			if r.URL.Query().Get("format") == "keys" {
				keysListings.Add(1)
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Total-Results", "1")
				_, _ = w.Write([]byte(trashKey))
				return
			}
			if r.URL.Query().Get("since") != "" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[` + trashJSON + `]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "sync.db")
	runSync := func(t *testing.T) {
		t.Helper()
		cmd := newSyncCmd(&rootFlags{
			configPath: testConfigFile(t, srv.URL+"/users/0"),
			timeout:    30 * time.Second,
		})
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--resources", "collections,items,items-trash", "--db", dbPath})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("sync error = %v", err)
		}
	}
	assertMirrored := func(t *testing.T, want string) {
		t.Helper()
		db, err := store.OpenWithContext(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("opening mirror: %v", err)
		}
		defer db.Close()
		if ids, err := db.ResourceIDs("collections"); err != nil {
			t.Fatalf("ResourceIDs error = %v", err)
		} else if len(ids) != 1 || !ids[collKey] {
			t.Fatalf("mirrored collections = %v, want [%s] (%s)", ids, collKey, want)
		}
		raw, err := db.Get("items", itemKey)
		if err != nil {
			t.Fatalf("Get member error = %v", err)
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("unmarshal member: %v", err)
		}
		data, _ := obj["data"].(map[string]any)
		colls, _ := data["collections"].([]any)
		if len(colls) != 1 || colls[0] != collKey {
			t.Fatalf("member data.collections = %v, want [%s] (%s)", colls, collKey, want)
		}
	}

	runSync(t)

	// Trash C: the probe (one member GET) sees the plane still names C, so
	// the row and the links stay.
	phase.Store(1)
	memberGets.Store(0)
	runSync(t)
	assertMirrored(t, "trashed collection spared with links intact")
	if got := memberGets.Load(); got != 1 {
		t.Errorf("member requests in trash phase = %d, want 1 (the probe)", got)
	}

	// Restore C: it reappears with a bumped version, the links were never
	// stripped, so no member refetch is needed.
	phase.Store(2)
	memberGets.Store(0)
	runSync(t)
	assertMirrored(t, "restored collection needs no repair")
	if got := memberGets.Load(); got != 0 {
		t.Errorf("member requests in restore phase = %d, want 0", got)
	}
}

// A full pass already sweeps everything, so the incremental reconciliation
// must not fire there: no key listings, no member refetches.
func TestSyncIncrementalReconciliationSkippedOnFull(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	fastRetryBackoff(t)

	const itemKey = "MMMM0003"
	memberUnfiled := `{"key":"` + itemKey + `","version":5,"data":{"key":"` + itemKey + `","version":5,"itemType":"book","title":"Member","collections":[]}}`

	var keysListings atomic.Int64
	var memberGets atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Last-Modified-Version", "11")
		switch {
		case r.URL.Path == "/users/0/collections" && r.URL.Query().Get("format") == "keys":
			keysListings.Add(1)
			_, _ = w.Write([]byte(``))
		case r.URL.Path == "/users/0/collections" || r.URL.Path == "/users/0/items/trash":
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/users/0/items":
			_, _ = w.Write([]byte(`[` + memberUnfiled + `]`))
		case r.URL.Path == "/users/0/items/"+itemKey:
			memberGets.Add(1)
			_, _ = w.Write([]byte(memberUnfiled))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cmd := newSyncCmd(&rootFlags{
		configPath: testConfigFile(t, srv.URL+"/users/0"),
		timeout:    30 * time.Second,
	})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--full", "--resources", "collections,items,items-trash", "--db", filepath.Join(t.TempDir(), "sync.db")})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("full sync error = %v", err)
	}
	if got := keysListings.Load(); got != 0 {
		t.Errorf("key-listing requests during --full = %d, want 0", got)
	}
	if got := memberGets.Load(); got != 0 {
		t.Errorf("member refetch requests during --full = %d, want 0", got)
	}
}

// A 200 with an empty body and no Total-Results header is missing evidence,
// not an empty library: a proxy that swallows the body must never let a
// sweep delete every mirrored collection. The same holds for a non-empty
// listing without the header.
func TestSyncIncrementalSkipsSweepWithoutTotalResults(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	fastRetryBackoff(t)

	const (
		collKey = "CCCC0004"
		itemKey = "MMMM0004"
	)
	collJSON := `{"key":"` + collKey + `","version":5,"data":{"key":"` + collKey + `","version":5,"name":"Kept"}}`
	memberListed := `{"key":"` + itemKey + `","version":5,"data":{"key":"` + itemKey + `","version":5,"itemType":"book","title":"Member","collections":["` + collKey + `"]}}`
	// The member endpoint models an erase (links dropped on the plane), so
	// any code that sweeps without header evidence reaps and repairs here.
	memberUnfiled := `{"key":"` + itemKey + `","version":5,"data":{"key":"` + itemKey + `","version":5,"itemType":"book","title":"Member","collections":[]}}`

	var keysListings atomic.Int64
	var memberGets atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Last-Modified-Version", "11")
		switch r.URL.Path {
		case "/users/0/collections":
			if r.URL.Query().Get("format") == "keys" {
				keysListings.Add(1)
				w.Header().Set("Content-Type", "text/plain")
				// Deliberately no Total-Results: missing evidence.
				_, _ = w.Write([]byte(``))
				return
			}
			if r.URL.Query().Get("since") != "" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[` + collJSON + `]`))
		case "/users/0/items":
			if r.URL.Query().Get("since") != "" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[` + memberListed + `]`))
		case "/users/0/items/" + itemKey:
			memberGets.Add(1)
			_, _ = w.Write([]byte(memberUnfiled))
		case "/users/0/items/trash":
			if r.URL.Query().Get("format") == "keys" {
				keysListings.Add(1)
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Total-Results", "0")
				_, _ = w.Write([]byte(``))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "sync.db")
	runSync := func(t *testing.T) {
		t.Helper()
		cmd := newSyncCmd(&rootFlags{
			configPath: testConfigFile(t, srv.URL+"/users/0"),
			timeout:    30 * time.Second,
		})
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--resources", "collections,items,items-trash", "--db", dbPath})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("sync error = %v", err)
		}
	}
	runSync(t)
	runSync(t)

	if got := keysListings.Load(); got != 4 {
		t.Errorf("key-listing requests = %d, want 4 (2 per sync)", got)
	}
	if got := memberGets.Load(); got != 0 {
		t.Errorf("member requests = %d, want 0 (no confirmed erase)", got)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("opening mirror: %v", err)
	}
	defer db.Close()
	if ids, err := db.ResourceIDs("collections"); err != nil {
		t.Fatalf("ResourceIDs error = %v", err)
	} else if len(ids) != 1 || !ids[collKey] {
		t.Fatalf("mirrored collections = %v, want [%s] kept without header evidence", ids, collKey)
	}
	raw, err := db.Get("items", itemKey)
	if err != nil {
		t.Fatalf("Get member error = %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal member: %v", err)
	}
	data, _ := obj["data"].(map[string]any)
	colls, _ := data["collections"].([]any)
	if len(colls) != 1 || colls[0] != collKey {
		t.Fatalf("member data.collections = %v, want [%s] untouched", colls, collKey)
	}
}

// An erased collection with 51 mirrored members must cost 2 member requests
// (1 probe plus ceil(50/50) batch), not 51 sequential GETs. Members are
// fetched via /items?itemKey= (up to 50 per request) and upserted in batches.
func TestSyncIncrementalBatchesMemberRefetch(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	fastRetryBackoff(t)

	const (
		collKey = "CCCC0005"
		members = 51
	)
	keyOf := func(i int) string { return fmt.Sprintf("M%07d", i) }
	memberListed := func(key string) string {
		return `{"key":"` + key + `","version":5,"data":{"key":"` + key + `","version":5,"itemType":"book","title":"Member","collections":["` + collKey + `"]}}`
	}
	memberUnfiled := func(key string) string {
		return `{"key":"` + key + `","version":5,"data":{"key":"` + key + `","version":5,"itemType":"book","title":"Member","collections":[]}}`
	}
	collJSON := `{"key":"` + collKey + `","version":5,"data":{"key":"` + collKey + `","version":5,"name":"Gone"}}`
	allKeys := make([]string, 0, members)
	for i := range members {
		allKeys = append(allKeys, keyOf(i))
	}

	var phase atomic.Int32
	var memberGets atomic.Int64
	var batchGets atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		erased := phase.Load() == 1
		w.Header().Set("Last-Modified-Version", "11")
		switch r.URL.Path {
		case "/users/0/collections":
			if r.URL.Query().Get("format") == "keys" {
				w.Header().Set("Content-Type", "text/plain")
				if erased {
					w.Header().Set("Total-Results", "0")
					_, _ = w.Write([]byte(``))
				} else {
					w.Header().Set("Total-Results", "1")
					_, _ = w.Write([]byte(collKey))
				}
				return
			}
			if r.URL.Query().Get("since") != "" && erased {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[` + collJSON + `]`))
		case "/users/0/items":
			if itemKeys := r.URL.Query().Get("itemKey"); itemKeys != "" {
				batchGets.Add(1)
				var out []string
				for _, k := range strings.Split(itemKeys, ",") {
					out = append(out, memberUnfiled(k))
				}
				_, _ = w.Write([]byte(`[` + strings.Join(out, ",") + `]`))
				return
			}
			if r.URL.Query().Get("since") != "" && erased {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			var out []string
			for _, k := range allKeys {
				out = append(out, memberListed(k))
			}
			_, _ = w.Write([]byte(`[` + strings.Join(out, ",") + `]`))
		case "/users/0/items/trash":
			if r.URL.Query().Get("format") == "keys" {
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Total-Results", "0")
				_, _ = w.Write([]byte(``))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		default:
			if strings.HasPrefix(r.URL.Path, "/users/0/items/") {
				memberGets.Add(1)
				key := strings.TrimPrefix(r.URL.Path, "/users/0/items/")
				_, _ = w.Write([]byte(memberUnfiled(key)))
				return
			}
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dbPath := filepath.Join(t.TempDir(), "sync.db")
	runSync := func(t *testing.T) {
		t.Helper()
		cmd := newSyncCmd(&rootFlags{
			configPath: testConfigFile(t, srv.URL+"/users/0"),
			timeout:    30 * time.Second,
		})
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"--resources", "collections,items,items-trash", "--db", dbPath})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("sync error = %v", err)
		}
	}
	runSync(t)
	phase.Store(1)
	memberGets.Store(0)
	batchGets.Store(0)
	runSync(t)

	if got := memberGets.Load(); got != 1 {
		t.Errorf("single member requests = %d, want 1 (the probe)", got)
	}
	if got := batchGets.Load(); got != 1 {
		t.Errorf("batch member requests = %d, want 1 (remaining 50 in one itemKey batch)", got)
	}
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("opening mirror: %v", err)
	}
	defer db.Close()
	for _, k := range allKeys {
		raw, err := db.Get("items", k)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", k, err)
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("unmarshal %s: %v", k, err)
		}
		data, _ := obj["data"].(map[string]any)
		colls, _ := data["collections"].([]any)
		if len(colls) != 0 {
			t.Fatalf("member %s data.collections = %v, want []", k, colls)
		}
	}
}

// parsePlaneKeySet reads the documented text wire format: newline-separated
// keys, no trailing newline, empty body for an empty set. Anything else is
// refused, because the sweep that consumes the keys must never run on
// guessed evidence.
func TestSyncReapParsePlaneKeySet(t *testing.T) {
	syncTestWithHumanFriendly(t, false)

	keys, ok := parsePlaneKeySet(json.RawMessage("A1B2C3D4\nE5F6G7H8"))
	if !ok || len(keys) != 2 || keys[0] != "A1B2C3D4" || keys[1] != "E5F6G7H8" {
		t.Errorf("two lines = %v,%v, want [A1B2C3D4 E5F6G7H8],true", keys, ok)
	}
	keys, ok = parsePlaneKeySet(json.RawMessage("A1B2C3D4\n"))
	if !ok || len(keys) != 1 || keys[0] != "A1B2C3D4" {
		t.Errorf("trailing newline = %v,%v, want [A1B2C3D4],true", keys, ok)
	}
	keys, ok = parsePlaneKeySet(json.RawMessage(""))
	if !ok || len(keys) != 0 {
		t.Errorf("empty body = %v,%v, want [],true", keys, ok)
	}
	keys, ok = parsePlaneKeySet(json.RawMessage("  \n "))
	if !ok || len(keys) != 0 {
		t.Errorf("blank body = %v,%v, want [],true", keys, ok)
	}
	for name, body := range map[string]string{
		"json array":   `["A1B2C3D4"]`,
		"json object":  `{"key":"A1B2C3D4"}`,
		"garbage":      `not json`,
		"junk line":    "A1B2C3D4\nnot-a-key!",
		"short key":    "SHORT",
		"lowercase":    "a1b2c3d4",
		"empty middle": "A1B2C3D4\n\n\n",
	} {
		_, ok := parsePlaneKeySet(json.RawMessage(body))
		if name == "empty middle" {
			if !ok {
				t.Errorf("%s (%q) refused, want tolerance for blank lines", name, body)
			}
			continue
		}
		if ok {
			t.Errorf("%s (%q) parsed ok, want refusal", name, body)
		}
	}
}

// fetchPlaneKeySet issues one unpaginated request: both planes return the
// full key set when limit is omitted (local API docs; dataserver applies
// LIMIT only when the param is present), so one response is one consistent
// snapshot and concurrent changes cannot shift offsets between pages. A
// Total-Results mismatch means the body is not the whole set and refuses it.
func TestSyncReapKeysListingSingleRequest(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	ctx := context.Background()

	body101 := strings.Join(validReapKeys101(), "\n")

	t.Run("allKeysOneRequest", func(t *testing.T) {
		var calls int
		var params map[string]string
		stub := &syncReapStubClient{
			bodies:      []string{body101},
			headerValue: "101",
			onCall: func(p map[string]string) {
				calls++
				params = p
			},
		}
		var out incrementalReapOutcome
		seen, complete := fetchPlaneKeySet(ctx, stub, "/collections", "collections", &out)
		if !complete {
			t.Fatal("complete = false, want true for one full listing")
		}
		if len(seen) != 101 {
			t.Errorf("seen keys = %d, want 101", len(seen))
		}
		if calls != 1 {
			t.Errorf("requests = %d, want exactly 1 (unpaginated)", calls)
		}
		if len(params) != 1 || params["format"] != "keys" {
			t.Errorf("listing params = %v, want only format=keys", params)
		}
		if out.requests != 1 {
			t.Errorf("outcome.requests = %d, want 1", out.requests)
		}
	})

	t.Run("totalResultsMismatch", func(t *testing.T) {
		stub := &syncReapStubClient{
			bodies:      []string{"A1B2C3D4\nE5F6G7H8"},
			headerValue: "5",
		}
		var out incrementalReapOutcome
		if _, complete := fetchPlaneKeySet(ctx, stub, "/collections", "collections", &out); complete {
			t.Error("complete = true despite Total-Results 5 with 2 keys, want false")
		}
	})

	t.Run("totalResultsMatch", func(t *testing.T) {
		stub := &syncReapStubClient{
			bodies:      []string{"A1B2C3D4\nE5F6G7H8"},
			headerValue: "2",
		}
		var out incrementalReapOutcome
		seen, complete := fetchPlaneKeySet(ctx, stub, "/collections", "collections", &out)
		if !complete || len(seen) != 2 {
			t.Errorf("seen,complete = %v,%v, want 2 keys,true", seen, complete)
		}
	})

	t.Run("missingHeader", func(t *testing.T) {
		// No Total-Results at all is missing evidence, even with keys present.
		stub := &syncReapStubClient{
			bodies: []string{"A1B2C3D4\nE5F6G7H8"},
		}
		var out incrementalReapOutcome
		if _, complete := fetchPlaneKeySet(ctx, stub, "/collections", "collections", &out); complete {
			t.Error("complete = true without Total-Results, want false")
		}
	})

	t.Run("emptyBodyNoHeader", func(t *testing.T) {
		// A 200 with an empty body and no header must never read as empty.
		stub := &syncReapStubClient{
			bodies: []string{``},
		}
		var out incrementalReapOutcome
		if _, complete := fetchPlaneKeySet(ctx, stub, "/collections", "collections", &out); complete {
			t.Error("complete = true for headerless empty body, want false")
		}
	})

	t.Run("failure", func(t *testing.T) {
		stub := &syncReapStubClient{err: errors.New("boom")}
		var out incrementalReapOutcome
		if _, complete := fetchPlaneKeySet(ctx, stub, "/collections", "collections", &out); complete {
			t.Error("complete = true on request failure, want false (never sweep partial evidence)")
		}
	})
}

// validReapKeys101 returns 101 distinct valid Zotero keys.
func validReapKeys101() []string {
	keys := make([]string, 0, 101)
	for i := range 101 {
		keys = append(keys, "KAAAAA"+string(rune('A'+i/26))+string(rune('A'+i%26)))
	}
	return keys
}

// syncReapStubClient is a canned syncHTTPClient serving fixed listing bodies,
// with an optional Total-Results header value.
type syncReapStubClient struct {
	bodies      []string
	calls       int
	onCall      func(map[string]string)
	err         error
	headerValue string
}

func (c *syncReapStubClient) GetWithVersion(_ string, params map[string]string) (json.RawMessage, int, error) {
	return c.GetWithVersionContext(context.Background(), "", params)
}

func (c *syncReapStubClient) GetWithVersionContext(_ context.Context, _ string, params map[string]string) (json.RawMessage, int, error) {
	if c.err != nil {
		return nil, 0, c.err
	}
	if c.onCall != nil {
		c.onCall(params)
	}
	body := ``
	if c.calls < len(c.bodies) {
		body = c.bodies[c.calls]
	}
	c.calls++
	return json.RawMessage(body), 0, nil
}

func (c *syncReapStubClient) GetWithHeaderContext(_ context.Context, _ string, params map[string]string, _ string) (json.RawMessage, string, error) {
	data, _, err := c.GetWithVersionContext(context.Background(), "", params)
	return data, c.headerValue, err
}

func (c *syncReapStubClient) RateLimit() float64 { return 0 }

func (c *syncReapStubClient) Plane() string { return "test://reap-stub" }
