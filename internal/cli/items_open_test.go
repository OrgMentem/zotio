// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Cover safe Zotero desktop URI opening behavior.

package cli

import (
	"bytes"
	"testing"

	"zotero-pp-cli/internal/cliutil"
)

func TestItemsOpenAnnotationsAreNotReadOnly(t *testing.T) {
	cmd := newItemsOpenCmd(&rootFlags{})
	if cmd.Annotations["mcp:read-only"] == "true" {
		t.Fatalf("items open must not be annotated read-only because --launch invokes macOS open")
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

func executeItemsOpen(args ...string) (string, string, error) {
	cmd := newItemsOpenCmd(&rootFlags{})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}
