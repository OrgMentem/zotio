// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH: connector-backed import file routing must preserve dry-run safety.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"zotio/internal/connector"
)

func TestImportFileConnectorDryRunDoesNotWrite(t *testing.T) {
	oldPing := connectorPing
	defer func() { connectorPing = oldPing }()
	connectorPing = func(ctx context.Context, c *connector.Client) error { return nil }

	path := filepath.Join(t.TempDir(), "refs.ris")
	if err := os.WriteFile(path, []byte("TY  - JOUR\nTI  - Dry Run\nER  - \n"), 0o600); err != nil {
		t.Fatalf("write RIS: %v", err)
	}
	flags := &rootFlags{asJSON: true, via: "connector", configPath: testConfigFile(t, "http://localhost:23119/api/users/0"), dryRun: true}
	cmd := newImportFileCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{path})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import file dry-run: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode output: %v; %s", err, out.String())
	}
	if got["dry_run"] != true || got["via"] != "connector" {
		t.Fatalf("output = %+v, want connector dry-run preview", got)
	}
}
