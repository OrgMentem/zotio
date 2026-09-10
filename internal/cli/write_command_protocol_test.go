// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// One write protocol, asserted once over every command that implements it:
// plan without --yes, touch no network under --dry-run, and refuse to write
// when the write plane reports no version to precondition on.
//
// These are properties of the write-safety contract, not of any one command,
// so a command joins by adding a row here. Each command used to carry its own
// copy of all three; a command added with only some of them still looked
// tested, because the copies it lacked were named after the other commands.

package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zotio/internal/client"
	"zotio/internal/config"
	"zotio/internal/mutation"
)

// writeCommandCase is one command's binding to the shared protocol.
type writeCommandCase struct {
	name string
	// newServer builds the K1 fixture whose write plane reports version.
	newServer func(t *testing.T, version string) *writePlaneTestServer
	// run executes the command. A mutation-incomplete error is tolerated only
	// when the envelope carries a Result for the caller to assert against.
	run func(t *testing.T, srv *writePlaneTestServer, flags *rootFlags, args ...string) mutation.Envelope
	// argSets each plan exactly one change against item K1.
	argSets []writeCommandArgs
}

// writeCommandArgs is one invocation plus the plan detail that only that
// invocation can witness.
type writeCommandArgs struct {
	args []string
	// wantTagType, when non-zero, is the tag type the plan must carry. It
	// exists because a dry run PLANS the change without writing it, so the
	// automatic-tag flag is only observable in the plan on this path; the
	// apply-mode tests assert it from the PATCH body instead.
	wantTagType int
}

func writeCommandCases() []writeCommandCase {
	return []writeCommandCase{
		{
			name: "items move",
			newServer: func(t *testing.T, version string) *writePlaneTestServer {
				return writePlaneTestNewItemServer(t, "collections",
					map[string]string{"K1": version},
					map[string][]string{"K1": {"SOURCE"}})
			},
			run: func(t *testing.T, _ *writePlaneTestServer, flags *rootFlags, args ...string) mutation.Envelope {
				t.Helper()
				env, stderr, err := writePlaneTestRunMutationCmd(t, newItemsMoveCmd, flags, args...)
				if err != nil && env.Result == nil {
					t.Fatalf("items move %v: %v; stderr=%s", args, err, stderr)
				}
				return env
			},
			argSets: []writeCommandArgs{{args: []string{"--to", "TARGET", "K1"}}},
		},
		{
			name: "items tags",
			newServer: func(t *testing.T, version string) *writePlaneTestServer {
				return writePlaneTestNewItemServer(t, "tags",
					map[string]string{"K1": version},
					map[string][]map[string]any{"K1": {{"tag": "existing", "type": float64(0)}}})
			},
			run: func(t *testing.T, srv *writePlaneTestServer, flags *rootFlags, args ...string) mutation.Envelope {
				t.Helper()
				env, _ := runItemsTagsTestCmd(t, srv, flags, args...)
				return env
			},
			// Add and remove reach the write plane by different plan paths.
			argSets: []writeCommandArgs{
				{args: []string{"add", "--tag", "fresh", "K1"}},
				{args: []string{"add", "--automatic", "--tag", "fresh", "K1"}, wantTagType: 1},
				{args: []string{"remove", "--tag", "existing", "K1"}},
			},
		},
	}
}

// Without --yes a write command plans and stops. The plan is still complete,
// so the planned count is asserted beside the absence of any write.
func TestWriteCommandsPreviewWriteNothing(t *testing.T) {
	for _, tc := range writeCommandCases() {
		for _, set := range tc.argSets {
			t.Run(tc.name+" "+strings.Join(set.args, " "), func(t *testing.T) {
				srv := tc.newServer(t, "42")
				env := tc.run(t, srv, &rootFlags{asJSON: true, maxChanges: -1}, set.args...)
				if !env.OK || env.Mode != "preview" || env.Result != nil || env.Plan.Summary.Planned != 1 {
					t.Fatalf("env = %+v, want preview plan with one change", env)
				}
				if srv.patchCounts["K1"] != 0 {
					t.Fatalf("PATCH count = %d, want 0", srv.patchCounts["K1"])
				}
			})
		}
	}
}

