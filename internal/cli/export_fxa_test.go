package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func fxaRunExport(t *testing.T, flags *rootFlags, args ...string) exportFileResult {
	t.Helper()
	cmd := newExportCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs(args)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("export %v: %v", args, err)
	}
	var res exportFileResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("decode export result %q: %v", out.String(), err)
	}
	return res
}

// TestFxaExportReportsRecordCountForEveryFormat pins that the file result
// counts the records written for --format json and for a single object
// exported by key, and that --dry-run reports the same count.
func TestFxaExportReportsRecordCountForEveryFormat(t *testing.T) {
	seedLocalQueryPlannerDB(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "items.json")

	preview := fxaRunExport(t, &rootFlags{asJSON: true, dataSource: "local", dryRun: true},
		"items", "--format", "json", "--limit", "2", "--output", target)
	res := fxaRunExport(t, &rootFlags{asJSON: true, dataSource: "local"},
		"items", "--format", "json", "--limit", "2", "--output", target)

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading export: %v", err)
	}
	var items []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatalf("decode json export %q: %v", data, err)
	}
	if len(items) == 0 || items[0].Key == "" {
		t.Fatalf("json export has no keyed items: %q", data)
	}
	if res.Records != len(items) {
		t.Errorf("json export Records = %d, want %d written", res.Records, len(items))
	}
	if preview.Records != res.Records {
		t.Errorf("json dry-run Records = %d, want %d like the real run", preview.Records, res.Records)
	}

	for _, format := range []string{"json", "jsonl"} {
		one := filepath.Join(dir, "one."+format)
		byKey := fxaRunExport(t, &rootFlags{asJSON: true, dataSource: "local"},
			"items", items[0].Key, "--format", format, "--output", one)
		if byKey.Records != 1 {
			t.Errorf("%s export by key Records = %d, want 1", format, byKey.Records)
		}
	}
}
