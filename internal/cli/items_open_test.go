// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Cover safe Zotero desktop URI opening behavior.

package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"zotio/internal/cliutil"
)

func TestItemsOpenAnnotationsAreNotReadOnly(t *testing.T) {
	cmd := newItemsOpenCmd(&rootFlags{})
	if cmd.Annotations["mcp:read-only"] == "true" {
		t.Fatalf("items open must not be annotated read-only because --launch invokes a desktop handler")
	}
	if cmd.Annotations["mcp:hidden"] == "true" {
		t.Fatalf("items open should remain exposed to MCP by default")
	}
}

func TestItemsOpenDefaultPrintsZoteroURI(t *testing.T) {
	stdout, stderr, err := executeItemsOpen("ABC123")
	if err != nil {
		t.Fatalf("items open returned error: %v", err)
	}
	if stdout != "zotero://select/library/items/ABC123\n" {
		t.Fatalf("stdout: want Zotero URI, got %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr: want empty, got %q", stderr)
	}
}

func TestItemsOpenVerifyEnvLaunchPrintsWouldOpen(t *testing.T) {
	t.Setenv(cliutil.VerifyEnvVar, "1")

	stdout, stderr, err := executeItemsOpen("--launch", "ABC123")
	if err != nil {
		t.Fatalf("items open --launch returned error: %v", err)
	}
	if stdout != "would open: zotero://select/library/items/ABC123\n" {
		t.Fatalf("stdout: want verify dry-run message, got %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("stderr: want empty, got %q", stderr)
	}
}

// PATCH(glean 8r0o): URI construction for every target type x library scope,
// plus path-segment escaping and invalid-type rejection.
func TestZoteroDeepLink(t *testing.T) {
	cases := []struct {
		name, typ, group             string
		key                          string
		wantURI, wantType, wantScope string
	}{
		{"personal_item", "item", "", "ABC123", "zotero://select/library/items/ABC123", "item", "personal"},
		{"default_type_is_item", "", "", "ABC123", "zotero://select/library/items/ABC123", "item", "personal"},
		{"group_item", "item", "123", "ABCD", "zotero://select/groups/123/items/ABCD", "item", "group:123"},
		{"personal_collection", "collection", "", "COLL1", "zotero://select/library/collections/COLL1", "collection", "personal"},
		{"group_collection", "collection", "777", "C9", "zotero://select/groups/777/collections/C9", "collection", "group:777"},
		{"personal_attachment", "attachment", "", "PDF9", "zotero://open-pdf/library/items/PDF9", "attachment", "personal"},
		{"pdf_alias", "pdf", "", "PDF9", "zotero://open-pdf/library/items/PDF9", "attachment", "personal"},
		{"key_path_escaped", "item", "", "A/B?x", "zotero://select/library/items/A%2FB%3Fx", "item", "personal"},
	}
	saved := activeGroupID
	t.Cleanup(func() { activeGroupID = saved })
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			activeGroupID = tc.group
			uri, typ, scope, err := zoteroDeepLink(tc.typ, tc.key)
			if err != nil {
				t.Fatalf("zoteroDeepLink: %v", err)
			}
			if uri != tc.wantURI {
				t.Errorf("uri = %q, want %q", uri, tc.wantURI)
			}
			if typ != tc.wantType {
				t.Errorf("target_type = %q, want %q", typ, tc.wantType)
			}
			if scope != tc.wantScope {
				t.Errorf("library_scope = %q, want %q", scope, tc.wantScope)
			}
		})
	}
}

func TestZoteroDeepLinkRejectsUnknownType(t *testing.T) {
	if _, _, _, err := zoteroDeepLink("bogus", "K"); err == nil {
		t.Fatal("expected error for unknown --type, got nil")
	}
}

// PATCH(glean 8r0o): --agent/--json emits the {uri,target_type,library_scope,launched} envelope.
func TestItemsOpenJSONEnvelope(t *testing.T) {
	stdout, _, err := executeItemsOpenWith(&rootFlags{asJSON: true}, "--type", "collection", "COLL1")
	if err != nil {
		t.Fatalf("items open --agent returned error: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("json output %q: %v", stdout, err)
	}
	if env["uri"] != "zotero://select/library/collections/COLL1" {
		t.Errorf("uri = %v", env["uri"])
	}
	if env["target_type"] != "collection" {
		t.Errorf("target_type = %v", env["target_type"])
	}
	if env["library_scope"] != "personal" {
		t.Errorf("library_scope = %v", env["library_scope"])
	}
	if env["launched"] != false {
		t.Errorf("launched = %v, want false (no --launch)", env["launched"])
	}
}

// PATCH(glean 8r0o): the global --group / activeGroupID scopes the deep link to a group library.
func TestItemsOpenGroupScopeURI(t *testing.T) {
	saved := activeGroupID
	t.Cleanup(func() { activeGroupID = saved })
	activeGroupID = "12345"

	stdout, _, err := executeItemsOpen("ABC123")
	if err != nil {
		t.Fatalf("items open returned error: %v", err)
	}
	if stdout != "zotero://select/groups/12345/items/ABC123\n" {
		t.Fatalf("stdout: want group-scoped URI, got %q", stdout)
	}
}

func executeItemsOpen(args ...string) (string, string, error) {
	return executeItemsOpenWith(&rootFlags{}, args...)
}

func executeItemsOpenWith(flags *rootFlags, args ...string) (string, string, error) {
	cmd := newItemsOpenCmd(flags)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}
