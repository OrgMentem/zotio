// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWatchRejectsTooShortInterval(t *testing.T) {
	cmd := newWatchCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--interval", "1s", "--once"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("watch --interval 1s --once returned nil, want usage error")
	}
	var cliErr *cliError
	if !errors.As(err, &cliErr) || cliErr.code != 2 {
		t.Fatalf("watch --interval 1s --once error = %T %[1]v, want usageErr code 2", err)
	}
	if !strings.Contains(err.Error(), "10s") {
		t.Fatalf("watch --interval 1s --once error = %q, want 10s minimum", err.Error())
	}
}

func runWatchOnceTest(t *testing.T, workflowPath string) (string, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[]`)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("HOME", t.TempDir())

	cmd := newWatchCmd(&rootFlags{timeout: time.Second})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	args := []string{"--interval", "10s", "--once"}
	if workflowPath != "" {
		args = append(args, "--workflow", workflowPath)
	}
	cmd.SetArgs(args)

	err := cmd.Execute()
	return stderr.String(), err
}

func TestWatchOnceRunsSingleSyncCycle(t *testing.T) {
	if _, err := runWatchOnceTest(t, ""); err != nil {
		t.Fatalf("watch --interval 10s --once returned error: %v", err)
	}
}

func TestWatchOnceWorkflowPreview(t *testing.T) {
	specPath := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"version"}},
	}})

	stderr, err := runWatchOnceTest(t, specPath)
	if err != nil {
		t.Fatalf("watch --once --workflow returned error: %v; stderr=%s", err, stderr)
	}
	if !strings.Contains(stderr, "workflow preview ok") {
		t.Fatalf("stderr = %q, want workflow preview success summary", stderr)
	}
}

func TestWatchOnceWorkflowMissingSpecFailsFast(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "missing-workflow.json")
	_, wantErr := readWorkflowRunSpec(specPath)
	if wantErr == nil {
		t.Fatal("read missing workflow spec returned nil error")
	}

	cmd := newWatchCmd(&rootFlags{})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--once", "--workflow", specPath})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("watch --once --workflow missing spec returned nil error")
	}
	if err.Error() != wantErr.Error() {
		t.Fatalf("watch missing spec error = %q, want %q", err, wantErr)
	}
}

func TestWatchOnceWorkflowFailureDoesNotFailCycle(t *testing.T) {
	specPath := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
		{Args: []string{"definitely-not-a-command"}},
	}})

	stderr, err := runWatchOnceTest(t, specPath)
	if err != nil {
		t.Fatalf("watch --once failing workflow returned error: %v; stderr=%s", err, stderr)
	}
	if !strings.Contains(stderr, "workflow preview failed:") {
		t.Fatalf("stderr = %q, want workflow failure summary", stderr)
	}
}
