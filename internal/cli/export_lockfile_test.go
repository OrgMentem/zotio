// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase6 d27f99d4): cover canonical lockfile fingerprint stability.

package cli

import (
	"encoding/json"
	"testing"
)

func TestBuildExportLockfileCanonicalizesItems(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"key":"B","version":2}`),
		json.RawMessage(`{"key":"A","version":1}`),
	}

	lockfile := buildExportLockfile("items", "json", items)
	if lockfile.Count != 2 {
		t.Fatalf("Count = %d, want 2", lockfile.Count)
	}
	if len(lockfile.Items) != 2 {
		t.Fatalf("len(Items) = %d, want 2", len(lockfile.Items))
	}
	if lockfile.Items[0] != (exportLockItem{Key: "A", Version: 1}) {
		t.Fatalf("Items[0] = %#v, want A version 1", lockfile.Items[0])
	}
	if lockfile.Items[1] != (exportLockItem{Key: "B", Version: 2}) {
		t.Fatalf("Items[1] = %#v, want B version 2", lockfile.Items[1])
	}
	if lockfile.ContentSHA256 == "" {
		t.Fatal("ContentSHA256 is empty")
	}
}

func TestBuildExportLockfileHashIsOrderInvariant(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"key":"B","version":2}`),
		json.RawMessage(`{"key":"A","version":1}`),
	}
	reversedItems := []json.RawMessage{
		json.RawMessage(`{"key":"A","version":1}`),
		json.RawMessage(`{"key":"B","version":2}`),
	}

	lockfile := buildExportLockfile("items", "json", items)
	reversedLockfile := buildExportLockfile("items", "json", reversedItems)
	if lockfile.ContentSHA256 != reversedLockfile.ContentSHA256 {
		t.Fatalf("ContentSHA256 changed with input order: %s != %s", lockfile.ContentSHA256, reversedLockfile.ContentSHA256)
	}
}

func TestBuildExportLockfileHashChangesWithVersion(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"key":"B","version":2}`),
		json.RawMessage(`{"key":"A","version":1}`),
	}
	changedVersionItems := []json.RawMessage{
		json.RawMessage(`{"key":"B","version":3}`),
		json.RawMessage(`{"key":"A","version":1}`),
	}

	lockfile := buildExportLockfile("items", "json", items)
	changedVersionLockfile := buildExportLockfile("items", "json", changedVersionItems)
	if lockfile.ContentSHA256 == changedVersionLockfile.ContentSHA256 {
		t.Fatalf("ContentSHA256 did not change after version changed: %s", lockfile.ContentSHA256)
	}
}