// --dry-run must not reach the network at all, not even for the version read
// that a real write needs. A dry run that fetches versions is a dry run that
// fails when the API is down, which is when people reach for it.
func TestWriteCommandsDryRunTouchesNoNetwork(t *testing.T) {
	for _, tc := range writeCommandCases() {
		for _, set := range tc.argSets {
			t.Run(tc.name+" "+strings.Join(set.args, " "), func(t *testing.T) {
				srv := tc.newServer(t, "42")
				env := tc.run(t, srv, &rootFlags{asJSON: true, dryRun: true, maxChanges: -1}, set.args...)
				if !env.OK || env.Mode != "preview" || env.PreviewReason != "dry_run" || env.Result != nil || env.Plan.Summary.Planned != 1 {
					t.Fatalf("env = %+v, want dry-run preview with one planned change", env)
				}
				if set.wantTagType != 0 && env.Plan.Operations[0].Changes[0].TagType != set.wantTagType {
					t.Fatalf("planned tag type = %d, want %d", env.Plan.Operations[0].Changes[0].TagType, set.wantTagType)
				}
				if srv.getCounts["K1"] != 0 || srv.patchCounts["K1"] != 0 {
					t.Fatalf("requests: GET=%d PATCH=%d, want none", srv.getCounts["K1"], srv.patchCounts["K1"])
				}
			})
		}
	}
}

// A write plane that reports version 0 gives nothing to precondition on.
// Zotero answers a preconditionless key-based write with an opaque 428, and a
// permissive server would overwrite a concurrent edit with no conflict
// detection, so the command must fail before the request leaves.
func TestWriteCommandsFailClosedOnZeroVersion(t *testing.T) {
	for _, tc := range writeCommandCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.newServer(t, "0")
			env := tc.run(t, srv, &rootFlags{asJSON: true, yes: true, maxChanges: -1}, tc.argSets[0].args...)
			if env.Result == nil || len(env.Result.Items) != 1 {
				t.Fatalf("env = %+v, want one result", env)
			}
			if env.Result.Items[0].Status != "failed" {
				t.Fatalf("status = %q, want failed (zero version must fail closed)", env.Result.Items[0].Status)
			}
			if srv.patchCounts["K1"] != 0 {
				t.Fatalf("PATCH count = %d, want 0 (no request when version is 0)", srv.patchCounts["K1"])
			}
		})
	}
}

// The patch helpers are the last gate before the wire. Every write command
// funnels through one of them, so they refuse an absent version themselves
// rather than trusting each caller to have checked.
func TestWritePlanePatchHelpersRefuseAbsentVersion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch func(c *client.Client, path string) (string, any, error)
	}{
		{
			name: "patchItemCollections",
			patch: func(c *client.Client, path string) (string, any, error) {
				return patchItemCollections(c, path, 0, []string{"TARGET"})
			},
		},
		{
			name: "patchItemTags",
			patch: func(c *client.Client, path string) (string, any, error) {
				return patchItemTags(c, path, 0, []map[string]any{{"tag": "fresh"}})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("%s dispatched %s %s with no precondition to send", tc.name, r.Method, r.URL.Path)
				http.Error(w, "unexpected request", http.StatusBadRequest)
			}))
			t.Cleanup(srv.Close)
			c := client.New(&config.Config{BaseURL: srv.URL + "/users/0"}, 0, 0)
			c.NoCache = true

			status, reason, err := tc.patch(c, "/users/0/items/K1")
			if err == nil {
				t.Fatalf("%s with version 0: err = nil, want error", tc.name)
			}
			if status != "failed" {
				t.Fatalf("status = %q, want failed", status)
			}
			msg, _ := reason.(string)
			if !strings.Contains(strings.ToLower(msg), "write-plane version") && !strings.Contains(strings.ToLower(msg), "if-unmodified-since-version") {
				t.Fatalf("reason = %q, want missing write-plane precondition", msg)
			}
		})
	}
}
