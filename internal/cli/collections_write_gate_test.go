// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

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

// collectionsWriteRecorder answers the version GET these commands need and
// records every request, so gate refusals can prove that no network call was
// attempted rather than merely proving no mutation was sent.
type collectionsWriteRecorder struct {
	server    *httptest.Server
	requests  []string
	mutating  []string
	putHeader string
}

func newCollectionsWriteRecorder(t *testing.T) *collectionsWriteRecorder {
	t.Helper()
	rec := &collectionsWriteRecorder{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := r.Method + " " + r.URL.Path
		rec.requests = append(rec.requests, request)
		if r.Method == http.MethodGet {
			w.Header().Set("Last-Modified-Version", "7")
			_, _ = w.Write([]byte(`{"key":"K","version":7,"data":{"key":"K","name":"before"}}`))
			return
		}
		if r.Method == http.MethodPut {
			rec.putHeader = r.Header.Get("If-Unmodified-Since-Version")
		}
		rec.mutating = append(rec.mutating, request)
		w.Header().Set("Last-Modified-Version", "8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"successful":{}}`))
	}))
	t.Cleanup(rec.server.Close)
	return rec
}

func runCollectionsWriteCmd(t *testing.T, rec *collectionsWriteRecorder, cmd *cobra.Command, args ...string) map[string]any {
	t.Helper()
	decoded, err := executeCollectionsWriteCmd(t, rec, cmd, args...)
	if err != nil {
		t.Fatalf("%s %v: %v", cmd.Name(), args, err)
	}
	return decoded
}

func executeCollectionsWriteCmd(t *testing.T, rec *collectionsWriteRecorder, cmd *cobra.Command, args ...string) (map[string]any, error) {
	t.Helper()
	t.Setenv("ZOTERO_BASE_URL", rec.server.URL+"/users/0")
	t.Setenv("ZOTERO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	t.Setenv("ZOTERO_API_KEY", "test-key")
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.Execute()
	decoded := map[string]any{}
	if out.Len() > 0 {
		if decodeErr := json.Unmarshal(out.Bytes(), &decoded); decodeErr != nil {
			t.Fatalf("decode %q: %v", out.String(), decodeErr)
		}
	}
	return decoded, err
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
				flags := &rootFlags{asJSON: true, yes: true, maxChanges: -1}
				if tc.name == "delete" {
					flags.allowDestructive = true
				}
				runCollectionsWriteCmd(t, rec, tc.build(flags), tc.args...)
				if len(rec.mutating) != 1 {
					t.Errorf("mutating requests = %v, want exactly one once --yes is given", rec.mutating)
				}
			})
		})
	}
}

func TestCollectionsUpdateAppliesVersionPrecondition(t *testing.T) {
	rec := newCollectionsWriteRecorder(t)
	cmd := newCollectionsUpdateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})

	runCollectionsWriteCmd(t, rec, cmd, "K", "--name", "Renamed")

	if got, want := strings.Join(rec.requests, ","), "GET /users/0/collections/K,PUT /users/0/collections/K"; got != want {
		t.Fatalf("request order = %q, want %q", got, want)
	}
	if got, want := rec.putHeader, "7"; got != want {
		t.Fatalf("If-Unmodified-Since-Version = %q, want %q", got, want)
	}
}

func TestCollectionsCreateHonorsMaxChanges(t *testing.T) {
	rec := newCollectionsWriteRecorder(t)
	cmd := newCollectionsCreateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: 1})
	cmd.SetIn(bytes.NewBufferString(`[{"name":"one"},{"name":"two"}]`))

	_, err := executeCollectionsWriteCmd(t, rec, cmd, "--stdin")
	if err == nil {
		t.Fatal("collections create succeeded above --max-changes")
	}
	// tags rename sends max_changes_exceeded through runMutation, which exits 1.
	if ExitCode(err) != 1 {
		t.Fatalf("ExitCode(%v) = %d, want tags rename-compatible exit 1", err, ExitCode(err))
	}
	if !strings.Contains(err.Error(), "planned 2 change(s), which exceeds the cap of 1") {
		t.Fatalf("error = %q, want max-changes gate message", err)
	}
	if len(rec.requests) != 0 {
		t.Fatalf("requests = %v, want none after gate refusal", rec.requests)
	}
}

func TestCollectionsUpdateHonorsMaxChanges(t *testing.T) {
	rec := newCollectionsWriteRecorder(t)
	cmd := newCollectionsUpdateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: 0})

	_, err := executeCollectionsWriteCmd(t, rec, cmd, "K", "--name", "Renamed")
	if err == nil {
		t.Fatal("collections update succeeded above --max-changes")
	}
	if ExitCode(err) != 1 {
		t.Fatalf("ExitCode(%v) = %d, want tags rename-compatible exit 1", err, ExitCode(err))
	}
	if !strings.Contains(err.Error(), "planned 1 change(s), which exceeds the cap of 0") {
		t.Fatalf("error = %q, want max-changes gate message", err)
	}
	if len(rec.requests) != 0 {
		t.Fatalf("requests = %v, want none after gate refusal", rec.requests)
	}
}

func TestCollectionsDeleteRequiresDestructiveOptIn(t *testing.T) {
	t.Run("refuses without opt-in", func(t *testing.T) {
		rec := newCollectionsWriteRecorder(t)
		cmd := newCollectionsDeleteCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})

		_, err := executeCollectionsWriteCmd(t, rec, cmd, "K")
		if err == nil {
			t.Fatal("collections delete succeeded without --allow-destructive")
		}
		if ExitCode(err) != 1 {
			t.Fatalf("ExitCode(%v) = %d, want tags rename-compatible exit 1", err, ExitCode(err))
		}
		if !strings.Contains(err.Error(), "destructive changes require --allow-destructive") {
			t.Fatalf("error = %q, want destructive opt-in message", err)
		}
		if len(rec.requests) != 0 {
			t.Fatalf("requests = %v, want none after destructive gate refusal", rec.requests)
		}
	})

	t.Run("applies with opt-in", func(t *testing.T) {
		rec := newCollectionsWriteRecorder(t)
		cmd := newCollectionsDeleteCmd(&rootFlags{
			asJSON: true, yes: true, maxChanges: -1, allowDestructive: true,
		})

		runCollectionsWriteCmd(t, rec, cmd, "K")
		if len(rec.mutating) != 1 || rec.mutating[0] != "DELETE /users/0/collections/K" {
			t.Fatalf("mutating requests = %v, want DELETE /users/0/collections/K", rec.mutating)
		}
	})
}

func TestCollectionsDeleteCapabilityIsDestructive(t *testing.T) {
	for _, capability := range buildCapabilityRegistry(RootCmd()) {
		if capability.Path != "collections delete" {
			continue
		}
		if capability.Operation != "write" || !capability.Destructive {
			t.Fatalf("collections delete capability = %+v, want destructive write", capability)
		}
		return
	}
	t.Fatal("collections delete is absent from the capability registry")
}
