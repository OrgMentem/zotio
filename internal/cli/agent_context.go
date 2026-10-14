// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// agentContextSchemaVersion is bumped on any breaking change to the JSON
// shape emitted by `agent-context`. Agents should check this before
// parsing. Shape at v3 adds kind-aware auth env var metadata. Shape at v4
// derives the zotio:* command annotations from the capability registry and
// adds annotation_semantics, exit_codes, global_flags, typed_exit_codes, and
// per-flag shorthand and required.
const agentContextSchemaVersion = "4"

// agentContext is the structured description of this CLI consumed by AI
// agents. Inspired by Cloudflare's /cdn-cgi/explorer/api runtime endpoint
// (2026-04-13 Wrangler post): agents can introspect the live CLI without
// parsing --help or reading source.
type agentContext struct {
	SchemaVersion       string                   `json:"schema_version"`
	CLI                 agentContextCLI          `json:"cli"`
	Auth                agentContextAuth         `json:"auth"`
	AnnotationSemantics map[string]string        `json:"annotation_semantics"`
	ExitCodes           []agentContextExitCode   `json:"exit_codes"`
	GlobalFlags         []agentContextGlobalFlag `json:"global_flags"`
	Discovery           *agentContextDiscovery   `json:"discovery,omitempty"`
	Commands            []agentContextCommand    `json:"commands"`
	// typed capability + preconditions registry.
	Capabilities               []capabilityEntry `json:"capabilities,omitempty"`
	AvailableProfiles          []string          `json:"available_profiles"`
	FeedbackEndpointConfigured bool              `json:"feedback_endpoint_configured"`
}

type agentContextCLI struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
}

type agentContextAuth struct {
	Mode    string                   `json:"mode"`
	EnvVars []agentContextAuthEnvVar `json:"env_vars"`
}

type agentContextAuthEnvVar struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Required    bool   `json:"required"`
	Sensitive   bool   `json:"sensitive"`
	Description string `json:"description,omitempty"`
}

type agentContextDiscovery struct {
	Source            string   `json:"source"`
	TargetURL         string   `json:"target_url,omitempty"`
	EntryCount        int      `json:"entry_count,omitempty"`
	APIEntryCount     int      `json:"api_entry_count,omitempty"`
	Reachability      string   `json:"reachability,omitempty"`
	Protocols         []string `json:"protocols,omitempty"`
	AuthCandidates    []string `json:"auth_candidates,omitempty"`
	Protections       []string `json:"protections,omitempty"`
	GenerationHints   []string `json:"generation_hints,omitempty"`
	Warnings          []string `json:"warnings,omitempty"`
	CandidateCommands []string `json:"candidate_commands,omitempty"`
}

type agentContextCommand struct {
	Name           string                `json:"name"`
	Use            string                `json:"use,omitempty"`
	Short          string                `json:"short,omitempty"`
	Annotations    map[string]string     `json:"annotations,omitempty"`
	TypedExitCodes []int                 `json:"typed_exit_codes,omitempty"`
	Flags          []agentContextFlag    `json:"flags,omitempty"`
	Subcommands    []agentContextCommand `json:"subcommands,omitempty"`
}

type agentContextFlag struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Usage     string `json:"usage,omitempty"`
	Default   string `json:"default,omitempty"`
	Required  bool   `json:"required,omitempty"`
}

// agentContextExitCode is one row of the exit-code contract.
type agentContextExitCode struct {
	Code    int    `json:"code"`
	Kind    string `json:"kind"`
	Meaning string `json:"meaning"`
}

// agentContextGlobalFlag describes one root persistent flag once, instead of
// repeating it on every command. AppliesTo is "all" (any command),
// "writes" (commands whose operation is write, plus local file writers for
// --dry-run), or "reads-with-data-source" (read commands that can use the
// live API or the local mirror).
type agentContextGlobalFlag struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Usage     string `json:"usage,omitempty"`
	Default   string `json:"default,omitempty"`
	AppliesTo string `json:"applies_to"`
}

