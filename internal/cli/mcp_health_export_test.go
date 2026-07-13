// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"testing"
)

func TestHealthJSONNoStore(t *testing.T) {
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })
	t.Setenv("HOME", t.TempDir())

	data, err := HealthJSON(context.Background(), "")
	if err != nil {
		t.Fatalf("HealthJSON returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("HealthJSON returned invalid JSON: %v", err)
	}
	if got["synced"] != false {
		t.Fatalf("synced = %v, want false", got["synced"])
	}
	if got["note"] != "local store not synced; run sync" {
		t.Fatalf("note = %v, want local store not synced note", got["note"])
	}
}
