// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
			// Pin the metadata, not just its presence: an agent that gets the wrong
			// variable name or loses the sensitive flag has unusable credential
			// guidance, and a length check accepts exactly that regression.
			// The key is optional: local desktop reads need no key, and only
			// operations whose capability entry requires web_api_key need one
			// (same contract as the MCP domain context). Claiming required:true
			// told agents the CLI is unusable without a key.
			wantEnv := []agentContextAuthEnvVar{{
				Name:        "ZOTERO_API_KEY",
				Kind:        "per_call",
				Required:    false,
				Sensitive:   true,
				Description: "Only for the Zotero web API (group libraries or while the desktop app is closed); local desktop at localhost:23119 needs no key.",
			}}
			if got.Auth.Mode != "api_key" || !reflect.DeepEqual(got.Auth.EnvVars, wantEnv) {
				t.Fatalf("auth = %+v, want api_key mode with %+v", got.Auth, wantEnv)
			}
			if len(got.Commands) == 0 {
				t.Fatal("commands is empty; the cobra tree was not collected")
			}
			if got.AvailableProfiles == nil {
				t.Fatal("available_profiles is null; agents parse it as a list")
			}

			// Assert indentation STRUCTURALLY. Matching a literal like
			// "\n  \"cli\": {" would pin the field order too, so a harmless
			// reordering of the struct would fail a test about whitespace.
			lines := strings.Count(strings.TrimSuffix(out, "\n"), "\n") + 1
			if name == "pretty" && lines < 2 {
				t.Fatalf("--pretty produced %d line(s), want an indented document; output=%s", lines, out)
			}
			if name == "compact" && lines != 1 {
				t.Fatalf("compact produced %d lines, want exactly 1; output=%s", lines, out)
			}
		})
	}
}

// collectAgentCommands skips hidden commands, skips agent-context itself, and
// sorts by name at EVERY level. Drive it with a synthetic tree, not the real one:
// the repo currently declares no hidden cobra command, so a real-tree assertion
// is vacuous and would survive deleting the Hidden filter outright.
func TestCollectAgentCommandsSkipsHiddenAndSelfAndSortsRecursively(t *testing.T) {
	// cobra.Commands() sorts by name on its own while EnableCommandSorting is
	// true, which makes the sort in collectAgentCommands unobservable: deleting it
	// changes nothing and any ordering assertion passes for the wrong reason.
	// Turn the global off so the production sort is the ONLY thing that can order
	// the payload. It is a package global, so restore it; no test in this package
	// that runs in parallel touches a cobra tree.
	cobra.EnableCommandSorting = false
	t.Cleanup(func() { cobra.EnableCommandSorting = true })

	newLeaf := func(name string) *cobra.Command {
		return &cobra.Command{Use: name, Run: func(*cobra.Command, []string) {}}
	}

	parent := newLeaf("parent")
	// Deliberately out of order, and with a hidden child one level down.
	parent.AddCommand(newLeaf("zeta"), newLeaf("alpha"))
	nestedHidden := newLeaf("nested-hidden")
	nestedHidden.Hidden = true
	parent.AddCommand(nestedHidden)

	topHidden := newLeaf("top-hidden")
	topHidden.Hidden = true

	root := &cobra.Command{Use: "zotio"}
	root.AddCommand(newLeaf("zulu"), newLeaf("bravo"), topHidden, newLeaf("agent-context"), parent)

	got := collectAgentCommands(root)

	var walk func(level string, cmds []agentContextCommand)
	walk = func(level string, cmds []agentContextCommand) {
		for i, c := range cmds {
			if c.Name == "agent-context" {
				t.Fatalf("%s: agent-context collected itself; the tree self-references", level)
			}
			if c.Name == "top-hidden" || c.Name == "nested-hidden" {
				t.Fatalf("%s: hidden command %q leaked into the payload", level, c.Name)
			}
			if i > 0 && cmds[i-1].Name >= c.Name {
				t.Fatalf("%s: not sorted ascending at %d: %q then %q", level, i, cmds[i-1].Name, c.Name)
			}
			walk(level+"/"+c.Name, c.Subcommands)
		}
	}
	walk("root", got)

	// Guard the fixture itself: if nothing survived the filters the walk above
	// asserts nothing, and if the recursion never descends the nested cases are
	// unclaimed.
	if want := []string{"bravo", "parent", "zulu"}; len(got) != len(want) {
		t.Fatalf("top level = %+v, want exactly %v", got, want)
	}
	for i, c := range got {
		if c.Name != []string{"bravo", "parent", "zulu"}[i] {
			t.Fatalf("top level = %+v, want bravo, parent, zulu", got)
		}
	}
	if len(got[1].Subcommands) != 2 {
		t.Fatalf("parent subcommands = %+v, want alpha and zeta only", got[1].Subcommands)
	}
}

func agentContextFind(t *testing.T, cmds []agentContextCommand, path ...string) agentContextCommand {
	t.Helper()
	for _, c := range cmds {
		if c.Name != path[0] {
			continue
		}
		if len(path) == 1 {
			return c
		}
		return agentContextFind(t, c.Subcommands, path[1:]...)
	}
	t.Fatalf("agent-context has no command %v", path)
	return agentContextCommand{}
}

