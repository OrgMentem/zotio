// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean roadmap-phase7 capabilities-drift): cover the read-only endpoint
// probe report shape so API capability drift is visible to agents.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestCapabilitiesDriftReportsSchemaEndpointDrift(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/items"),
			strings.HasSuffix(r.URL.Path, "/collections"),
			strings.HasSuffix(r.URL.Path, "/tags"),
			strings.HasSuffix(r.URL.Path, "/searches"),
			strings.HasSuffix(r.URL.Path, "/itemTypes"):
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/itemFields"):
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ZOTERO_BASE_URL", srv.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	flags := &rootFlags{asJSON: true}
	rootForRegistry := &cobra.Command{Use: "zotio"}
	cmd := newCapabilitiesCmd(rootForRegistry, flags)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs([]string{"drift"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("capabilities drift: %v", err)
	}

	var report struct {
		Checked int `json:"checked"`
		OK      int `json:"ok"`
		Drifted []struct {
			Path  string `json:"path"`
			Error string `json:"error"`
		} `json:"drifted"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out.String())
	}
	if report.Checked != 6 {
		t.Fatalf("checked = %d, want 6", report.Checked)
	}
	if report.OK != 5 {
		t.Fatalf("ok = %d, want 5", report.OK)
	}

	var sawItemFields bool
	for _, drift := range report.Drifted {
		if drift.Path == "/itemFields" {
			sawItemFields = true
			if drift.Error == "" {
				t.Fatal("/itemFields drift has empty error")
			}
		}
		if drift.Path == "/items" {
			t.Fatal("/items unexpectedly reported as drifted")
		}
	}
	if !sawItemFields {
		t.Fatalf("drifted = %+v, want /itemFields", report.Drifted)
	}
}
