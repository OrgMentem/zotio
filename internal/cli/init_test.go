// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitNoInputJSONReportsSetupRequiredWithoutPrompt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_API_KEY", "")
	t.Setenv("ZOTERO_PROFILE", "")
	t.Setenv("ZOTERO_GROUP", "")
	savedGroup := activeGroupID
	activeGroupID = ""
	t.Cleanup(func() { activeGroupID = savedGroup })

	srv := httptest.NewServer(http.NotFoundHandler())
	baseURL := srv.URL
	srv.Close()
	t.Setenv("ZOTERO_BASE_URL", baseURL)

	flags := &rootFlags{}
	cmd := newRootCmd(flags)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--json", "--no-input", "init"})
	promptInput := strings.NewReader("SHOULD_NOT_BE_READ\n")
	initialInputLen := promptInput.Len()
	cmd.SetIn(promptInput)
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if err == nil {
		t.Fatal("zotio init --json --no-input returned nil error, want setup-required exit")
	}
	var cliErr *cliError
	if !errors.As(err, &cliErr) || cliErr.code != 9 {
		t.Fatalf("init error = %T %[1]v, want precondition cli error code 9", err)
	}
	if code := ExitCode(err); code != 9 {
		t.Fatalf("ExitCode(err) = %d, want 9", code)
	}
	if promptInput.Len() != initialInputLen {
		t.Fatalf("init consumed stdin despite --no-input; remaining bytes = %d, want %d", promptInput.Len(), initialInputLen)
	}
	if strings.Contains(out.String(), "API key:") || strings.Contains(stderr.String(), "API key:") {
		t.Fatalf("init prompted for an API key under --no-input; stdout=%q stderr=%q", out.String(), stderr.String())
	}

	var raw map[string]any
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("decode init JSON %q: %v", out.String(), err)
	}
	if raw["ok"] != false {
		t.Fatalf("raw ok = %v, want false", raw["ok"])
	}
	rawSteps, ok := raw["steps"].([]any)
	if !ok || len(rawSteps) != 4 {
		t.Fatalf("raw steps = %#v, want four step objects", raw["steps"])
	}
	for _, rawStep := range rawSteps {
		step, ok := rawStep.(map[string]any)
		if !ok {
			t.Fatalf("step JSON = %#v, want object", rawStep)
		}
		if _, ok := step["step"].(string); !ok {
			t.Fatalf("step JSON missing string step field: %#v", step)
		}
		if _, ok := step["ok"].(bool); !ok {
			t.Fatalf("step JSON missing bool ok field: %#v", step)
		}
	}

	var report initReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode typed init report: %v", err)
	}
	if report.OK {
		t.Fatal("report.OK = true, want setup-required false")
	}
	steps := map[string]initStepReport{}
	for _, step := range report.Steps {
		steps[step.Step] = step
	}
	assertInitStep(t, steps[initStepLocalAPI], initStepLocalAPI, false, "unreachable", initLocalAPIRemediation)
	assertInitStep(t, steps[initStepAPIKey], initStepAPIKey, false, "missing", "create a Zotero API key")
	assertInitStep(t, steps[initStepSync], initStepSync, false, "blocked", initLocalAPIRemediation)
	assertInitStep(t, steps[initStepHealth], initStepHealth, false, "not_synced", "run zotio sync first")
	if report.HealthVerdict != "" {
		t.Fatalf("health verdict = %q, want empty when health step cannot run", report.HealthVerdict)
	}
}

func assertInitStep(t *testing.T, got initStepReport, wantStep string, wantOK bool, wantStatus, remediationSubstring string) {
	t.Helper()
	if got.Step != wantStep || got.OK != wantOK || got.Status != wantStatus {
		t.Fatalf("%s step = %+v, want ok=%v status=%q", wantStep, got, wantOK, wantStatus)
	}
	if remediationSubstring != "" && !strings.Contains(got.Remediation, remediationSubstring) {
		t.Fatalf("%s remediation = %q, want to contain %q", wantStep, got.Remediation, remediationSubstring)
	}
}
