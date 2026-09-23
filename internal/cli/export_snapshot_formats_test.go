// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestExportSnapshotTranslatorPagesAndManifest(t *testing.T) {
	const count = 5
	for _, format := range []string{"bibtex", "ris", "csljson"} {
		t.Run(format, func(t *testing.T) {
			var mu sync.Mutex
			var starts []int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("include") != "" && (r.URL.Query().Get("format") != "json" || r.URL.Query().Get("include") != "data,"+format) {
					t.Errorf("query = %s, want format=json&include=data,%s", r.URL.RawQuery, format)
				}
				start, _ := strconv.Atoi(r.URL.Query().Get("start"))
				limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
				if r.URL.Query().Get("include") != "" {
					mu.Lock()
					starts = append(starts, start)
					mu.Unlock()
				}
				items := make([]map[string]any, 0)
				for i := start; i < start+limit && i < count; i++ {
					item := snapshotFormatItem(i)
					items = append(items, item)
				}
				_ = json.NewEncoder(w).Encode(items)
			}))
			t.Cleanup(srv.Close)
			t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
			t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
			output := filepath.Join(t.TempDir(), "library."+format)
			if err := runSnapshotFormat(output, format, false); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			gotStarts := fmt.Sprint(starts)
			mu.Unlock()
			if gotStarts != "[0 2 4]" {
				t.Errorf("page starts = %s, want [0 2 4]", gotStarts)
			}
			assertSnapshotFormatContent(t, output, format, count)
			assertSnapshotFormatManifest(t, output, format, count)
			if _, err := os.Stat(output + ".checkpoint.json.items.jsonl"); !os.IsNotExist(err) {
				t.Errorf("checkpoint items remain after success: %v", err)
			}
			// Verification reads only the manifest and current library data, not
			// the translator output as if it were JSONL.
			verify := newExportSnapshotVerifyCmd(&rootFlags{asJSON: true})
			verify.SetArgs([]string{output + ".manifest.json", "--fail-on-drift"})
			verify.SetOut(&bytes.Buffer{})
			if err := verify.Execute(); err != nil {
				t.Fatalf("verify %s snapshot: %v", format, err)
			}
		})
	}
}

func snapshotFormatItem(i int) map[string]any {
	key := fmt.Sprintf("K%03d", i)
	return map[string]any{
		"key": key, "version": i + 1,
		"data":    map[string]any{"key": key, "version": i + 1, "title": fmt.Sprintf("Title %d", i)},
		"bibtex":  fmt.Sprintf("@book{%s, title={Title %d}}\n", key, i),
		"ris":     fmt.Sprintf("TY  - BOOK\nID  - %s\nER  - \n", key),
		"csljson": map[string]any{"id": key, "title": fmt.Sprintf("Title %d", i)},
	}
}

func runSnapshotFormat(output, format string, resume bool) error {
	cmd := newExportSnapshotCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	args := []string{"--output", output, "--page-size", "2", "--format", format}
	if resume {
		args = append(args, "--resume")
	}
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return cmd.Execute()
}

func assertSnapshotFormatContent(t *testing.T, output, format string, count int) {
	t.Helper()
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if format == "csljson" {
		var entries []map[string]any
		if err := json.Unmarshal(data, &entries); err != nil {
			t.Fatalf("CSL output is not one JSON array: %v: %s", err, data)
		}
		if len(entries) != count {
			t.Fatalf("CSL entries = %d, want %d", len(entries), count)
		}
		for i, entry := range entries {
			if entry["id"] != fmt.Sprintf("K%03d", i) {
				t.Errorf("CSL entry %d id = %v", i, entry["id"])
			}
		}
		return
	}
	var want strings.Builder
	for i := 0; i < count; i++ {
		want.WriteString(snapshotFormatItem(i)[format].(string))
	}
	if string(data) != want.String() {
		t.Errorf("%s output = %q, want %q", format, data, want.String())
	}
}

