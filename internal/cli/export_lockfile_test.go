// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

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

	lockfile, err := buildExportLockfile("items", "json", items)
	if err != nil {
		t.Fatalf("build lockfile: %v", err)
	}
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

	lockfile, err := buildExportLockfile("items", "json", items)
	if err != nil {
		t.Fatalf("build lockfile: %v", err)
	}
	reversedLockfile, err := buildExportLockfile("items", "json", reversedItems)
	if err != nil {
		t.Fatalf("build reversed lockfile: %v", err)
	}
	if lockfile.ContentSHA256 != reversedLockfile.ContentSHA256 {
		t.Fatalf("ContentSHA256 changed with input order: %s != %s", lockfile.ContentSHA256, reversedLockfile.ContentSHA256)
	}
}

// The lockfile hash must track item CONTENT, not the fields Zotero rewrites on
// its own. export snapshot verify reports drift from this hash, and Zotero
// bumps version and dateModified whenever the desktop re-touches an item, so a
// hash that followed them would report drift for every idle sync.
// exportVolatileItemField strips four fields; each needs a witness here, or
// dropping one silently widens the hash and produces false drift.
func TestBuildExportLockfileHashTracksContentNotVolatileFields(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"key":"A","version":1,"data":{"key":"A","version":1,"title":"First","dateAdded":"2026-01-01T00:00:00Z","dateModified":"2026-01-01T00:00:00Z"}}`),
		json.RawMessage(`{"key":"B","version":2,"data":{"key":"B","version":2,"title":"Second","dateAdded":"2026-01-02T00:00:00Z","dateModified":"2026-01-02T00:00:00Z"}}`),
	}
	// Item B as Zotero leaves it after an edit that changed nothing the export
	// carries: new version, new dateModified, same content.
	volatileChurn := []json.RawMessage{
		json.RawMessage(`{"key":"A","version":1,"data":{"key":"A","version":1,"title":"First","dateAdded":"2026-01-01T00:00:00Z","dateModified":"2026-01-01T00:00:00Z"}}`),
		json.RawMessage(`{"key":"B","version":9,"data":{"key":"B","version":9,"title":"Second","dateAdded":"2026-03-03T00:00:00Z","dateModified":"2026-03-03T00:00:00Z"}}`),
	}
	contentChange := []json.RawMessage{
		json.RawMessage(`{"key":"A","version":1,"data":{"key":"A","version":1,"title":"First","dateAdded":"2026-01-01T00:00:00Z","dateModified":"2026-01-01T00:00:00Z"}}`),
		json.RawMessage(`{"key":"B","version":2,"data":{"key":"B","version":2,"title":"Second, revised","dateAdded":"2026-01-02T00:00:00Z","dateModified":"2026-01-02T00:00:00Z"}}`),
	}

	baseline, err := buildExportLockfile("items", "json", items)
	if err != nil {
		t.Fatalf("build lockfile: %v", err)
	}
	churned, err := buildExportLockfile("items", "json", volatileChurn)
	if err != nil {
		t.Fatalf("build churned lockfile: %v", err)
	}
	revised, err := buildExportLockfile("items", "json", contentChange)
	if err != nil {
		t.Fatalf("build revised lockfile: %v", err)
	}

	if baseline.ContentSHA256 != churned.ContentSHA256 {
		t.Errorf("ContentSHA256 changed on version/date churn alone: %s != %s", baseline.ContentSHA256, churned.ContentSHA256)
	}
	if baseline.ContentSHA256 == revised.ContentSHA256 {
		t.Errorf("ContentSHA256 unchanged after a title edit: %s", baseline.ContentSHA256)
	}
	// The per-item version is still recorded, just kept out of the hash: verify
	// reads it to tell a re-fetch apart from a rewrite.
	if churned.Items[1].Version != 9 {
		t.Errorf("item B version = %d, want the churned 9 recorded in the lockfile", churned.Items[1].Version)
	}
}

// An item carrying no content at all has no content hash to record: every one
// of its fields is volatile, so the canonical form is "{}" and would collide
// across distinct items. buildExportLockfile falls back to key and version
// there, which is the ONLY path that lets a version reach the hash.
func TestBuildExportLockfileEmptyItemHashFallsBackToVersion(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"key":"B","version":2}`),
		json.RawMessage(`{"key":"A","version":1}`),
	}
	changedVersionItems := []json.RawMessage{
		json.RawMessage(`{"key":"B","version":3}`),
		json.RawMessage(`{"key":"A","version":1}`),
	}

	lockfile, err := buildExportLockfile("items", "json", items)
	if err != nil {
		t.Fatalf("build lockfile: %v", err)
	}
	if lockfile.Items[0].ContentSHA256 != "" {
		t.Fatalf("content hash = %q, want empty for an item with no content", lockfile.Items[0].ContentSHA256)
	}
	changedVersionLockfile, err := buildExportLockfile("items", "json", changedVersionItems)
	if err != nil {
		t.Fatalf("build changed lockfile: %v", err)
	}
	if lockfile.ContentSHA256 == changedVersionLockfile.ContentSHA256 {
		t.Fatalf("ContentSHA256 did not change after version changed: %s", lockfile.ContentSHA256)
	}
}

func TestBuildExportLockfileRejectsItemWithoutKey(t *testing.T) {
	validItems := []json.RawMessage{json.RawMessage(`{"key":"A","data":{}}`)}
	if _, err := buildExportLockfile("items", "json", validItems); err != nil {
		t.Fatalf("build valid lockfile: %v", err)
	}

	items := append(validItems, json.RawMessage(`{"data":{}}`))
	if _, err := buildExportLockfile("items", "json", items); err == nil {
		t.Fatal("build lockfile without item key succeeded")
	}
}
