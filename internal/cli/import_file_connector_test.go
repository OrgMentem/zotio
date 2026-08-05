// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Connector-backed import file routing must preserve dry-run safety.

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

// The connector preview must stay offline: no ping, no session, no translator
// call until the run is applied.
func TestImportFileConnectorPreviewDoesNotWrite(t *testing.T) {
	oldPing := connectorPing
	defer func() { connectorPing = oldPing }()
	pinged := false
	connectorPing = func(ctx context.Context, c *connector.Client) error {
		pinged = true
		return nil
	}

	for _, tc := range []struct {
		name       string
		flags      rootFlags
		wantReason string
	}{
		{name: "bare", flags: rootFlags{asJSON: true, via: "connector", maxChanges: -1}, wantReason: "default"},
		{name: "agent", flags: rootFlags{asJSON: true, agent: true, via: "connector", maxChanges: -1}, wantReason: "default"},
		{name: "dry-run", flags: rootFlags{asJSON: true, via: "connector", dryRun: true, maxChanges: -1}, wantReason: "dry_run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinged = false
			path := filepath.Join(t.TempDir(), "refs.ris")
			if err := os.WriteFile(path, []byte("TY  - JOUR\nTI  - Dry Run\nER  - \n"), 0o600); err != nil {
				t.Fatalf("write RIS: %v", err)
			}
			flags := tc.flags
			flags.configPath = testConfigFile(t, "http://localhost:23119/api/users/0")
			cmd := newImportFileCmd(&flags)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			cmd.SetArgs([]string{path})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("import file connector preview: %v", err)
			}
			if pinged {
				t.Fatal("connector preview pinged the desktop; it must stay offline")
			}
			assertConnectorPreview(t, out.Bytes(), tc.wantReason)
		})
	}
}

func TestImportFileCSLJSONConnectorPreviewUsesConnectorPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "refs.json")
	if err := os.WriteFile(path, []byte(`[{"type":"article-journal","title":"Dry Run"}]`), 0o600); err != nil {
		t.Fatalf("write CSL JSON: %v", err)
	}

	flags := &rootFlags{asJSON: true, via: "connector", configPath: testConfigFile(t, "http://localhost:23119/api/users/0"), dryRun: true, maxChanges: -1}
	cmd := newImportFileCmd(flags)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--format", "csljson", path})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("CSL JSON connector preview: %v", err)
	}
	assertConnectorPreview(t, out.Bytes(), "dry_run")
}

func assertConnectorPreview(t *testing.T, raw []byte, wantReason string) {
	t.Helper()
	var env struct {
		Mode          string `json:"mode"`
		PreviewReason string `json:"preview_reason"`
		Result        *any   `json:"result"`
		Plan          struct {
			Summary struct {
				Planned int `json:"planned"`
			} `json:"summary"`
			Operations []struct {
				Changes []struct {
					Add map[string]any `json:"add"`
				} `json:"changes"`
			} `json:"operations"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode output: %v; %s", err, raw)
	}
	if env.Mode != "preview" || env.PreviewReason != wantReason || env.Result != nil {
		t.Fatalf("envelope = %s, want preview/%s with no result", raw, wantReason)
	}
	if env.Plan.Summary.Planned != 1 {
		t.Fatalf("planned = %d, want the single counted record (%s)", env.Plan.Summary.Planned, raw)
	}
	if len(env.Plan.Operations) != 1 || env.Plan.Operations[0].Changes[0].Add["via"] != "connector" {
		t.Fatalf("operations = %s, want one connector-routed record", raw)
	}
}
