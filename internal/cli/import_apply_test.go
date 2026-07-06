// Copyright 2026 OrgMentem. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase4 import-apply): preview-only tests for applying reviewed import manifests.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/connector"
	"zotio/internal/mutation"
)

func TestImportApplyPreviewPlansCreateAndLinkedAttach(t *testing.T) {
	manifestPath := writeImportApplyTestManifest(t, importApplyTestManifest())
	env, stderr, err := runImportApplyTestCmd(t, []string{"--attach-mode", "linked-file", manifestPath})
	if err != nil {
		t.Fatalf("import apply: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "preview" || env.Result != nil {
		t.Fatalf("env = %+v, want successful preview", env)
	}
	if env.Plan.Summary.Planned != 2 || env.Plan.Summary.NoOp != 0 || len(env.Plan.Operations) != 2 {
		t.Fatalf("plan = %+v, ops=%+v; want two planned ops", env.Plan.Summary, env.Plan.Operations)
	}
	if env.Plan.Operations[0].Kind != "import_create" || env.Plan.Operations[1].Kind != "import_attach" {
		t.Fatalf("kinds = %q, %q; want import_create, import_attach", env.Plan.Operations[0].Kind, env.Plan.Operations[1].Kind)
	}
}

func TestImportApplyDefaultAttachModeNoneMakesAttachNoOp(t *testing.T) {
	manifestPath := writeImportApplyTestManifest(t, importApplyTestManifest())
	env, stderr, err := runImportApplyTestCmd(t, []string{manifestPath})
	if err != nil {
		t.Fatalf("import apply: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "preview" || env.Result != nil {
		t.Fatalf("env = %+v, want successful preview", env)
	}
	if env.Plan.Summary.Planned != 1 || env.Plan.Summary.NoOp != 1 || len(env.Plan.Operations) != 2 {
		t.Fatalf("plan = %+v, ops=%+v; want create planned and attach no-op", env.Plan.Summary, env.Plan.Operations)
	}
	attach := env.Plan.Operations[1]
	if attach.Kind != "import_attach" || attach.Key != "MATCH1" || len(attach.Changes) != 0 {
		t.Fatalf("attach op = %+v, want empty-change import_attach no-op", attach)
	}
}

func TestImportApplyStoredPreviewRequiresConnector(t *testing.T) {
	manifestPath := writeImportApplyTestManifest(t, importApplyTestManifest())
	_, _, err := runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true, via: "web"}, []string{"--attach-mode", "stored", manifestPath})
	if err == nil || !strings.Contains(err.Error(), "--attach-mode stored requires the desktop connector") {
		t.Fatalf("err = %v, want stored connector precondition error", err)
	}
}
func TestManifestUnidentifiedDefaultsToRecognize(t *testing.T) {
	if got := manifestActionForStatus("unidentified"); got != "recognize" {
		t.Fatalf("manifestActionForStatus(unidentified) = %q, want recognize", got)
	}
}

func TestImportApplyRecognizeRequiresConnector(t *testing.T) {
	manifest := importApplyTestManifest()
	manifest.Entries = []importManifestEntry{{
		Path:   filepath.Join(string(filepath.Separator), "tmp", "unknown.pdf"),
		Action: "recognize",
		Status: "resolved",
		Title:  "Unknown PDF",
	}}
	manifestPath := writeImportApplyTestManifest(t, manifest)
	_, _, err := runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true, via: "web"}, []string{manifestPath})
	if err == nil || !strings.Contains(err.Error(), "action recognize requires the desktop connector") {
		t.Fatalf("err = %v, want recognize connector precondition error", err)
	}
}

