// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

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
	if env.Plan.Summary.Planned != 2 || env.Plan.Summary.NoOp != 1 || len(env.Plan.Operations) != 3 {
		t.Fatalf("plan = %+v, ops=%+v; want two planned ops and one duplicate no-op", env.Plan.Summary, env.Plan.Operations)
	}
	if env.Plan.Operations[0].Kind != "import_create" || env.Plan.Operations[1].Kind != "import_attach" || env.Plan.Operations[2].Kind != "import_duplicate" {
		t.Fatalf("operations = %+v; want create, attach, duplicate", env.Plan.Operations)
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
	if env.Plan.Summary.Planned != 1 || env.Plan.Summary.NoOp != 2 || len(env.Plan.Operations) != 3 {
		t.Fatalf("plan = %+v, ops=%+v; want create planned plus attach and duplicate no-ops", env.Plan.Summary, env.Plan.Operations)
	}
	attach, duplicate := env.Plan.Operations[1], env.Plan.Operations[2]
	if attach.Kind != "import_attach" || attach.Key != "MATCH1" || len(attach.Changes) != 0 ||
		duplicate.Kind != "import_duplicate" || duplicate.Key != "DUP1" || len(duplicate.Changes) != 0 {
		t.Fatalf("attach=%+v duplicate=%+v, want explicit no-ops", attach, duplicate)
	}
}

func TestImportApplyDuplicateIsAuditableNoOp(t *testing.T) {
	manifest := importApplyTestManifest()
	manifest.Entries = manifest.Entries[2:]
	manifestPath := writeImportApplyTestManifest(t, manifest)
	env, stderr, err := runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true, via: "web", yes: true}, []string{manifestPath})
	if err != nil {
		t.Fatalf("duplicate apply: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.NoOp != 1 || env.Result.Summary.Applied != 0 {
		t.Fatalf("env = %+v, want one applied duplicate no-op", env)
	}
	if len(env.Result.Items) != 1 || env.Result.Items[0].Key != "DUP1" || env.Result.Items[0].Status != "no_op" {
		t.Fatalf("items = %+v, want duplicate parent no-op", env.Result.Items)
	}
}

func TestImportApplyStoredPreviewSupportsWebCreates(t *testing.T) {
	manifestPath := writeImportApplyTestManifest(t, importApplyTestManifest())
	env, stderr, err := runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true, via: "web"}, []string{"--attach-mode", "stored", manifestPath})
	if err != nil {
		t.Fatalf("stored Web preview: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "preview" || env.Plan.Summary.Planned != 2 {
		t.Fatalf("env = %+v, want two-operation stored Web preview", env)
	}
}