// agentContextAnnotationSemantics explains the command annotations agent-context
// publishes. mcp:read-only is a three-state annotation, so absence must not be
// read as "mutating"; mcp:effect and the capabilities registry are the
// unambiguous source.
var agentContextAnnotationSemantics = map[string]string{
	"mcp:read-only":                    "Three states. \"true\": the command does not change the library or durable state the user did not name (it may still write a file the user names; see mcp:writes-files). \"false\": the command changes the library or local installation state. Absent: the annotation does not classify the command; read mcp:effect or capabilities[].operation instead. Absent does not mean mutating.",
	"mcp:writes-files":                 "The command may create, replace, or delete local files, such as an output path, a baseline, or the demo sandbox. The registry reports write_target local_file and --dry-run previews the write.",
	"mcp:effect":                       "Derived from the capability registry. write: changes the library or local installation state. local-file: writes a file the user names. local-state: changes only the local mirror. read: changes nothing. other: a local UI action.",
	"mcp:hidden":                       "The command is not exposed over MCP.",
	"zotio:supports-dry-run":           "Derived from the registry. true for every write command and every local file writer; false for init, sync, and watch, which reject --dry-run.",
	"zotio:destructive":                "Derived from the registry. true when the change cannot be undone by `journal undo`: permanent delete, collections delete, duplicates resolve merge, creators rename, creators audit fix, vault resolve.",
	"zotio:requires-allow-destructive": "Derived from the registry. Equals zotio:destructive.",
	"zotio:default-max-changes":        "Derived from the registry. The --max-changes cap for a plain run. A run under --agent uses zotio:default-max-changes-agent.",
	"zotio:default-max-changes-agent":  "Derived from the registry. The --max-changes cap under --agent.",
	"zotio:preflight":                  "skip: the command checks its own preconditions instead of the central preflight.",
}

// agentContextExitCodes publishes the exit-code contract from the constants in
// helpers.go. Kinds match the "kind" field of the JSON error object.
func agentContextExitCodes() []agentContextExitCode {
	rows := []struct {
		code    int
		kind    string
		meaning string
	}{
		{0, "ok", "Success."},
		{1, "general", "Unclassified failure."},
		{2, "", "Usage error: bad flag value, bad argument count, invalid key, mutually exclusive flags."},
		{3, "", "Not found: the key or name does not exist."},
		{4, "", "Authentication failed or no Web API key."},
		{5, "", "Upstream error: the Zotero API or a provider failed."},
		{7, "", "Rate limited."},
		{9, "", "Precondition unmet: unsynced store, Zotero unreachable when required, or another writer holds the lock. Fix the environment and retry."},
		{10, "", "Configuration error."},
		{11, "", "Quality gate tripped (--fail-on)."},
		{12, "", "Freshness gate tripped (--require-fresh). Run sync and retry."},
		{13, "", "Partial: part of a write applied and part failed for a non-conflict reason, or part of a read was unreadable."},
		{14, "", "Timed out: a bounded wait ended before the condition held. Wait again."},
		{15, "stuck", "Zotero is open but stuck: its connector cannot take requests."},
	}
	out := make([]agentContextExitCode, 0, len(rows))
	for _, r := range rows {
		kind := r.kind
		if kind == "" {
			kind = errorKindForCode(r.code)
		}
		out = append(out, agentContextExitCode{Code: r.code, Kind: kind, Meaning: r.meaning})
	}
	return out
}

// globalFlagAppliesTo classifies the root persistent flags by the commands they
// affect. A flag not listed applies to all commands.
var globalFlagAppliesTo = map[string]string{
	"dry-run":            "writes",
	"yes":                "writes",
	"max-changes":        "writes",
	"allow-destructive":  "writes",
	"allow-zotero-cloud": "writes",
	"continue-on-error":  "writes",
	"max-failures":       "writes",
	"idempotent":         "writes",
	"ignore-missing":     "writes",
	"via":                "writes",
	"connector-target":   "writes",
	"data-source":        "reads-with-data-source",
	"no-cache":           "reads-with-data-source",
}

