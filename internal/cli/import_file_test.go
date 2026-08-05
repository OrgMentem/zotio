// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// importFileEnvelope is the mutation envelope subset these tests assert on.
type importFileEnvelope struct {
	OK            bool   `json:"ok"`
	Operation     string `json:"operation"`
	Mode          string `json:"mode"`
	PreviewReason string `json:"preview_reason"`
	Plan          struct {
		Summary struct {
			Selected int `json:"selected"`
			Planned  int `json:"planned"`
		} `json:"summary"`
		Operations []struct {
			ID      string `json:"id"`
			Key     string `json:"key"`
			Changes []struct {
				Field string `json:"field"`
				Add   any    `json:"add"`
			} `json:"changes"`
		} `json:"operations"`
	} `json:"plan"`
	Result *struct {
		Summary struct {
			Applied int `json:"applied"`
			Failed  int `json:"failed"`
		} `json:"summary"`
		Items []struct {
			Status string `json:"status"`
			Reason any    `json:"reason"`
		} `json:"items"`
	} `json:"result"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeImportFixture(t *testing.T, content string) string {
	t.Helper()
	filePath := filepath.Join(t.TempDir(), "items.bib")
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("write import fixture: %v", err)
	}
	return filePath
}

func runImportFile(t *testing.T, flags *rootFlags, args ...string) (importFileEnvelope, string, error) {
	t.Helper()
	cmd := newImportFileCmd(flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.Execute()

	var env importFileEnvelope
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
			t.Fatalf("decode envelope %q: %v", out.String(), decodeErr)
		}
	}
	return env, out.String(), err
}

// The write gate is the point of the command: without --yes nothing is sent,
// including under --agent, which only changes formatting.
func TestImportFilePreviewsWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name       string
		flags      rootFlags
		wantReason string
	}{
		{name: "bare", flags: rootFlags{asJSON: true, maxChanges: -1}, wantReason: "default"},
		{name: "agent", flags: rootFlags{asJSON: true, agent: true, maxChanges: -1}, wantReason: "default"},
		{name: "dry-run", flags: rootFlags{asJSON: true, dryRun: true, maxChanges: -1}, wantReason: "dry_run"},
		{name: "dry-run beats yes", flags: rootFlags{asJSON: true, dryRun: true, yes: true, maxChanges: -1}, wantReason: "dry_run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"successful":{"0":{"key":"NEWKEY11"}},"success":{},"unchanged":{},"failed":{}}`))
			}))
			defer srv.Close()
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

			filePath := writeImportFixture(t, "@article{example,\n  title = {Example}\n}\n")
			flags := tc.flags
			env, raw, err := runImportFile(t, &flags, filePath)
			if err != nil {
				t.Fatalf("import file preview: %v", err)
			}
			if requests != 0 {
				t.Fatalf("preview issued %d HTTP request(s); want none", requests)
			}
			if env.Mode != "preview" || env.PreviewReason != tc.wantReason {
				t.Fatalf("mode=%q reason=%q, want preview/%s (%s)", env.Mode, env.PreviewReason, tc.wantReason, raw)
			}
			if env.Result != nil {
				t.Fatalf("preview reported a result: %s", raw)
			}
			if env.Plan.Summary.Planned != 1 {
				t.Fatalf("planned = %d, want 1 (%s)", env.Plan.Summary.Planned, raw)
			}
			// Each parsed record is its own operation, carrying the parsed body.
			if len(env.Plan.Operations) != 1 || len(env.Plan.Operations[0].Changes) != 1 {
				t.Fatalf("operations = %+v, want one record", env.Plan.Operations)
			}
			if env.Plan.Operations[0].Key != "Example" {
				t.Fatalf("operation key = %q, want the parsed title", env.Plan.Operations[0].Key)
			}
			item, ok := env.Plan.Operations[0].Changes[0].Add.(map[string]any)
			if !ok || item["title"] != "Example" {
				t.Fatalf("previewed record = %v, want the parsed item body", env.Plan.Operations[0].Changes[0].Add)
			}
		})
	}
}

// --max-changes is charged per parsed record, so an oversized file is refused
// before any network call.
func TestImportFileRefusesRecordCountAboveMaxChanges(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	var content strings.Builder
	for i := range 3 {
		fmt.Fprintf(&content, "@article{example%d,\n  title = {Example %d}\n}\n", i, i)
	}
	filePath := writeImportFixture(t, content.String())

	env, raw, err := runImportFile(t, &rootFlags{asJSON: true, yes: true, maxChanges: 2}, filePath)
	if err == nil {
		t.Fatal("import error = nil, want a max-changes refusal")
	}
	if env.Error == nil || env.Error.Code != "max_changes_exceeded" {
		t.Fatalf("envelope = %s, want a max_changes_exceeded gate error", raw)
	}
	if !strings.Contains(env.Error.Message, "planned 3 change(s)") || !strings.Contains(env.Error.Message, "cap of 2") {
		t.Fatalf("gate message = %q, want the per-record count and cap", env.Error.Message)
	}
	if requests != 0 {
		t.Fatalf("refused import issued %d HTTP request(s); want none", requests)
	}
}

func TestImportFileReportsBatchWriteFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"successful":{},"success":{},"unchanged":{},"failed":{"0":{"code":400,"message":"title is required"}}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	filePath := writeImportFixture(t, "@article{example,\n  title = {Example}\n}\n")
	env, raw, err := runImportFile(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, filePath)
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("import error = %v, exit=%d; want degraded failure", err, ExitCode(err))
	}
	if env.Result == nil || env.Result.Summary.Failed != 1 || env.Result.Summary.Applied != 0 {
		t.Fatalf("result = %s, want the single record failed", raw)
	}
	reason, _ := env.Result.Items[0].Reason.(string)
	for _, want := range []string{"index 0", "code 400", "title is required"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("failure reason = %q, want %q", reason, want)
		}
	}
}

func TestImportFileOffsetsLaterBatchWriteFailureIndexes(t *testing.T) {
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		if requestCount == 2 {
			_, _ = w.Write([]byte(`{"successful":{},"success":{},"unchanged":{},"failed":{"0":{"code":400,"message":"title is required"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"successful":{"0":{"key":"NEWKEY11"}},"success":{},"unchanged":{},"failed":{}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	var content strings.Builder
	for i := range importFileBatchSize + 1 {
		fmt.Fprintf(&content, "@article{example%d,\n  title = {Example %d}\n}\n", i, i)
	}
	filePath := writeImportFixture(t, content.String())

	env, raw, err := runImportFile(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, filePath)
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("import error = %v, exit=%d; want degraded failure", err, ExitCode(err))
	}
	if requestCount != 2 {
		t.Fatalf("import requests = %d, want 2 batches", requestCount)
	}
	// The rejection lands on the record that produced it, and the 50 records
	// posted in the first batch are still reported as applied.
	if env.Result == nil || env.Result.Summary.Applied != importFileBatchSize || env.Result.Summary.Failed != 1 {
		t.Fatalf("result = %s, want %d applied and 1 failed", raw, importFileBatchSize)
	}
	if got := env.Result.Items[importFileBatchSize].Status; got != "failed" {
		t.Fatalf("record %d status = %q, want failed", importFileBatchSize, got)
	}
	reason, _ := env.Result.Items[importFileBatchSize].Reason.(string)
	if !strings.Contains(reason, "index 50") {
		t.Fatalf("failure reason = %q, want source-relative index 50", reason)
	}
}

func TestImportFilePreservesNonNumericBatchWriteFailureIndexes(t *testing.T) {
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		if requestCount == 2 {
			_, _ = w.Write([]byte(`{"successful":{},"success":{},"unchanged":{},"failed":{"unexpected":{"code":400,"message":"title is required"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"successful":{"0":{"key":"NEWKEY11"}},"success":{},"unchanged":{},"failed":{}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	var content strings.Builder
	for i := range importFileBatchSize + 1 {
		fmt.Fprintf(&content, "@article{example%d,\n  title = {Example %d}\n}\n", i, i)
	}
	filePath := writeImportFixture(t, content.String())

	env, raw, err := runImportFile(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, filePath)
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("import error = %v, exit=%d; want degraded failure", err, ExitCode(err))
	}
	if requestCount != 2 {
		t.Fatalf("import requests = %d, want 2 batches", requestCount)
	}
	if env.Result == nil || env.Result.Summary.Failed != 1 {
		t.Fatalf("result = %s, want the non-numeric rejection reported once", raw)
	}
	// A non-numeric element index cannot name a record, so it is charged to the
	// batch's first record rather than dropped.
	reason, _ := env.Result.Items[importFileBatchSize].Reason.(string)
	if !strings.Contains(reason, "index unexpected") {
		t.Fatalf("failure reason = %q, want the preserved non-numeric index", reason)
	}
}

func TestImportFileReportsSuccessfulBatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"successful":{"0":{"key":"NEWKEY11"}},"success":{},"unchanged":{},"failed":{}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	filePath := writeImportFixture(t, "@article{example,\n  title = {Example}\n}\n")
	env, raw, err := runImportFile(t, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, filePath)
	if err != nil {
		t.Fatalf("import successful batch: %v", err)
	}
	if !env.OK || env.Mode != "apply" {
		t.Fatalf("envelope = %s, want an applied run", raw)
	}
	if env.Result == nil || env.Result.Summary.Applied != 1 || env.Result.Summary.Failed != 0 {
		t.Fatalf("result = %s, want one applied batch", raw)
	}
}

func TestImportFileRejectsCSLJSONWithoutConnector(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "items.json")
	if err := os.WriteFile(filePath, []byte(`[{"type":"article-journal","title":"Example"}]`), 0o600); err != nil {
		t.Fatalf("write CSL JSON fixture: %v", err)
	}

	cmd := newImportFileCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"--format", "csljson", filePath})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--via connector") {
		t.Fatalf("CSL JSON error = %v, want translator guidance via --via connector", err)
	}
}
