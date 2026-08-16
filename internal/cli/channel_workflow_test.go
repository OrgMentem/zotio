// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowStatus_NonexistentDB_ReportsNoData_Human(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing.db")
	cmd := newWorkflowStatusCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--db", dbPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("workflow status error = %v; want nil (no-data branch)", err)
	}
	if got := out.String(); !strings.Contains(got, "No archived data") {
		t.Fatalf("stdout = %q; want containing 'No archived data'", got)
	}
	if strings.Contains(errOut.String(), "opening store") {
		t.Fatalf("stderr = %q; must not contain store-open error on fresh install", errOut.String())
	}
}

func TestWorkflowStatus_NonexistentDB_ReportsNoData_JSON(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing.db")
	cmd := newWorkflowStatusCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--db", dbPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("workflow status --json error = %v; want nil", err)
	}
	var status map[string]int
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &status); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if len(status) != 0 {
		t.Fatalf("status = %v; want empty map for no-data", status)
	}
	if strings.Contains(errOut.String(), "opening store") {
		t.Fatalf("stderr = %q; must not contain store-open error", errOut.String())
	}
}
