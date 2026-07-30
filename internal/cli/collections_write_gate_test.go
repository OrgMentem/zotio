// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// collectionsWriteRecorder answers the version GET these commands need and
// records every mutating request, so "did anything reach the library" is a
// direct observation rather than an inference from output.
type collectionsWriteRecorder struct {
	server   *httptest.Server
	mutating []string
}

func newCollectionsWriteRecorder(t *testing.T) *collectionsWriteRecorder {
	t.Helper()
	rec := &collectionsWriteRecorder{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Last-Modified-Version", "7")
			_, _ = w.Write([]byte(`{"key":"K","version":7,"data":{"key":"K","name":"before"}}`))
			return
		}
		rec.mutating = append(rec.mutating, r.Method+" "+r.URL.Path)
		w.Header().Set("Last-Modified-Version", "8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"successful":{}}`))
	}))
	t.Cleanup(rec.server.Close)
	return rec
}

func runCollectionsWriteCmd(t *testing.T, rec *collectionsWriteRecorder, cmd *cobra.Command, args ...string) map[string]any {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", rec.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_API_KEY", "test-key")
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%s %v: %v", cmd.Name(), args, err)
	}
	decoded := map[string]any{}
	if out.Len() > 0 {
		if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
			t.Fatalf("decode %q: %v", out.String(), err)
		}
	}
	return decoded
}

// collections create/update/delete wrote on invocation, honoring none of the
// write-safety gates. MCP advertises those gate flags on every mutating
// command, so a host was shown `yes`, `dry-run`, and `allow-destructive` on
// three commands that ignored all of them -- a false affordance the project's
// threat model treats as a real bug, and the reachable end of an injection
// chain, since untrusted library text reaches the same host model.
func TestCollectionsWritesPreviewUntilExplicitlyApplied(t *testing.T) {
	for _, tc := range []struct {
		name   string
		build  func(*rootFlags) *cobra.Command
		args   []string
		action string
	}{
		{
			name:   "create",
			build:  newCollectionsCreateCmd,
			args:   []string{"--name", "New"},
			action: "create",
		},
		{
			name:   "update",
			build:  newCollectionsUpdateCmd,
			args:   []string{"K", "--name", "Renamed"},
			action: "update",
		},
		{
			name:   "delete",
			build:  newCollectionsDeleteCmd,
			args:   []string{"K"},
			action: "delete",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("default previews", func(t *testing.T) {
				rec := newCollectionsWriteRecorder(t)
				got := runCollectionsWriteCmd(t, rec, tc.build(&rootFlags{asJSON: true}), tc.args...)
				if got["action"] != tc.action || got["dry_run"] != true || got["preview_reason"] != "default" {
					t.Errorf("preview envelope = %v, want a default-reason %s preview", got, tc.action)
				}
				if len(rec.mutating) != 0 {
					t.Errorf("reached the library without --yes: %v", rec.mutating)
				}
			})

			t.Run("dry run previews", func(t *testing.T) {
				rec := newCollectionsWriteRecorder(t)
				got := runCollectionsWriteCmd(t, rec, tc.build(&rootFlags{asJSON: true, dryRun: true}), tc.args...)
				if got["preview_reason"] != "dry_run" {
					t.Errorf("preview_reason = %v, want dry_run", got["preview_reason"])
				}
				if len(rec.mutating) != 0 {
					t.Errorf("reached the library under --dry-run: %v", rec.mutating)
				}
			})

			// --yes with --dry-run must still refuse: ResolveMode applies only
			// when yes is set and dry-run is not.
			t.Run("dry run beats yes", func(t *testing.T) {
				rec := newCollectionsWriteRecorder(t)
				runCollectionsWriteCmd(t, rec, tc.build(&rootFlags{asJSON: true, yes: true, dryRun: true}), tc.args...)
				if len(rec.mutating) != 0 {
					t.Errorf("--dry-run did not override --yes: %v", rec.mutating)
				}
			})

			t.Run("yes applies", func(t *testing.T) {
				rec := newCollectionsWriteRecorder(t)
				runCollectionsWriteCmd(t, rec, tc.build(&rootFlags{asJSON: true, yes: true}), tc.args...)
				if len(rec.mutating) != 1 {
					t.Errorf("mutating requests = %v, want exactly one once --yes is given", rec.mutating)
				}
			})
		})
	}
}

// The annotations are what an MCP host reads to decide whether a tool needs
// approval, so a mutating command that omits them is misrepresented on the
// agent surface even once its runtime gate is correct.
func TestCollectionsWriteCommandsDeclareTheirWriteSafetyAnnotations(t *testing.T) {
	for _, tc := range []struct {
		name        string
		build       func(*rootFlags) *cobra.Command
		destructive string
	}{
		{name: "create", build: newCollectionsCreateCmd, destructive: "false"},
		{name: "update", build: newCollectionsUpdateCmd, destructive: "false"},
		{name: "delete", build: newCollectionsDeleteCmd, destructive: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			annotations := tc.build(&rootFlags{}).Annotations
			if got := annotations["zotio:destructive"]; got != tc.destructive {
				t.Errorf("zotio:destructive = %q, want %q", got, tc.destructive)
			}
			if got := annotations["zotio:supports-dry-run"]; got != "true" {
				t.Errorf("zotio:supports-dry-run = %q, want true", got)
			}
			if annotations["mcp:read-only"] != "" {
				t.Error("a write command must not claim mcp:read-only")
			}
		})
	}
}
