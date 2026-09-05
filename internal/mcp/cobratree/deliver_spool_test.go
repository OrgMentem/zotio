// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Cover --deliver resource ownership on the in-process mirrored path.

package cobratree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/cli"
)

// The mirror forwards arbitrary arguments, so a mirrored call can carry the
// global --deliver. zotio-mcp is a persistent server: every such call used to
// leave one open descriptor and one temp file behind for the lifetime of the
// process, and never delivered the captured output to the requested sink.
func TestRunMirroredInProcessDeliversAndRemovesDeliverSpool(t *testing.T) {
	target := filepath.Join(t.TempDir(), "mirrored.json")

	res := runMirroredInProcess(context.Background(), cli.RootCmd, []string{"which"}, map[string]any{
		"args": "citation --deliver file:" + target,
	})
	if res.IsError {
		t.Fatalf("mirrored command failed: %q", toolResultText(t, res))
	}

	delivered, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("deliver sink: %v", err)
	}
	if len(delivered) == 0 {
		t.Fatal("deliver sink is empty: the captured output never reached it")
	}
	// The spool copies the mirrored output; it must not divert it, or the tool
	// result would come back blank and the bytes would land on the server's own
	// stdout, which under the stdio transport is the JSON-RPC stream.
	if got := toolResultText(t, res); !strings.Contains(got, strings.TrimSpace(string(delivered))) {
		t.Fatalf("tool result = %q, want it to carry the delivered output %q", got, delivered)
	}
	assertNoMirroredDeliverTemp(t, target)
}

// A failing command is when a leak is most likely, and it is the case a
// PersistentPostRunE hook would miss: Cobra runs no post-run hook once the
// execution fails. --data-source is validated just after the spool is opened.
func TestRunMirroredInProcessRemovesDeliverSpoolWhenCommandFails(t *testing.T) {
	target := filepath.Join(t.TempDir(), "mirrored.json")

	res := runMirroredInProcess(context.Background(), cli.RootCmd, []string{"which"}, map[string]any{
		"args": "citation --deliver file:" + target + " --data-source bogus",
	})
	if !res.IsError {
		t.Fatalf("mirrored command succeeded: %q", toolResultText(t, res))
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target exists after a failed command: %v", err)
	}
	assertNoMirroredDeliverTemp(t, target)
}

// assertNoMirroredDeliverTemp counts the spool temp files a file sink creates
// beside its target. Asserting on the files themselves, rather than on a log
// line, is what makes this a leak test.
func assertNoMirroredDeliverTemp(t *testing.T, target string) {
	t.Helper()

	temps, err := filepath.Glob(filepath.Join(
		filepath.Dir(target),
		"."+filepath.Base(target)+"-*.tmp",
	))
	if err != nil {
		t.Fatalf("glob deliver temps: %v", err)
	}
	if len(temps) != 0 {
		t.Fatalf("leftover deliver temps: %q", temps)
	}
}
