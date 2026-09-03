// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// probe report shape so API capability drift is visible to agents.

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
		Checked  int       `json:"checked"`
		OK       int       `json:"ok"`
		Findings []Finding `json:"findings"`
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
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %+v, want exactly the /itemFields drift", report.Findings)
	}

	finding := report.Findings[0]
	if finding.Kind != "api_drift" || finding.Severity != sevCritical {
		t.Fatalf("finding kind/severity = %q/%q, want api_drift/%s", finding.Kind, finding.Severity, sevCritical)
	}
	if got := sqlStringValue(finding.Evidence["value"]); got != "/itemFields" {
		t.Fatalf("finding evidence value = %q, want /itemFields", got)
	}
	if sqlStringValue(finding.Evidence["error"]) == "" {
		t.Fatal("/itemFields drift carries an empty error")
	}
	// "live", not "web_api": the probe follows the configured base, which may
	// be Zotero desktop's local API.
	if finding.Source.Kind != "live" {
		t.Fatalf("finding source = %q, want live", finding.Source.Kind)
	}
	// The registry declares the endpoint readable and nothing here repairs it,
	// so the action must triage rather than claim an autofix.
	if finding.Autofixable {
		t.Fatal("api_drift must not claim to be autofixable")
	}
	if finding.RecommendedAction == nil || finding.RecommendedAction.Command != "zotio doctor" {
		t.Fatalf("finding action = %+v, want the doctor triage command", finding.RecommendedAction)
	}

	// The human report now renders from the finding's evidence rather than a
	// second private struct; a wrong evidence key would silently print an
	// empty path and an empty reason.
	humanCmd := newCapabilitiesCmd(&cobra.Command{Use: "zotio"}, &rootFlags{})
	humanCmd.SilenceErrors, humanCmd.SilenceUsage = true, true
	humanCmd.SetArgs([]string{"drift"})
	var humanOut bytes.Buffer
	humanCmd.SetOut(&humanOut)
	humanCmd.SetErr(&bytes.Buffer{})
	if err := humanCmd.Execute(); err != nil {
		t.Fatalf("capabilities drift (human): %v", err)
	}
	human := humanOut.String()
	if !strings.Contains(human, "6 endpoints checked, 5 ok, 1 drifted") {
		t.Fatalf("human drift summary missing:\n%s", human)
	}
	if !strings.Contains(human, "/itemFields: ") || strings.Contains(human, "/itemFields: \n") {
		t.Fatalf("human drift line lacks the path and its error:\n%s", human)
	}
}

// A permanently broken endpoint must keep one identity across runs. The probe
// error text carries per-request detail (timeouts, status lines), so keying on
// it would report the same dead endpoint as a brand-new finding every run and
// break the cross-run diff FindingIdentities feeds.
func TestCapabilitiesDriftFindingIdentityIgnoresErrorText(t *testing.T) {
	first := capabilitiesDriftFinding("/itemFields", errors.New("GET /itemFields: 404 Not Found"))
	second := capabilitiesDriftFinding("/itemFields", errors.New("GET /itemFields: dial tcp 127.0.0.1:1: connect: connection refused"))
	if watchHealthFindingKey(first) != watchHealthFindingKey(second) {
		t.Fatalf("identity changed with the error text: %q vs %q", watchHealthFindingKey(first), watchHealthFindingKey(second))
	}
	if got := watchHealthFindingKey(first); got != "api_drift\x00group\x00path\x00/itemFields" {
		t.Fatalf("identity = %q, want the (kind, path) grouped key", got)
	}
	other := capabilitiesDriftFinding("/items", errors.New("GET /items: 404 Not Found"))
	if watchHealthFindingKey(first) == watchHealthFindingKey(other) {
		t.Fatal("two different endpoints must not share one identity")
	}
}
