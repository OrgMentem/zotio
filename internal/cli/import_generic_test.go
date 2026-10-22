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

// Zotero write endpoints take arrays: each record is POSTed as a
// one-element array, and a 200 that lists the record under "failed" is a
// failed record attributed to its input line on stderr and in the JSON.
func TestGenericImportSendsArrayBodyAndAttributesFailures(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		bodies = append(bodies, buf.String())
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(buf.String(), `"B"`) {
			_, _ = w.Write([]byte(`{"successful":{},"failed":{"0":{"key":"","code":400,"message":"bad itemType"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"successful":{"0":{"key":"ABCD2345"}},"failed":{}}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	filePath := writeImportGenericFixture(t, "{\"title\":\"A\"}\n\n{\"title\":\"B\"}\n{\"title\":\"C\"}\n")
	flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
	_, raw, stderr, err := runImportGeneric(t, flags, "items", "--input", filePath)
	if err == nil {
		t.Fatalf("expected incomplete error (%s)", raw)
	}
	if len(bodies) != 3 {
		t.Fatalf("posted %d bodies, want 3", len(bodies))
	}
	for _, b := range bodies {
		if !strings.HasPrefix(b, "[{") || !strings.HasSuffix(strings.TrimSpace(b), "}]") {
			t.Fatalf("body %q is not a one-element array", b)
		}
	}
	if !strings.Contains(stderr, "record 2 (line 3): zotero rejected the record (code 400): bad itemType") {
		t.Fatalf("stderr lacks attribution: %q", stderr)
	}
	var out struct {
		Mode      string `json:"mode"`
		Succeeded int    `json:"succeeded"`
		Failed    int    `json:"failed"`
		Failures  []struct {
			Record int    `json:"record"`
			Line   int    `json:"line"`
			Reason string `json:"reason"`
		} `json:"failures"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	if out.Mode != "apply" || out.Succeeded != 2 || out.Failed != 1 || len(out.Failures) != 1 ||
		out.Failures[0].Record != 2 || out.Failures[0].Line != 3 || !strings.Contains(out.Failures[0].Reason, "bad itemType") {
		t.Fatalf("unexpected output: %s", raw)
	}
}

// The preview renders the real plan (mode, planned operations, endpoint).
func TestGenericImportPreviewShowsPlan(t *testing.T) {
	_, requests := recordingImportServer(t)
	filePath := writeImportGenericFixture(t, "{\"title\":\"A\"}\n{\"title\":\"B\"}\n")
	flags := &rootFlags{asJSON: true, maxChanges: -1}
	_, raw, _, err := runImportGeneric(t, flags, "items", "--input", filePath)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if *requests != 0 {
		t.Fatalf("preview posted %d requests", *requests)
	}
	var env struct {
		Mode string `json:"mode"`
		Plan struct {
			Operations []struct {
				Changes []struct {
					Field string `json:"field"`
					Add   any    `json:"add"`
				} `json:"changes"`
			} `json:"operations"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	if env.Mode != "preview" || len(env.Plan.Operations) != 2 {
		t.Fatalf("want preview with 2 planned ops, got %s", raw)
	}
	if !strings.Contains(raw, "POST /items") {
		t.Fatalf("plan lacks endpoint: %s", raw)
	}
}

// A JSONL line that is not a JSON object (null decodes to a nil map without an
// error) is a parse failure, never a create op.
func TestGenericImportRejectsNonObjectLines(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		wantPosts   int
		wantFailed  int
	}{
		{"null line", "null\n{\"title\":\"A\"}\n", 1, 1},
		{"only null", "null\n", 0, 1},
		{"array line", "[1]\n{\"title\":\"A\"}\n", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, requests := recordingImportServer(t)
			filePath := writeImportGenericFixture(t, tc.input)
			flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
			env, raw, errOut, _ := runImportGeneric(t, flags, "items", "--input", filePath)
			if *requests != tc.wantPosts {
				t.Fatalf("posted %d request(s), want %d (%s)", *requests, tc.wantPosts, raw)
			}
			if env.Failed != tc.wantFailed {
				t.Fatalf("failed = %d, want %d (%s)", env.Failed, tc.wantFailed, raw)
			}
			if !strings.Contains(errOut, "skipping invalid JSON") {
				t.Fatalf("stderr = %q, want a skip warning", errOut)
			}
		})
	}
}