// A stored manifest with only attach entries needs the Web API, not the connector.
func TestImportApplyStoredAttachOnlyManifestSkipsConnector(t *testing.T) {
	manifest := importApplyTestManifest()
	entries := manifest.Entries[:0]
	for _, entry := range manifest.Entries {
		if entry.Action == "attach" {
			entries = append(entries, entry)
		}
	}
	manifest.Entries = entries
	manifestPath := writeImportApplyTestManifest(t, manifest)
	env, stderr, err := runImportApplyTestCmdWithFlags(t, &rootFlags{asJSON: true, via: "web"}, []string{"--attach-mode", "stored", manifestPath})
	if err != nil {
		t.Fatalf("stored attach-only preview: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "preview" || len(env.Plan.Operations) != 1 || env.Plan.Operations[0].Kind != "import_attach" {
		t.Fatalf("env = %+v ops=%+v, want one previewed import_attach op", env, env.Plan.Operations)
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

func TestImportApplyStoredPreviewPlansConnectorCreateAndWebAttach(t *testing.T) {
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
	if env.Plan.Summary.Planned != 2 || env.Plan.Summary.NoOp != 1 || len(env.Plan.Operations) != 3 {
		t.Fatalf("plan = %+v, ops=%+v; want connector create, web-upload attach, and duplicate no-op", env.Plan.Summary, env.Plan.Operations)
	}
}

func TestImportApplyStoredWebCreateAppliesParentAndAttachment(t *testing.T) {
	fake := newFakeZoteroUpload(t, "")
	pdf := writeUploadFixture(t, "created-paper.pdf", []byte("%PDF-1.4\ncreated\n%%EOF"))
	manifest := importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           filepath.Dir(pdf),
		Entries: []importManifestEntry{{
			Path: pdf, Action: "create", Status: "resolved", Title: "Created Paper",
			Item: map[string]any{"itemType": "journalArticle", "title": "Created Paper", "DOI": "10.1002/example"},
		}},
	}
	manifestPath := writeImportApplyTestManifest(t, manifest)
	flags := &rootFlags{
		asJSON: true, yes: true, via: "web", maxChanges: -1,
		configPath: testConfigFile(t, fake.srv.URL+"/users/0"),
	}
	env, stderr, err := runImportApplyTestCmdWithFlags(t, flags, []string{"--attach-mode", "stored", manifestPath})
	if err != nil {
		t.Fatalf("stored Web create apply: %v; stderr=%s", err, stderr)
	}
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Summary.Failed != 0 {
		t.Fatalf("env = %+v, want one applied create+attachment operation", env)
	}
	reason, ok := env.Result.Items[0].Reason.(map[string]any)
	if !ok || reason["parent_key"] != "PARENT1" || reason["attachment_key"] != "ATT1" {
		t.Fatalf("reason = %#v, want returned parent and attachment keys", env.Result.Items[0].Reason)
	}
	creates, uploads, registers := fake.snapshot()
	if fake.parentSnapshot() != 1 || creates != 1 || uploads != 1 || registers != 1 {
		t.Fatalf("traffic parent=%d attachment=%d upload=%d register=%d, want 1 each", fake.parentSnapshot(), creates, uploads, registers)
	}
}

func TestImportApplyLinkedFileWebCreateReturnsParentAndAttachmentKeys(t *testing.T) {
	fake := newFakeZoteroUpload(t, "")
	pdf := writeUploadFixture(t, "linked-paper.pdf", []byte("%PDF-1.4\nlinked\n%%EOF"))
	manifest := importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           filepath.Dir(pdf),
		Entries: []importManifestEntry{{
			Path: pdf, Action: "create", Status: "resolved", Title: "Linked Paper",
			Item: map[string]any{"itemType": "journalArticle", "title": "Linked Paper", "DOI": "10.1002/linked"},
		}},
	}
	manifestPath := writeImportApplyTestManifest(t, manifest)
	flags := &rootFlags{
		asJSON: true, yes: true, via: "web", maxChanges: -1,
		configPath: testConfigFile(t, fake.srv.URL+"/users/0"),
	}
	env, stderr, err := runImportApplyTestCmdWithFlags(t, flags, []string{"--attach-mode", "linked-file", manifestPath})
	if err != nil {
		t.Fatalf("linked-file Web create apply: %v; result=%+v; stderr=%s", err, env.Result, stderr)
	}
	if !env.OK || env.Mode != "apply" || env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Summary.Failed != 0 {
		t.Fatalf("env = %+v, want one applied create+attachment operation", env)
	}
	reason, ok := env.Result.Items[0].Reason.(map[string]any)
	if !ok || reason["parent_key"] != "PARENT1" || reason["attachment_key"] != "ATT1" {
		t.Fatalf("reason = %#v, want returned parent and attachment keys", env.Result.Items[0].Reason)
	}
	creates, uploads, registers := fake.snapshot()
	if fake.parentSnapshot() != 1 || creates != 1 || uploads != 0 || registers != 0 {
		t.Fatalf("traffic parent=%d attachment=%d upload=%d register=%d, want parent=1 attachment=1 upload=0 register=0",
			fake.parentSnapshot(), creates, uploads, registers)
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
