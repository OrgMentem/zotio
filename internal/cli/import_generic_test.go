// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// importGenericEnvelope is the parent `import <resource>` command's stable
// {"succeeded","failed","skipped"} output contract.
type importGenericEnvelope struct {
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

func writeImportGenericFixture(t *testing.T, content string) string {
	t.Helper()
	filePath := filepath.Join(t.TempDir(), "records.jsonl")
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("write import fixture: %v", err)
	}
	return filePath
}

// runImportGeneric executes `import <resource>` with the given flags/args and
// decodes stdout as the command's own envelope (not the shared mutation one:
// this command keeps its pre-existing succeeded/failed/skipped contract).
func runImportGeneric(t *testing.T, flags *rootFlags, args ...string) (importGenericEnvelope, string, string, error) {
	t.Helper()
	cmd := newImportCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()

	var env importGenericEnvelope
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode envelope %q: %v", out.String(), decodeErr)
		}
	}
	return env, out.String(), errOut.String(), err
}

func recordingImportServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	return srv, &requests
}

// The write gate is the point of this command: without --yes nothing is
// posted, including under --agent, which only changes formatting.
func TestGenericImportPreviewsWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags rootFlags
	}{
		{name: "bare", flags: rootFlags{asJSON: true, maxChanges: -1}},
		{name: "agent", flags: rootFlags{asJSON: true, agent: true, maxChanges: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, requests := recordingImportServer(t)

			filePath := writeImportGenericFixture(t, "{\"title\":\"A\"}\n{\"title\":\"B\"}\n")
			flags := tc.flags
			env, raw, _, err := runImportGeneric(t, &flags, "items", "--input", filePath)
			if err != nil {
				t.Fatalf("preview returned error: %v (%s)", err, raw)
			}
			if *requests != 0 {
				t.Fatalf("preview issued %d HTTP request(s); want none", *requests)
			}
			// Nothing was applied yet, so the preview reports zero successes
			// even though two records were parsed and would be posted.
			if env.Succeeded != 0 || env.Failed != 0 || env.Skipped != 0 {
				t.Fatalf("envelope = %+v, want a zero-write preview (%s)", env, raw)
			}
		})
	}
}

// --yes posts exactly one request per parsed record.
func TestGenericImportAppliesUnderYes(t *testing.T) {
	_, requests := recordingImportServer(t)

	filePath := writeImportGenericFixture(t, "{\"title\":\"A\"}\n{\"title\":\"B\"}\n")
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
	env, raw, _, err := runImportGeneric(t, flags, "items", "--input", filePath)
	if err != nil {
		t.Fatalf("apply returned error: %v (%s)", err, raw)
	}
	if *requests != 2 {
		t.Fatalf("posted %d request(s), want exactly 1 per record (2)", *requests)
	}
	if env.Succeeded != 2 || env.Failed != 0 {
		t.Fatalf("envelope = %+v, want 2 succeeded, 0 failed (%s)", env, raw)
	}
}

// --dry-run always wins over --yes: it must behave exactly like a bare preview.
func TestGenericImportDryRunBeatsYes(t *testing.T) {
	_, requests := recordingImportServer(t)

	filePath := writeImportGenericFixture(t, "{\"title\":\"A\"}\n")
	flags := &rootFlags{asJSON: true, yes: true, dryRun: true, maxChanges: -1}
	env, raw, _, err := runImportGeneric(t, flags, "items", "--input", filePath)
	if err != nil {
		t.Fatalf("dry-run returned error: %v (%s)", err, raw)
	}
	if *requests != 0 {
		t.Fatalf("--dry-run --yes issued %d HTTP request(s); want none", *requests)
	}
	if env.Succeeded != 0 {
		t.Fatalf("envelope = %+v, want zero successes under --dry-run", env)
	}
}

// Each parsed record charges one op against --max-changes; the whole run must
// be refused before any record reaches the network.
func TestGenericImportChargesEachRecordAgainstMaxChanges(t *testing.T) {
	_, requests := recordingImportServer(t)

	filePath := writeImportGenericFixture(t, "{\"title\":\"A\"}\n{\"title\":\"B\"}\n{\"title\":\"C\"}\n")
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: 2}
	_, raw, _, err := runImportGeneric(t, flags, "items", "--input", filePath)
	if err == nil {
		t.Fatalf("expected a max-changes refusal, got none (%s)", raw)
	}
	if *requests != 0 {
		t.Fatalf("posted %d request(s) before the gate refused the run; want none", *requests)
	}
}

// Blank and #-comment lines are counted as skipped and never become an op, so
// they are never posted.
func TestGenericImportSkipsBlankAndCommentLines(t *testing.T) {
	_, requests := recordingImportServer(t)

	content := "\n# a comment\n{\"title\":\"A\"}\n   \n# another comment\n"
	filePath := writeImportGenericFixture(t, content)
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
	env, raw, _, err := runImportGeneric(t, flags, "items", "--input", filePath)
	if err != nil {
		t.Fatalf("apply returned error: %v (%s)", err, raw)
	}
	if *requests != 1 {
		t.Fatalf("posted %d request(s), want exactly 1 (blank/comment lines must never post)", *requests)
	}
	if env.Succeeded != 1 {
		t.Fatalf("succeeded = %d, want 1 (%s)", env.Succeeded, raw)
	}
	if env.Skipped != 4 {
		t.Fatalf("skipped = %d, want 4 (two blank lines, two comment lines) (%s)", env.Skipped, raw)
	}
	if env.Failed != 0 {
		t.Fatalf("failed = %d, want 0 (%s)", env.Failed, raw)
	}
}

// --input - must read from the command's own reader, never the process's real
// stdin: a stdio MCP server pipes the JSON-RPC transport over os.Stdin, so
// reading it directly would corrupt the session.
func TestGenericImportReadsStdinFromCommandReader(t *testing.T) {
	_, requests := recordingImportServer(t)

	// Point the real process stdin at an already-closed pipe. If the command
	// fell back to os.Stdin it would read zero records (immediate EOF)
	// instead of the one record supplied through cmd.SetIn below.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = pr
	t.Cleanup(func() {
		os.Stdin = origStdin
		pr.Close()
	})

	cmd := newImportCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetIn(strings.NewReader("{\"title\":\"From cmd.SetIn\"}\n"))
	cmd.SetArgs([]string{"items", "--input", "-"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import from stdin: %v (%s)", err, out.String())
	}

	if *requests != 1 {
		t.Fatalf("posted %d request(s), want 1: command must read cmd.InOrStdin(), not the process stdin", *requests)
	}
	var env importGenericEnvelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", out.String(), err)
	}
	if env.Succeeded != 1 {
		t.Fatalf("succeeded = %d, want 1 (%s)", env.Succeeded, out.String())
	}
}
