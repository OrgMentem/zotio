// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// pin the unsynced MCP payload so
// agents can distinguish missing local state from command failure.

package cli

import (
	"encoding/json"
	"testing"
)

func TestFreshnessJSONUnsynced(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	got, err := FreshnessJSON()
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
