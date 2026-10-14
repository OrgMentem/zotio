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
			_, _ = w.Write([]byte(`{"key":"CLCTAB23","version":7,"data":{"key":"CLCTAB23","name":"before"}}`))
			return
		}
		if r.Method == http.MethodPut {
			rec.putHeader = r.Header.Get("If-Unmodified-Since-Version")
		}
		rec.mutating = append(rec.mutating, request)
		w.Header().Set("Last-Modified-Version", "8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodPost {
			// A real batch create answers with the object keyed by its
			// request index. An empty envelope attributes nothing, which
			// the batch-envelope gate now refuses as an unknown outcome.
			_, _ = w.Write([]byte(`{"success":{"0":"NEWCOLL1"},"successful":{"0":{"key":"NEWCOLL1","version":8,"data":{"key":"NEWCOLL1","name":"New"}}},"unchanged":{},"failed":{}}`))
			return
		}
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
		name      string
		build     func(*rootFlags) *cobra.Command
		args      []string
		operation string
	}{
		{
			name:      "create",
			build:     newCollectionsCreateCmd,
			args:      []string{"--name", "New"},
			operation: "collections.create",
		},
		{
			name:      "update",
			build:     newCollectionsUpdateCmd,
			args:      []string{"CLCTAB23", "--name", "Renamed"},
			operation: "collections.update",
		},
		{
			name:      "delete",
			build:     newCollectionsDeleteCmd,
			args:      []string{"CLCTAB23"},
			operation: "collections.delete",
		},
		{
			name:      "move",
			build:     newCollectionsMoveCmd,
			args:      []string{"CLCTAB23", "--to", "PRNTAB23"},
			operation: "collections.move",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A delete preview is only ok when apply would be allowed.
			flags := func(f rootFlags) *rootFlags {
				f.asJSON, f.maxChanges, f.allowDestructive = true, -1, true
				return &f
			}
			assertPreview := func(t *testing.T, got map[string]any, reason string) {
				t.Helper()
				if got["ok"] != true || got["mode"] != "preview" || got["operation"] != tc.operation || got["preview_reason"] != reason || got["result"] != nil {
					t.Errorf("preview envelope = %v, want an ok %s preview (reason %s) with a null result", got, tc.operation, reason)
				}
				if _, legacy := got["success"]; legacy {
					t.Errorf("preview envelope = %v carries the legacy success flag", got)
				}
			}

			t.Run("default previews", func(t *testing.T) {
				rec := newCollectionsWriteRecorder(t)
				got := runCollectionsWriteCmd(t, rec, tc.build(flags(rootFlags{})), tc.args...)
				assertPreview(t, got, "default")
				if len(rec.mutating) != 0 {
					t.Errorf("preview wrote to the library: %v", rec.mutating)
				}
				if tc.name != "create" && len(rec.requests) == 0 {
					t.Errorf("preview made no read; it must run the same reads as apply")
				}
			})

			t.Run("dry run previews", func(t *testing.T) {
				rec := newCollectionsWriteRecorder(t)
				got := runCollectionsWriteCmd(t, rec, tc.build(flags(rootFlags{dryRun: true})), tc.args...)
				assertPreview(t, got, "dry_run")
				if len(rec.mutating) != 0 {
					t.Errorf("preview wrote to the library under --dry-run: %v", rec.mutating)
				}
			})

			// --yes with --dry-run must still refuse: ResolveMode applies only
			// when yes is set and dry-run is not.
			t.Run("dry run beats yes", func(t *testing.T) {
				rec := newCollectionsWriteRecorder(t)
				runCollectionsWriteCmd(t, rec, tc.build(flags(rootFlags{yes: true, dryRun: true})), tc.args...)
				if len(rec.mutating) != 0 {
					t.Errorf("--dry-run did not override --yes: %v", rec.mutating)
				}
			})

			t.Run("yes applies", func(t *testing.T) {
				rec := newCollectionsWriteRecorder(t)
				got := runCollectionsWriteCmd(t, rec, tc.build(flags(rootFlags{yes: true})), tc.args...)
				if len(rec.mutating) != 1 {
					t.Errorf("mutating requests = %v, want exactly one once --yes is given", rec.mutating)
				}
				if got["ok"] != true || got["mode"] != "apply" || got["operation"] != tc.operation {
					t.Errorf("apply envelope = %v, want an ok %s apply", got, tc.operation)
				}
			})
		})
	}
}