func agentContextGlobalFlags(rootCmd *cobra.Command) []agentContextGlobalFlag {
	out := []agentContextGlobalFlag{}
	rootCmd.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		appliesTo := globalFlagAppliesTo[f.Name]
		if appliesTo == "" {
			appliesTo = "all"
		}
		out = append(out, agentContextGlobalFlag{
			Name:      f.Name,
			Shorthand: f.Shorthand,
			Type:      f.Value.Type(),
			Usage:     f.Usage,
			Default:   f.DefValue,
			AppliesTo: appliesTo,
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func registryByPath(entries []capabilityEntry) map[string]capabilityEntry {
	out := make(map[string]capabilityEntry, len(entries))
	for _, entry := range entries {
		out[entry.Path] = entry
	}
	return out
}

func newAgentContextCmd(rootCmd *cobra.Command) *cobra.Command {
	var pretty bool
	cmd := &cobra.Command{
		Use:         "agent-context",
		Short:       "Emit structured JSON describing this CLI for agents",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Long: `Outputs a machine-readable description of commands, flags, and auth so
agents can introspect this CLI at runtime without parsing --help or
reading source. Schema is versioned via schema_version.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := buildAgentContext(rootCmd)
			// cmd.OutOrStdout(), not os.Stdout: every other command writes through
			// the Cobra writer, which is what --deliver-spool tees and what
			// in-process callers (tests, capture wrappers) redirect. Writing the
			// payload straight to the process stdout escaped all of them.
			enc := json.NewEncoder(cmd.OutOrStdout())
			if pretty {
				enc.SetIndent("", "  ")
			}
			return enc.Encode(ctx)
		},
	}
	cmd.Flags().BoolVar(&pretty, "pretty", false, "indent JSON output for human reading")
	return cmd
}

func buildAgentContext(rootCmd *cobra.Command) agentContext {
	// The key is optional for local reads and required only for operations
	// whose capability entry needs Web API access. This mirrors the MCP
	// domain context (internal/mcp/resources.go domainContext), which
	// advertises the same key as required:false: local desktop reads need
	// no key. Marking it required here used to tell agents the CLI is
	// unusable without one, which is wrong for the primary local path.
	envVars := []agentContextAuthEnvVar{
		{
			Name:        "ZOTERO_API_KEY",
			Kind:        "per_call",
			Required:    false,
			Sensitive:   true,
			Description: "Only for the Zotero web API (group libraries or while the desktop app is closed); local desktop at localhost:23119 needs no key.",
		},
	}
	authMode := "api_key"
	if authMode == "" {
		authMode = "none"
	}
	profiles := ListProfileNames()
	if profiles == nil {
		profiles = []string{}
	}
	registry := buildCapabilityRegistry(rootCmd)
	return agentContext{
		SchemaVersion:       agentContextSchemaVersion,
		AnnotationSemantics: agentContextAnnotationSemantics,
		ExitCodes:           agentContextExitCodes(),
		GlobalFlags:         agentContextGlobalFlags(rootCmd),
		CLI: agentContextCLI{
			Name:        "zotio",
			Description: "The trust-and-automation layer for Zotero: local-first search and auditing, preview-first writes, and bounded context for humans, scripts, and MCP agents.",
			Version:     rootCmd.Version,
		},
		Auth: agentContextAuth{
			Mode:    authMode,
			EnvVars: envVars,
		},
		Discovery:                  buildAgentDiscoveryContext(),
		Commands:                   collectAgentCommandsIn(rootCmd, rootCmd, registryByPath(registry)),
		Capabilities:               registry,
		AvailableProfiles:          profiles,
		FeedbackEndpointConfigured: FeedbackEndpointConfigured(),
	}
}

// surface the curated which-index as agent discovery hints.
func buildAgentDiscoveryContext() *agentContextDiscovery {
	cmds := make([]string, 0, len(whichIndex))
	for _, e := range whichIndex {
		cmds = append(cmds, e.Command)
	}
	return &agentContextDiscovery{
		Source:            "which",
		EntryCount:        len(whichIndex),
		CandidateCommands: cmds,
	}
}

// collectAgentCommands walks the cobra tree from the given command and
// returns its direct children (skipping hidden commands and the
// agent-context command itself to avoid self-reference). Each child is
// recursed into if it has subcommands. Flags are captured via VisitAll.
// Output is sorted by command name for stable diffs across regenerations.
func collectAgentCommands(c *cobra.Command) []agentContextCommand {
	return collectAgentCommandsIn(c, c.Root(), registryByPath(buildCapabilityRegistry(c.Root())))
}

// registryDerivedAnnotations are the annotation keys agent-context takes only
// from the capability registry. A literal value a command carries for one of
// them is ignored, so the registry stays the single source and no command can
// publish a claim that contradicts it.
var registryDerivedAnnotations = []string{
	"zotio:supports-dry-run",
	"zotio:destructive",
	"zotio:requires-allow-destructive",
	"zotio:default-max-changes",
	"zotio:default-max-changes-agent",
	"mcp:effect",
}

// typedExitCodesAnnotation lists the exit codes a command's contract names,
// comma separated. agent-context publishes it as typed_exit_codes.
const typedExitCodesAnnotation = "zotio:typed-exit-codes"

func collectAgentCommandsIn(c, root *cobra.Command, registry map[string]capabilityEntry) []agentContextCommand {
	children := c.Commands()
	sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })

	out := make([]agentContextCommand, 0, len(children))
	for _, sub := range children {
		if sub.Hidden || sub.Name() == "agent-context" {
			continue
		}
		entry := agentContextCommand{
			Name:  sub.Name(),
			Use:   sub.Use,
			Short: sub.Short,
		}
		path := strings.TrimPrefix(sub.CommandPath(), root.Name()+" ")
		regEntry, inRegistry := registry[path]
		// Surface Cobra annotations (e.g., zotio:endpoint, mcp:read-only) so
		// agents and tooling can detect destructive-at-auth
		// endpoints without parsing source. Empty maps are stripped via
		// omitempty in the struct tag.
		if len(sub.Annotations) > 0 {
			entry.Annotations = make(map[string]string, len(sub.Annotations))
			for k, v := range sub.Annotations {
				entry.Annotations[k] = v
			}
		}
		if codes := parseTypedExitCodes(sub.Annotations[typedExitCodesAnnotation]); len(codes) > 0 {
			entry.TypedExitCodes = codes
			delete(entry.Annotations, typedExitCodesAnnotation)
		}
		for _, key := range registryDerivedAnnotations {
			delete(entry.Annotations, key)
		}
		if inRegistry {
			derived := registryAnnotations(regEntry, commandWritesFiles(sub))
			if effect := registryEffect(regEntry, commandWritesFiles(sub)); effect != "" {
				if derived == nil {
					derived = map[string]string{}
				}
				derived["mcp:effect"] = effect
			}
			if len(derived) > 0 && entry.Annotations == nil {
				entry.Annotations = make(map[string]string, len(derived))
			}
			for k, v := range derived {
				entry.Annotations[k] = v
			}
		}
		if len(entry.Annotations) == 0 {
			entry.Annotations = nil
		}
		sub.LocalFlags().VisitAll(func(f *pflag.Flag) {
			entry.Flags = append(entry.Flags, agentContextFlag{
				Name:      f.Name,
				Shorthand: f.Shorthand,
				Type:      f.Value.Type(),
				Usage:     f.Usage,
				Default:   f.DefValue,
				Required:  len(f.Annotations[cobra.BashCompOneRequiredFlag]) > 0 && f.Annotations[cobra.BashCompOneRequiredFlag][0] == "true",
			})
		})
		sort.Slice(entry.Flags, func(i, j int) bool {
			return entry.Flags[i].Name < entry.Flags[j].Name
		})
		if len(sub.Commands()) > 0 {
			entry.Subcommands = collectAgentCommandsIn(sub, root, registry)
		}
		out = append(out, entry)
	}
	return out
}

func parseTypedExitCodes(raw string) []int {
	var codes []int
	for _, part := range strings.Split(raw, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			codes = append(codes, n)
		}
	}
	return codes
}

// registryEffect derives the single effect label that replaces reading the
// three-state mcp:read-only annotation: write (changes the library or local
// installation state a user did not name), local-file (writes a file the
// user names), local-state (changes only the local mirror), read (changes
// nothing), other (a local UI action).
func registryEffect(entry capabilityEntry, writesFiles bool) string {
	switch {
	case entry.Operation == "write":
		return "write"
	case writesFiles:
		return "local-file"
	case entry.Operation == "sync":
		return "local-state"
	case entry.Operation == "read" || entry.Operation == "introspect":
		return "read"
	case entry.Operation == "other":
		return "other"
	}
	return ""
}
