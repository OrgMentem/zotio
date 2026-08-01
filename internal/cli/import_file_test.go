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

func TestImportFileDryRunPrintsPreviewWithoutImporting(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "items.bib")
	if err := os.WriteFile(filePath, []byte("@article{example,\n  title = {Example}\n}\n"), 0o600); err != nil {
		t.Fatalf("write import fixture: %v", err)
	}
	t.Setenv("ZOTERO_BASE_URL", "http://127.0.0.1:1/users/0")

	cmd := newImportFileCmd(&rootFlags{asJSON: true, dryRun: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{filePath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import file dry-run: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode preview %q: %v", out.String(), err)
	}
	if got["dry_run"] != true || got["success"] != false || got["status"] != float64(0) {
		t.Fatalf("preview = %+v, want explicit unsuccessful dry-run", got)
	}
	if got["planned"] != float64(1) {
		t.Fatalf("preview = %+v, want one planned import", got)
	}
	if _, ok := got["imported"]; ok {
		t.Fatalf("preview = %+v, must not claim imported items", got)
	}
}

func TestImportFileReportsBatchWriteFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"successful":{},"success":{},"unchanged":{},"failed":{"0":{"code":400,"message":"title is required"}}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	filePath := filepath.Join(t.TempDir(), "items.bib")
	if err := os.WriteFile(filePath, []byte("@article{example,\n  title = {Example}\n}\n"), 0o600); err != nil {
		t.Fatalf("write import fixture: %v", err)
	}

	cmd := newImportFileCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{filePath})
	err := cmd.Execute()
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("import error = %v, exit=%d; want degraded failure", err, ExitCode(err))
	}
	for _, want := range []string{"index 0", "code 400", "title is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("import error = %q, want %q", err, want)
		}
	}
	if out.Len() != 0 {
		t.Fatalf("import output = %q, must not report a failed batch as imported", out.String())
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
	for i := 0; i <= importFileBatchSize; i++ {
		fmt.Fprintf(&content, "@article{example%d,\n  title = {Example %d}\n}\n", i, i)
	}
	filePath := filepath.Join(t.TempDir(), "items.bib")
	if err := os.WriteFile(filePath, []byte(content.String()), 0o600); err != nil {
		t.Fatalf("write import fixture: %v", err)
	}

	cmd := newImportFileCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{filePath})
	err := cmd.Execute()
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("import error = %v, exit=%d; want degraded failure", err, ExitCode(err))
	}
	if requestCount != 2 {
		t.Fatalf("import requests = %d, want 2 batches", requestCount)
	}
	if !strings.Contains(err.Error(), "index 50") {
		t.Fatalf("import error = %q, want source-relative index 50", err)
	}
	if strings.Contains(err.Error(), "index 0") {
		t.Fatalf("import error = %q, must not report the second batch's relative index", err)
	}
}

func TestImportFilePreservesNonNumericBatchWriteFailureIndexes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"successful":{},"success":{},"unchanged":{},"failed":{"unexpected":{"code":400,"message":"title is required"}}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	filePath := filepath.Join(t.TempDir(), "items.bib")
	if err := os.WriteFile(filePath, []byte("@article{example,\n  title = {Example}\n}\n"), 0o600); err != nil {
		t.Fatalf("write import fixture: %v", err)
	}

	cmd := newImportFileCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{filePath})
	err := cmd.Execute()
	if err == nil || ExitCode(err) != 13 {
		t.Fatalf("import error = %v, exit=%d; want degraded failure", err, ExitCode(err))
	}
	if !strings.Contains(err.Error(), "index unexpected") {
		t.Fatalf("import error = %q, want unchanged non-numeric index", err)
	}
}

func TestImportFileReportsSuccessfulBatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"successful":{"0":{"key":"NEWKEY11"}},"success":{},"unchanged":{},"failed":{}}`))
	}))
	defer srv.Close()
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	filePath := filepath.Join(t.TempDir(), "items.bib")
	if err := os.WriteFile(filePath, []byte("@article{example,\n  title = {Example}\n}\n"), 0o600); err != nil {
		t.Fatalf("write import fixture: %v", err)
	}

	cmd := newImportFileCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{filePath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("import successful batch: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode import output %q: %v", out.String(), err)
	}
	if got["file"] != filePath || got["imported"] != float64(1) {
		t.Fatalf("import output = %+v, want unchanged success report", got)
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