func assertSnapshotFormatManifest(t *testing.T, output, format string, count int) {
	t.Helper()
	lf, err := readExportLockfile(output + ".manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if lf.Format != format || lf.Count != count || len(lf.Items) != count {
		t.Fatalf("manifest format=%q count=%d items=%d", lf.Format, lf.Count, len(lf.Items))
	}
	for i, entry := range lf.Items {
		raw, _ := json.Marshal(snapshotFormatItem(i))
		expectedSHA, err := exportItemContentSHA256(raw)
		if err != nil {
			t.Fatal(err)
		}
		if entry.Key != fmt.Sprintf("K%03d", i) || entry.Version != i+1 || entry.ContentSHA256 != expectedSHA {
			t.Errorf("manifest item %d = %+v; JSONL hash = %s", i, entry, expectedSHA)
		}
	}
}

func TestExportSnapshotCSLJSONInterruptedResume(t *testing.T) {
	var mu sync.Mutex
	var starts []int
	failed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		mu.Lock()
		starts = append(starts, start)
		if start == 2 && !failed {
			failed = true
			mu.Unlock()
			http.Error(w, "temporary failure", http.StatusBadRequest)
			return
		}
		mu.Unlock()
		items := make([]map[string]any, 0)
		for i := start; i < start+2 && i < 5; i++ {
			items = append(items, snapshotFormatItem(i))
		}
		_ = json.NewEncoder(w).Encode(items)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	output := filepath.Join(t.TempDir(), "library.json")
	if err := runSnapshotFormat(output, "csljson", false); err == nil {
		t.Fatal("interrupted snapshot unexpectedly succeeded")
	}
	assertSnapshotFormatContent(t, output, "csljson", 2)
	cp, ok := readExportCheckpoint(output + ".checkpoint.json")
	if !ok || cp.Format != "csljson" || cp.Fetched != 2 {
		t.Fatalf("checkpoint = %+v, readable=%v", cp, ok)
	}
	// Simulate a crash after writing part of an uncommitted page. Resume
	// rebuilds the data from the checkpoint's committed item count.
	sidecar, err := os.OpenFile(output+".checkpoint.json.items.jsonl", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sidecar.WriteString(`{"key":"UNCOMMITTED"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := sidecar.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte(`[{"id":"PARTIAL"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runSnapshotFormat(output, "csljson", true); err != nil {
		t.Fatalf("resume: %v", err)
	}
	assertSnapshotFormatContent(t, output, "csljson", 5)
	assertSnapshotFormatManifest(t, output, "csljson", 5)
	mu.Lock()
	gotStarts := fmt.Sprint(starts)
	mu.Unlock()
	if gotStarts != "[0 2 2 4]" {
		t.Errorf("page starts = %s, want [0 2 2 4]", gotStarts)
	}
}

func TestExportSnapshotMissingIncludeAndFormatMismatch(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		item := snapshotFormatItem(0)
		delete(item, "bibtex")
		_ = json.NewEncoder(w).Encode([]any{item})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	output := filepath.Join(t.TempDir(), "library.bib")
	err := runSnapshotFormat(output, "bibtex", false)
	var cliErr *cliError
	if !errors.As(err, &cliErr) || cliErr.code != 5 || !strings.Contains(err.Error(), "K000") || !strings.Contains(err.Error(), "bibtex") {
		t.Fatalf("missing include error = %T %[1]v, want exit 5 with item and format", err)
	}
	// Seed an interrupted checkpoint with a different translator format.
	c, err := (&rootFlags{}).newClient()
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]string{"format": "json", "include": "data,csljson"}
	cp := exportCheckpoint{Path: "/items", Source: exportReadSource(c, ""), Format: "csljson", Scope: exportCheckpointScope(exportReadSource(c, ""), "/items", params, 2, 0), Fetched: 1, NextStart: 1}
	if err := writeExportCheckpoint(output+".checkpoint.json", cp); err != nil {
		t.Fatal(err)
	}
	before := requests
	err = runSnapshotFormat(output, "bibtex", true)
	if err == nil || !strings.Contains(err.Error(), "checkpoint scope does not match") {
		t.Fatalf("format mismatch error = %v", err)
	}
	if requests != before {
		t.Errorf("requests after format mismatch = %d, want %d", requests, before)
	}
	err = runSnapshotFormat(output, "unknown", false)
	if !errors.As(err, &cliErr) || cliErr.code != 2 {
		t.Fatalf("unknown format error = %T %[1]v, want exit 2", err)
	}
}
