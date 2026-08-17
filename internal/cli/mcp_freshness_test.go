// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// pin the unsynced MCP payload so
// agents can distinguish missing local state from command failure.

package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"zotio/internal/store"
)

func TestFreshnessJSONUnsynced(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	got, err := FreshnessJSON(context.Background())
	if err != nil {
		t.Fatalf("FreshnessJSON: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("unmarshal FreshnessJSON: %v", err)
	}
	if payload["synced"] != false {
		t.Fatalf("synced = %v, want false", payload["synced"])
	}
	if payload["note"] != "local store not synced; run sync" {
		t.Fatalf("note = %v, want local store guidance", payload["note"])
	}
}

func TestFreshnessJSONSurfacesSyncStateError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	// Create store and poison sync_state so GetSyncState returns a real error.
	s, err := store.OpenWithContext(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := s.DB().ExecContext(context.Background(),
		`INSERT INTO sync_state (resource_type, last_synced_at) VALUES ('items', 'not-a-timestamp')
		 ON CONFLICT(resource_type) DO UPDATE SET last_synced_at = excluded.last_synced_at`,
	); err != nil {
		s.Close()
		t.Fatalf("poison sync_state: %v", err)
	}
	s.Close()

	got, err := FreshnessJSON(context.Background())
	if err != nil {
		t.Fatalf("FreshnessJSON: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, string(got))
	}
	resources, _ := payload["resources"].([]any)
	foundErr := false
	for _, r := range resources {
		m, _ := r.(map[string]any)
		if _, has := m["error"]; has {
			foundErr = true
		}
		if m["resource"] == "items" {
			if age, _ := m["age"].(string); age == "never" && m["error"] == nil {
				t.Fatalf("items resource masked error as 'never': %+v", m)
			}
		}
	}
	if !foundErr {
		t.Fatalf("expected at least one resource with error, got: %s", string(got))
	}
}
