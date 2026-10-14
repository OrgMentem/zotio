// Copyright 2026 OrgMentem. Licensed under MIT.

package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// An unknown --format is a usage error raised before any request, so a typo
// costs no API traffic and never falls through to JSON output.
func TestExportRejectsUnknownFormatBeforeFetching(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"key":"K1"}]`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")

	cmd := newExportCmd(&rootFlags{asJSON: true})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"items", "--format", "bogus"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("export --format bogus succeeded, want a usage error")
	}
	if ExitCode(err) != 2 {
		t.Errorf("ExitCode(%v) = %d, want 2", err, ExitCode(err))
	}
	for _, want := range []string{"bogus", "jsonl", "json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("server received %d requests, want 0", n)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

// JSONL is one compact JSON object per line. The Zotero APIs return records
// pretty-printed, and writing them as returned made one record span dozens of
// lines, so a line reader could not parse the export.
func TestWriteExportJSONLWritesOneCompactObjectPerLine(t *testing.T) {
	data := []byte("[\n  {\n    \"key\": \"K1\",\n    \"data\": {\"title\": \"A\"}\n  },\n  {\n    \"key\": \"K2\"\n  }\n]")
	var out bytes.Buffer
	count, err := writeExport(&out, "jsonl", data, 0)
	if err != nil {
		t.Fatalf("writeExport: %v", err)
	}
	want := "{\"key\":\"K1\",\"data\":{\"title\":\"A\"}}\n{\"key\":\"K2\"}\n"
	if count != 2 || out.String() != want {
		t.Fatalf("writeExport = %d records, %q; want 2 records, %q", count, out.String(), want)
	}
}
