// Copyright 2026 enieuwy and contributors. Licensed under Apache-2.0. See LICENSE.

package cache

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestStoreSetGetClear(t *testing.T) {
	store := New(t.TempDir(), time.Hour)
	value := json.RawMessage(`{"ok":true,"n":2}`)

	store.Set("GET /items?limit=1", value)
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

	if err := store.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, ok := store.Get("GET /items?limit=1"); ok {
		t.Fatal("Get found value after Clear")
	}
}

func TestStoreGetRespectsTTL(t *testing.T) {
	store := New(t.TempDir(), time.Minute)
	key := "GET /items/K1"
	store.Set(key, json.RawMessage(`{"key":"K1"}`))

	expired := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(store.path(key), expired, expired); err != nil {
		t.Fatalf("age cache file: %v", err)
	}
	if got, ok := store.Get(key); ok {
		t.Fatalf("expired cache entry returned (%s), want miss", got)
	}
}
