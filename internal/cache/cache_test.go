// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cache

import (
	"encoding/json"
	"os"
	"strings"
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
}

// Get must never delete or replace what is at the path. Writers publish by
// atomic rename, so a fresh Set can land between any check a reader makes and
// any removal it issues by name; a read path that removes expired entries can
// therefore delete a value a concurrent Set just published. An expired read
// is a miss that leaves the directory exactly as it found it, and the next Set
// replaces the stale entry.
func TestStoreGetKeepsConcurrentFreshWrite(t *testing.T) {
	store := New(t.TempDir(), time.Minute)
	key := "GET /items/K1"
	stale := json.RawMessage(`{"key":"stale"}`)
	if err := store.Set(key, stale); err != nil {
		t.Fatalf("Set: %v", err)
	}
	expired := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(store.path(key), expired, expired); err != nil {
		t.Fatalf("age cache file: %v", err)
	}
	before := ccDirNames(t, store.Dir)

	if got, ok := store.Get(key); ok {
		t.Fatalf("expired cache entry returned (%s), want miss", got)
	}
	if after := ccDirNames(t, store.Dir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("expired Get changed the cache dir: %v -> %v", before, after)
	}
	if data, err := os.ReadFile(store.path(key)); err != nil || string(data) != string(stale) {
		t.Fatalf("expired Get touched the entry: (%s, %v)", data, err)
	}

	fresh := json.RawMessage(`{"key":"fresh"}`)
	if err := store.Set(key, fresh); err != nil {
		t.Fatalf("Set fresh: %v", err)
	}
	got, ok := store.Get(key)
	if !ok || string(got) != string(fresh) {
		t.Fatalf("Get after fresh Set = (%s, %v), want %s", got, ok, fresh)
	}
}

func ccDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