func TestImportApplyRecognizePlansImportPDF(t *testing.T) {
	oldPing := connectorPing
	defer func() { connectorPing = oldPing }()
	connectorPing = func(ctx context.Context, c *connector.Client) error { return nil }

	manifest := importApplyTestManifest()
	manifest.Entries = []importManifestEntry{{
		Path:   filepath.Join(string(filepath.Separator), "tmp", "unknown.pdf"),
		Action: "recognize",
		Status: "resolved",
		Title:  "Unknown PDF",
	}}
	manifestPath := writeImportApplyTestManifest(t, manifest)
	flags := &rootFlags{asJSON: true, via: "connector", configPath: testConfigFile(t, "http://localhost:23119/api/users/0")}
	env, stderr, err := runImportApplyTestCmdWithFlags(t, flags, []string{manifestPath})
	if err != nil {
		t.Fatalf("import apply recognize preview: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Plan.Summary.Planned != 1 || len(env.Plan.Operations) != 1 || env.Plan.Operations[0].Kind != "import_pdf" {
		t.Fatalf("env = %+v ops=%+v, want one import_pdf preview op", env, env.Plan.Operations)
	}
}

func TestImportApplyStoredPreviewPlansConnectorCreateAndExistingAttachFailure(t *testing.T) {
	oldPing := connectorPing
	defer func() { connectorPing = oldPing }()
	connectorPing = func(ctx context.Context, c *connector.Client) error { return nil }

	manifestPath := writeImportApplyTestManifest(t, importApplyTestManifest())
	flags := &rootFlags{asJSON: true, via: "connector", configPath: testConfigFile(t, "http://localhost:23119/api/users/0")}
	env, stderr, err := runImportApplyTestCmdWithFlags(t, flags, []string{"--attach-mode", "stored", manifestPath})
	if err != nil {
		t.Fatalf("import apply stored preview: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "preview" || env.Result != nil {
		t.Fatalf("env = %+v, want successful stored preview", env)
	}
	if env.Plan.Summary.Planned != 2 || len(env.Plan.Operations) != 2 {
		t.Fatalf("plan = %+v, ops=%+v; want create plus planned attach failure", env.Plan.Summary, env.Plan.Operations)
	}
}

func TestImportApplyUploadModeIsInvalid(t *testing.T) {
	manifestPath := writeImportApplyTestManifest(t, importApplyTestManifest())
	_, _, err := runImportApplyTestCmd(t, []string{"--attach-mode", "upload", manifestPath})
	if err == nil || !strings.Contains(err.Error(), "--attach-mode must be one of none, linked-file, stored") {
		t.Fatalf("err = %v, want upload invalid error", err)
	}
}

func TestLinkedFileAttachmentItem(t *testing.T) {
	path := filepath.Join(string(filepath.Separator), "tmp", "Paper.pdf")
	item := linkedFileAttachmentItem("PARENT1", path)
	want := map[string]any{
		"itemType":    "attachment",
		"linkMode":    "linked_file",
		"parentItem":  "PARENT1",
		"title":       "Paper.pdf",
		"path":        path,
		"contentType": "application/pdf",
	}
	if len(item) != len(want) {
		t.Fatalf("item = %+v, want %+v", item, want)
	}
	for key, wantValue := range want {
		if item[key] != wantValue {
			t.Fatalf("item[%s] = %v, want %v; item=%+v", key, item[key], wantValue, item)
		}
	}
}

func TestCreatedItemKey(t *testing.T) {
	if key, ok := createdItemKey(json.RawMessage(`{"success":{"0":"KEY1"}}`)); !ok || key != "KEY1" {
		t.Fatalf("success key = %q, %v; want KEY1, true", key, ok)
	}
	if key, ok := createdItemKey(json.RawMessage(`{"successful":{"0":{"key":"KEY2"}}}`)); !ok || key != "KEY2" {
		t.Fatalf("successful key = %q, %v; want KEY2, true", key, ok)
	}
	if key, ok := createdItemKey(json.RawMessage(`{"failed":{"0":{"code":400}}}`)); ok || key != "" {
		t.Fatalf("failed key = %q, %v; want empty, false", key, ok)
	}
}

func importApplyTestManifest() importManifest {
	return importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           filepath.Join(string(filepath.Separator), "tmp"),
		Entries: []importManifestEntry{
			{
				Path:   filepath.Join(string(filepath.Separator), "tmp", "create.pdf"),
				Action: "create",
				Status: "resolved",
				Title:  "Created Paper",
				Item: map[string]any{
					"itemType": "journalArticle",
					"title":    "Created Paper",
				},
			},
			{
				Path:       filepath.Join(string(filepath.Separator), "tmp", "attach.pdf"),
				Action:     "attach",
				Status:     "resolved",
				MatchedKey: "MATCH1",
				Title:      "Attach Paper",
			},
			{
				Path:           filepath.Join(string(filepath.Separator), "tmp", "duplicate.pdf"),
				Classification: "duplicate",
				Action:         "skip",
				Status:         "resolved",
				MatchedKey:     "DUP1",
				Title:          "Duplicate Paper",
			},
		},
	}
}

func writeImportApplyTestManifest(t *testing.T, m importManifest) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create manifest: %v", err)
	}
	if err := writeImportManifest(f, m); err != nil {
		_ = f.Close()
		t.Fatalf("write manifest: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close manifest: %v", err)
	}
	return path
}

func runImportApplyTestCmd(t *testing.T, args []string) (mutation.Envelope, string, error) {
	t.Helper()
	return runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true}, args)
}

func runImportApplyTestCmdWithFlags(t *testing.T, flags *rootFlags, args []string) (mutation.Envelope, string, error) {
	t.Helper()
	cmd := newImportApplyCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	var env mutation.Envelope
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode mutation envelope %q: %v", out.String(), decodeErr)
		}
	}
	return env, errOut.String(), err
}
