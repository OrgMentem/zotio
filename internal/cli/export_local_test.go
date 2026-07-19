// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestExportUsesExplicitLocalDataSource(t *testing.T) {
	seedLocalQueryPlannerDB(t)

	var stdout, stderr bytes.Buffer
	cmd := newExportCmd(&rootFlags{asJSON: true, dataSource: "local"})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"items", "--limit", "2"})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export local items: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("exported lines = %d, want 2: %q", len(lines), stdout.String())
	}
	for _, line := range lines {
		var item struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			t.Fatalf("decode exported item %q: %v", line, err)
		}
		if item.Key == "" {
			t.Fatalf("exported item missing key: %q", line)
		}
	}
}
