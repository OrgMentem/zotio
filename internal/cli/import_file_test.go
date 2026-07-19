// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestImportFileDryRunPrintsPreviewWithoutImporting(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "items.bib")
	if err := os.WriteFile(filePath, []byte("@article{example,\n  title = {Example}\n}\n"), 0o600); err != nil {
		t.Fatalf("write import fixture: %v", err)
	}
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/users/0")

	cmd := newImportFileCmd(&rootFlags{asJSON: true, dryRun: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{filePath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import file dry-run: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode preview %q: %v", out.String(), err)
	}
	if got["dry_run"] != true || got["success"] != false || got["status"] != float64(0) {
		t.Fatalf("preview = %+v, want explicit unsuccessful dry-run", got)
	}
	if got["planned"] != float64(1) {
		t.Fatalf("preview = %+v, want one planned import", got)
	}
	if _, ok := got["imported"]; ok {
		t.Fatalf("preview = %+v, must not claim imported items", got)
	}
}