// Required-flag and value checks run before the preview branch, so a preview
// never succeeds for input the apply refuses.
func TestCollectionsWriteValidationRunsBeforePreview(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(*rootFlags) *cobra.Command
		args  []string
	}{
		{"create without name", newCollectionsCreateCmd, nil},
		{"create with bad parent", newCollectionsCreateCmd, []string{"--name", "New", "--parent-collection", "bad"}},
		{"update without changes", newCollectionsUpdateCmd, []string{"CLCTAB23"}},
		{"update with bad parent", newCollectionsUpdateCmd, []string{"CLCTAB23", "--parent-collection", "bad"}},
		{"move without --to", newCollectionsMoveCmd, []string{"CLCTAB23"}},
		{"move with bad parent", newCollectionsMoveCmd, []string{"CLCTAB23", "--to", "bad"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, dryRun := range []bool{false, true} {
				rec := newCollectionsWriteRecorder(t)
				_, err := executeCollectionsWriteCmd(t, rec, tc.build(&rootFlags{asJSON: true, dryRun: dryRun, maxChanges: -1}), tc.args...)
				if err == nil || ExitCode(err) != 2 {
					t.Fatalf("dryRun=%v: error = %v, exit=%d; want usage error exit 2", dryRun, err, ExitCode(err))
				}
				if len(rec.requests) != 0 {
					t.Fatalf("dryRun=%v: requests = %v, want none", dryRun, rec.requests)
				}
			}
		})
	}
}

func TestCollectionsUpdateAppliesVersionPrecondition(t *testing.T) {
	rec := newCollectionsWriteRecorder(t)
	cmd := newCollectionsUpdateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})

	runCollectionsWriteCmd(t, rec, cmd, "CLCTAB23", "--name", "Renamed")

	if got, want := strings.Join(rec.requests, ","), "GET /users/0/collections/CLCTAB23,PUT /users/0/collections/CLCTAB23"; got != want {
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

	got, err := executeCollectionsWriteCmd(t, rec, cmd, "--stdin")
	if err == nil {
		t.Fatal("collections create succeeded above --max-changes")
	}
	// The refusal is rendered as the envelope, not lost to a bare error.
	if errObj, _ := got["error"].(map[string]any); got["ok"] != false || errObj["code"] != "max_changes_exceeded" {
		t.Fatalf("refusal output = %v, want an ok:false envelope with max_changes_exceeded", got)
	}
	// tags rename sends max_changes_exceeded through runMutation, which exits 1.
	if ExitCode(err) != 1 {
		t.Fatalf("ExitCode(%v) = %d, want tags rename-compatible exit 1", err, ExitCode(err))
	}
	if !strings.Contains(err.Error(), "planned 2 change(s), which exceeds the cap of 1") {
		t.Fatalf("error = %q, want max-changes gate message", err)
	}
	if len(rec.mutating) != 0 {
		t.Fatalf("mutating requests = %v, want none after gate refusal", rec.mutating)
	}
}

func TestCollectionsUpdateHonorsMaxChanges(t *testing.T) {
	rec := newCollectionsWriteRecorder(t)
	cmd := newCollectionsUpdateCmd(&rootFlags{asJSON: true, yes: true, maxChanges: 0})

	_, err := executeCollectionsWriteCmd(t, rec, cmd, "CLCTAB23", "--name", "Renamed")
	if err == nil {
		t.Fatal("collections update succeeded above --max-changes")
	}
	if ExitCode(err) != 1 {
		t.Fatalf("ExitCode(%v) = %d, want tags rename-compatible exit 1", err, ExitCode(err))
	}
	if !strings.Contains(err.Error(), "planned 1 change(s), which exceeds the cap of 0") {
		t.Fatalf("error = %q, want max-changes gate message", err)
	}
	if len(rec.mutating) != 0 {
		t.Fatalf("mutating requests = %v, want none after gate refusal", rec.mutating)
	}
}

func TestCollectionsDeleteRequiresDestructiveOptIn(t *testing.T) {
	t.Run("refuses without opt-in", func(t *testing.T) {
		rec := newCollectionsWriteRecorder(t)
		cmd := newCollectionsDeleteCmd(&rootFlags{asJSON: true, yes: true, maxChanges: -1})

		got, err := executeCollectionsWriteCmd(t, rec, cmd, "CLCTAB23")
		if err == nil {
			t.Fatal("collections delete succeeded without --allow-destructive")
		}
		if errObj, _ := got["error"].(map[string]any); got["ok"] != false || errObj["code"] != "destructive_opt_in_required" {
			t.Fatalf("refusal output = %v, want an ok:false envelope with destructive_opt_in_required", got)
		}
		if ExitCode(err) != 1 {
			t.Fatalf("ExitCode(%v) = %d, want tags rename-compatible exit 1", err, ExitCode(err))
		}
		if !strings.Contains(err.Error(), "destructive changes require --allow-destructive") {
			t.Fatalf("error = %q, want destructive opt-in message", err)
		}
		if len(rec.mutating) != 0 {
			t.Fatalf("mutating requests = %v, want none after destructive gate refusal", rec.mutating)
		}
	})

	t.Run("applies with opt-in", func(t *testing.T) {
		rec := newCollectionsWriteRecorder(t)
		cmd := newCollectionsDeleteCmd(&rootFlags{
			asJSON: true, yes: true, maxChanges: -1, allowDestructive: true,
		})

		runCollectionsWriteCmd(t, rec, cmd, "CLCTAB23")
		if len(rec.mutating) != 1 || rec.mutating[0] != "DELETE /users/0/collections/CLCTAB23" {
			t.Fatalf("mutating requests = %v, want DELETE /users/0/collections/CLCTAB23", rec.mutating)
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
