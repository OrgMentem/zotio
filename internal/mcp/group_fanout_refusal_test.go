// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package mcp

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"

	"zotio/internal/cli"
)

// The `--group all` fan-out iterates the CLI's process-global library scope,
// one library at a time, inside a single command execution. On this surface
// that command runs in-process (ADR-0001: command_run and the mirror both go
// through cobratree.runMirroredInProcess), while the native tools registered
// here — search, sql, and the resource handlers — read that same global from
// concurrent dispatch goroutines WITHOUT taking the mirrored-run slot. They
// would therefore serve data-group-99.db to a caller that asked for nothing of
// the sort, purely because a fan-out happened to be mid-iteration.
//
// RegisterTools is the one unconditional call on every transport and both
// surfaces (cmd/zotio-mcp/main.go), so marking the surface there refuses the
// shape process-wide — including for `--group all` arriving as a command
// argument through the facade, which the ZOTERO_GROUP check cannot see, and
// including a `workflow run` step that carries it, which an argv check at the
// facade would also miss.
func TestRegisterToolsRefusesTheGroupFanoutOnThisSurface(t *testing.T) {
	restore := cli.SnapshotGlobals()
	defer restore()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_DATA_DIR", t.TempDir())
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/api/users/0")
	t.Setenv("ZOTERO_GROUP", "")
	t.Setenv("ZOTIO_DEMO", "")

	RegisterTools(server.NewMCPServer("Zotero", "1.0.0", server.WithToolCapabilities(false)))

	run := func(args ...string) (string, error) {
		root := cli.RootCmd()
		root.SilenceErrors, root.SilenceUsage = true, true
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		return out.String(), root.ExecuteContext(context.Background())
	}

	out, err := run("collections", "list", "--group", "all", "--json")
	if err == nil {
		t.Fatalf("collections list --group all = nil error under the MCP surface, want a refusal; out=%s", out)
	}
	if code := cli.ExitCode(err); code != 2 {
		t.Fatalf("exit code = %d, want 2 (usage); err=%v", code, err)
	}
	for _, want := range []string{"CLI-only", "--group <id>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err.Error(), want)
		}
	}
	// The refusal is specific to the fan-out shape: a numeric group still
	// establishes one scope for one command, which is the exposure this server
	// already accepts and serializes with StateGuard plus the mirrored slot.
	// The command may still fail for its own reasons here (this environment has
	// no synced mirror); what it must not hit is the fan-out refusal.
	if _, numericErr := run("collections", "list", "--group", "12345", "--json", "--data-source", "local"); numericErr != nil && strings.Contains(numericErr.Error(), "CLI-only") {
		t.Fatalf("--group 12345 under the MCP surface = %v, want the fan-out refusal not to apply", numericErr)
	}
}
