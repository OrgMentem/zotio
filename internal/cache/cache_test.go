// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cache

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestStoreSetGet(t *testing.T) {
	store := New(t.TempDir(), time.Hour)
	value := json.RawMessage(`{"ok":true,"n":2}`)

	if err := store.Set("GET /items?limit=1", value); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok := store.Get("GET /items?limit=1")
	if !ok {
		t.Fatal("Get did not find value written by Set")
	}
	if string(got) != string(value) {
		t.Fatalf("Get = %s, want %s", got, value)
	}
	if _, ok := store.Get("GET /items?limit=2"); ok {
		t.Fatal("Get for a different key returned the stored value")
	}
}

func TestStoreGetRespectsTTL(t *testing.T) {
	store := New(t.TempDir(), time.Minute)
	key := "GET /items/K1"
	if err := store.Set(key, json.RawMessage(`{"key":"K1"}`)); err != nil {
		t.Fatalf("Set: %v", err)
	}

	expired := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(store.path(key), expired, expired); err != nil {
		t.Fatalf("age cache file: %v", err)
	}
	if got, ok := store.Get(key); ok {
		t.Fatalf("expired cache entry returned (%s), want miss", got)
	}
	if _, err := os.Stat(store.path(key)); !os.IsNotExist(err) {
		t.Fatalf("expired cache entry still exists, stat error = %v", err)
	}
}

// A Set that lands between Get's expiry check and its cleanup remove must
// survive: Get observed an expired file, but the path now holds a fresh
// publication from a concurrent request. Deleting it would turn one slow
// reader into a lost refresh and an avoidable upstream refetch.
func TestStoreGetKeepsConcurrentFreshWrite(t *testing.T) {
	store := New(t.TempDir(), time.Minute)
	key := "GET /items/K1"
	if err := store.Set(key, json.RawMessage(`{"key":"stale"}`)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	expired := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(store.path(key), expired, expired); err != nil {
		t.Fatalf("age cache file: %v", err)
	}
	fresh := json.RawMessage(`{"key":"fresh"}`)
	old := expiredCleanupProbe
	expiredCleanupProbe = func(string) {
		if err := store.Set(key, fresh); err != nil {
			t.Errorf("concurrent Set: %v", err)
		}
	}
	t.Cleanup(func() { expiredCleanupProbe = old })
	got, ok := store.Get(key)
	if !ok {
		t.Fatal("Get missed a fresh value published during expiry cleanup")
	}
	if string(got) != string(fresh) {
		t.Fatalf("Get = %s, want %s", got, fresh)
	}
	if _, err := os.Stat(store.path(key)); err != nil {
		t.Fatalf("fresh cache entry was deleted by expiry cleanup: %v", err)
	}
}