// The zotio:* safety annotations come from the capability registry, never from
// a literal on the command, so agent-context cannot contradict `capabilities`.
func TestAgentContextAnnotationsComeFromTheRegistry(t *testing.T) {
	ctx := buildAgentContext(RootCmd())
	check := func(path []string, want map[string]string) {
		t.Helper()
		cmd := agentContextFind(t, ctx.Commands, path...)
		for key, value := range want {
			if got := cmd.Annotations[key]; got != value {
				t.Errorf("%v annotation %s = %q, want %q", path, key, got, value)
			}
		}
	}
	check([]string{"items", "add-to-collection"}, map[string]string{
		"zotio:supports-dry-run":           "true",
		"zotio:destructive":                "false",
		"zotio:requires-allow-destructive": "false",
		"zotio:default-max-changes":        "500",
		"zotio:default-max-changes-agent":  "50",
	})
	check([]string{"items", "create"}, map[string]string{
		"zotio:supports-dry-run": "true",
		"mcp:effect":             "write",
	})
	check([]string{"items", "delete"}, map[string]string{
		"zotio:destructive":                "true",
		"zotio:requires-allow-destructive": "true",
	})
	check([]string{"creators", "rename"}, map[string]string{"zotio:destructive": "true"})
	check([]string{"workflow", "run"}, map[string]string{"zotio:destructive": "true"})
	check([]string{"init"}, map[string]string{"zotio:supports-dry-run": "false"})
	check([]string{"library", "wrapped"}, map[string]string{
		"zotio:supports-dry-run": "true",
		"mcp:effect":             "local-file",
	})
	check([]string{"library", "stats"}, map[string]string{"mcp:effect": "read"})

	// Every write command is covered, and the registry says so for each.
	registry := registryByPath(ctx.Capabilities)
	var walk func(prefix []string, cmds []agentContextCommand)
	walk = func(prefix []string, cmds []agentContextCommand) {
		for _, c := range cmds {
			path := append(append([]string{}, prefix...), c.Name)
			if entry, ok := registry[strings.Join(path, " ")]; ok && entry.Operation == "write" {
				if c.Annotations["zotio:supports-dry-run"] == "" || c.Annotations["zotio:default-max-changes"] == "" {
					t.Errorf("write command %v lacks the registry-derived annotations: %v", path, c.Annotations)
				}
			}
			walk(path, c.Subcommands)
		}
	}
	walk(nil, ctx.Commands)
}

func TestAgentContextPublishesExitCodesAndGlobalFlags(t *testing.T) {
	ctx := buildAgentContext(RootCmd())

	kinds := map[int]string{}
	meanings := map[int]string{}
	for _, row := range ctx.ExitCodes {
		if row.Meaning == "" {
			t.Errorf("exit code %d has no meaning", row.Code)
		}
		kinds[row.Code] = row.Kind
		meanings[row.Code] = row.Meaning
	}
	// ADR-0008: a conflict exits 1, so exit 13 must not claim predicted conflicts.
	if want := "Partial: part of a write applied and part failed for a non-conflict reason, or part of a read was unreadable."; meanings[13] != want {
		t.Errorf("exit code 13 meaning = %q, want %q", meanings[13], want)
	}
	for code, kind := range map[int]string{0: "ok", 2: "usage", 3: "not_found", 9: "precondition_unmet", 11: "gate", 13: "degraded"} {
		if kinds[code] != kind {
			t.Errorf("exit code %d kind = %q, want %q", code, kinds[code], kind)
		}
	}

	applies := map[string]string{}
	for _, f := range ctx.GlobalFlags {
		applies[f.Name] = f.AppliesTo
	}
	for name, want := range map[string]string{"json": "all", "dry-run": "writes", "yes": "writes", "data-source": "reads-with-data-source"} {
		if applies[name] != want {
			t.Errorf("global flag --%s applies_to = %q, want %q", name, applies[name], want)
		}
	}

	if len(ctx.AnnotationSemantics["mcp:read-only"]) == 0 {
		t.Error("annotation_semantics does not explain the three-state mcp:read-only annotation")
	}
}

func TestAgentContextFlagsCarryRequiredAndTypedExitCodes(t *testing.T) {
	ctx := buildAgentContext(RootCmd())

	bundle := agentContextFind(t, ctx.Commands, "collections", "bundle")
	required := false
	for _, f := range bundle.Flags {
		if f.Name == "output-dir" {
			required = f.Required
		}
		if f.Name == "yes" || f.Name == "json" {
			t.Errorf("collections bundle lists global flag --%s as a command flag; globals belong in global_flags", f.Name)
		}
	}
	if !required {
		t.Error("collections bundle --output-dir is required but agent-context does not say so")
	}

	which := agentContextFind(t, ctx.Commands, "which")
	if len(which.TypedExitCodes) != 2 || which.TypedExitCodes[0] != 0 || which.TypedExitCodes[1] != 2 {
		t.Errorf("which typed_exit_codes = %v, want [0 2]", which.TypedExitCodes)
	}
	if _, dup := which.Annotations[typedExitCodesAnnotation]; dup {
		t.Error("which publishes the typed exit codes both as typed_exit_codes and as an annotation")
	}
}
