// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"zotio/internal/client"
	"zotio/internal/store"
)

const (
	recoveryCollKey = "CCCC0001"
	recoveryMemberA = "MMMM0001"
	recoveryMemberB = "MMMM0002"
)

func recoveryMember(key string, collections ...string) json.RawMessage {
	quoted := make([]string, len(collections))
	for i, c := range collections {
		quoted[i] = fmt.Sprintf("%q", c)
	}
	list := strings.Join(quoted, ",")
	return json.RawMessage(fmt.Sprintf(`{"key":%q,"version":5,"data":{"key":%q,"version":5,"itemType":"book","title":%q,"collections":[%s]}}`, key, key, key, list))
}

// recoveryPlane serves a plane on which collection C was erased: its key
// listing is empty and its members, at their unchanged version 5, name no
// collection. failBatch makes the itemKey batch refetch answer 500.
func recoveryPlane(t *testing.T, failBatch *atomic.Bool) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Last-Modified-Version", "11")
		switch {
		case r.URL.Path == "/users/0/collections" && r.URL.Query().Get("format") == "keys":
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Total-Results", "0")
		case r.URL.Path == "/users/0/items":
			if failBatch.Load() {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			var rows []string
			for _, key := range strings.Split(r.URL.Query().Get("itemKey"), ",") {
				rows = append(rows, string(recoveryMember(key)))
			}
			w.Header().Set("Total-Results", fmt.Sprint(len(rows)))
			_, _ = w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
		case strings.HasPrefix(r.URL.Path, "/users/0/items/"):
			_, _ = w.Write(recoveryMember(strings.TrimPrefix(r.URL.Path, "/users/0/items/")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return syncTestClient(srv.URL + "/users/0")
}

func recoverySeed(t *testing.T, dbPath string) *store.Store {
	t.Helper()
	db, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, _, err := db.UpsertBatch("collections", []json.RawMessage{
		json.RawMessage(`{"key":"` + recoveryCollKey + `","version":5,"data":{"key":"` + recoveryCollKey + `","version":5,"name":"Gone"}}`),
	}); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	if _, _, err := db.UpsertBatch("items", []json.RawMessage{
		recoveryMember(recoveryMemberA, recoveryCollKey),
		recoveryMember(recoveryMemberB, recoveryCollKey),
	}); err != nil {
		t.Fatalf("seed members: %v", err)
	}
	return db
}

func recoveryMemberCollections(t *testing.T, db *store.Store, key string) []any {
	t.Helper()
	raw, err := db.Get("items", key)
	if err != nil {
		t.Fatalf("Get(items/%s): %v", key, err)
	}
	var obj struct {
		Data struct {
			Collections []any `json:"collections"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	return obj.Data.Collections
}

func recoveryCollectionMirrored(t *testing.T, db *store.Store) bool {
	t.Helper()
	ids, err := db.ResourceIDs("collections")
	if err != nil {
		t.Fatalf("ResourceIDs(collections): %v", err)
	}
	return ids[recoveryCollKey]
}

var recoveryClean = map[string]bool{"collections": true, "items": true}

// Erasing a collection bumps no member version, so the mirrored collection row
// is the only thing that makes a later sync probe it and repair its members.
// The reap used to commit first: a process that died before the member repair
// left the members naming an erased collection, with nothing left to find the
// work again.
func TestIncrementalCollectionReapRecoversInterruptedMemberRepair(t *testing.T) {
	syncTestWithHumanFriendly(t, false)
	fastRetryBackoff(t)
	ctx := context.Background()

	t.Run("process dies after the collection sweep", func(t *testing.T) {
		var failBatch atomic.Bool
		c := recoveryPlane(t, &failBatch)
		dbPath := filepath.Join(t.TempDir(), "mirror.db")
		db := recoverySeed(t, dbPath)

		var out incrementalReapOutcome
		plan := sweepIncrementalCollections(ctx, c, db, &out)
		if len(plan.confirmed) != 1 || plan.confirmed[0] != recoveryCollKey {
			t.Fatalf("confirmed-erased collections = %v, want [%s]", plan.confirmed, recoveryCollKey)
		}
		if !recoveryCollectionMirrored(t, db) {
			t.Fatal("the erased collection row was removed before its members were repaired; a crash here loses the repair for good")
		}
		// The process dies here: nothing in memory survives.
		if err := db.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
		db, err := store.OpenWithContext(ctx, dbPath)
		if err != nil {
			t.Fatalf("reopen store: %v", err)
		}
		defer db.Close()

		restart := reconcileIncrementalSync(ctx, c, db, []string{"collections", "items"}, recoveryClean)
		if restart.incomplete {
			t.Errorf("restarted reconciliation incomplete: %+v", restart)
		}
		for _, key := range []string{recoveryMemberA, recoveryMemberB} {
			if got := recoveryMemberCollections(t, db, key); len(got) != 0 {
				t.Errorf("member %s data.collections = %v after the restarted sync, want [] (repaired without --full)", key, got)
			}
		}
		if recoveryCollectionMirrored(t, db) {
			t.Error("the erased collection row survived a fully repaired restart")
		}
	})

	t.Run("member batch fails, next sync retries it", func(t *testing.T) {
		var failBatch atomic.Bool
		c := recoveryPlane(t, &failBatch)
		db := recoverySeed(t, filepath.Join(t.TempDir(), "mirror.db"))
		defer db.Close()

		failBatch.Store(true)
		first := reconcileIncrementalSync(ctx, c, db, []string{"collections", "items"}, recoveryClean)
		if !first.incomplete {
			t.Error("a failed member refetch was not reported as an incomplete reconciliation")
		}
		if !recoveryCollectionMirrored(t, db) {
			t.Fatal("the collection row was removed although a member refetch failed; the next sync could no longer find the unrepaired member")
		}
		if got := recoveryMemberCollections(t, db, recoveryMemberB); len(got) != 1 {
			t.Fatalf("member %s data.collections = %v, want it still naming the collection after the failed batch", recoveryMemberB, got)
		}

		failBatch.Store(false)
		second := reconcileIncrementalSync(ctx, c, db, []string{"collections", "items"}, recoveryClean)
		if second.incomplete {
			t.Errorf("retry reconciliation incomplete: %+v", second)
		}
		for _, key := range []string{recoveryMemberA, recoveryMemberB} {
			if got := recoveryMemberCollections(t, db, key); len(got) != 0 {
				t.Errorf("member %s data.collections = %v after the retry, want []", key, got)
			}
		}
		if recoveryCollectionMirrored(t, db) {
			t.Error("the erased collection row survived the successful retry")
		}
	})

	t.Run("items not synced keeps the row for a later sync", func(t *testing.T) {
		var failBatch atomic.Bool
		c := recoveryPlane(t, &failBatch)
		db := recoverySeed(t, filepath.Join(t.TempDir(), "mirror.db"))
		defer db.Close()

		out := reconcileIncrementalSync(ctx, c, db, []string{"collections"}, map[string]bool{"collections": true})
		if !out.incomplete {
			t.Error("reaping an erased collection without syncing its members was not reported incomplete")
		}
		if !recoveryCollectionMirrored(t, db) {
			t.Fatal("the collection row was removed although items did not sync; a later `zotio sync` could no longer repair the members")
		}
		full := reconcileIncrementalSync(ctx, c, db, []string{"collections", "items"}, recoveryClean)
		if full.incomplete || recoveryCollectionMirrored(t, db) {
			t.Errorf("a later sync with items did not finish the repair: incomplete=%v mirrored=%v", full.incomplete, recoveryCollectionMirrored(t, db))
		}
		if got := recoveryMemberCollections(t, db, recoveryMemberA); len(got) != 0 {
			t.Errorf("member %s data.collections = %v, want []", recoveryMemberA, got)
		}
	})
}
