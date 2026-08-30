// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildAgentDiscoveryContext(t *testing.T) {
	d := buildAgentDiscoveryContext()
	if d == nil {
		t.Fatal("expected non-nil discovery")
	}
	if d.Source != "which" {
		t.Fatalf("source = %q, want which", d.Source)
	}
	if d.EntryCount != len(whichIndex) {
		t.Fatalf("entry_count = %d, want %d", d.EntryCount, len(whichIndex))
	}
	if len(d.CandidateCommands) != len(whichIndex) {
		t.Fatalf("candidate_commands len = %d, want %d", len(d.CandidateCommands), len(whichIndex))
	}
}

func runAgentContextTestCmd(t *testing.T, args ...string) string {
	t.Helper()
	root := newRootCmd(&rootFlags{})
	cmd := newAgentContextCmd(root)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("agent-context %v: %v; output=%s", args, err, out.String())
	}
	return out.String()
}

// AGENTS.md names `zotio agent-context --pretty` as the runtime introspection
// entry point for agents, so its envelope is a published contract. Execute the
// command (not just buildAgentContext) so a payload that never reaches the Cobra
// writer fails here: agent-context used to encode straight to os.Stdout, which
// escaped output capture and the --deliver-spool tee.
func TestAgentContextCmdEmitsTheVersionedEnvelope(t *testing.T) {
	for _, name := range []string{"compact", "pretty"} {
		t.Run(name, func(t *testing.T) {
			var args []string
			if name == "pretty" {
				args = []string{"--pretty"}
			}
			out := runAgentContextTestCmd(t, args...)
			if out == "" {
				t.Fatal("agent-context produced no output on the Cobra writer")
			}

			var got agentContext
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("payload is not JSON: %v; output=%s", err, out)
			}
			if got.SchemaVersion != agentContextSchemaVersion {
				t.Fatalf("schema_version = %q, want %q", got.SchemaVersion, agentContextSchemaVersion)
			}
			if got.CLI.Name != "zotio" {
				t.Fatalf("cli.name = %q, want zotio", got.CLI.Name)
			}
			if got.Auth.Mode != "api_key" || len(got.Auth.EnvVars) == 0 {
				t.Fatalf("auth = %+v, want api_key mode with env vars", got.Auth)
			}
			if len(got.Commands) == 0 {
				t.Fatal("commands is empty; the cobra tree was not collected")
			}
			if got.AvailableProfiles == nil {
				t.Fatal("available_profiles is null; agents parse it as a list")
			}

			// --pretty is the documented flag, so its only observable effect must hold.
			indented := strings.Contains(out, "\n  \"cli\": {")
			if want := name == "pretty"; indented != want {
				t.Fatalf("indented = %v, want %v; output=%s", indented, want, out)
			}
		})
	}
}

// collectAgentCommands skips hidden commands and agent-context itself, and sorts
// by name. All three are contract: agents diff the payload across releases, and a
// self-referencing tree would recurse.
func TestCollectAgentCommandsSkipsHiddenAndSelfAndSorts(t *testing.T) {
	out := runAgentContextTestCmd(t)
	var got agentContext
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}

	names := make([]string, 0, len(got.Commands))
	for _, c := range got.Commands {
		names = append(names, c.Name)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("commands not sorted ascending at %d: %v", i, names)
		}
	}

	root := newRootCmd(&rootFlags{})
	hidden := map[string]bool{}
	for _, c := range root.Commands() {
		if c.Hidden {
			hidden[c.Name()] = true
		}
	}
	for _, name := range names {
		if name == "agent-context" {
			t.Fatal("agent-context collected itself; the tree self-references")
		}
		if hidden[name] {
			t.Fatalf("hidden command %q leaked into the payload", name)
		}
	}
}
