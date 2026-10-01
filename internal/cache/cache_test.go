// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.

package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	if names := ccDirNames(t, store.Dir); len(names) != 0 {
		t.Fatalf("expiry cleanup left files behind: %v", names)
	}
}

// A Set that lands after Get's cleanup has judged the entry expired and before
// it removes it must survive: the path now holds a fresh publication from a
// concurrent request. A check-then-remove cleanup deletes it, turning one slow
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

// RemoveExpired judges the file it actually moved, not the caller's earlier
// observation, so an entry that is fresh by then is never lost: it goes back,
// unless an even newer entry was published meanwhile, which it must not
// replace.
func TestRemoveExpiredNeverDropsAFreshEntry(t *testing.T) {
	cases := []struct {
		name    string
		publish string // published at the path while the entry is moved aside
		want    string
	}{
		{name: "fresh entry is put back", want: `{"v":1}`},
		{name: "newer entry is not replaced", publish: `{"v":2}`, want: `{"v":2}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "entry.json")
			if err := os.WriteFile(path, []byte(`{"v":1}`), 0o600); err != nil {
				t.Fatalf("seed entry: %v", err)
			}
			old := expiredCleanupProbe
			expiredCleanupProbe = func(string) {
				if tc.publish == "" {
					return
				}
				if err := os.WriteFile(path, []byte(tc.publish), 0o600); err != nil {
					t.Errorf("concurrent publish: %v", err)
				}
			}
			t.Cleanup(func() { expiredCleanupProbe = old })

			RemoveExpired(path, time.Minute)

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("fresh entry lost by expiry cleanup: %v", err)
			}
			if string(data) != tc.want {
				t.Fatalf("entry = %s, want %s", data, tc.want)
			}
			if names := ccDirNames(t, dir); len(names) != 1 {
				t.Fatalf("expiry cleanup left files behind: %v", names)
			}
		})
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
