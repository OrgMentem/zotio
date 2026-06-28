// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase2): typed capability + preconditions registry — the
// single machine-readable source of truth for what each command does (read/
// write/destructive), where it writes, and what it requires (live desktop API,
// a Web API key, a synced store, Better BibTeX). Agents select safe commands and
// pre-flight preconditions from this instead of parsing --help.

package cli

import (
	"encoding/json"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// Precondition vocabulary. These strings are the contract shared with the
// per-command precondition_unmet envelopes (see library_health.go, ensure_live.go).
const (
	preconditionLiveLocalAPI = "live_local_api"
	preconditionWebAPIKey    = "web_api_key"
	preconditionSyncedStore  = "synced_store"
	preconditionBetterBibTeX = "better_bibtex"
)

type capabilityEntry struct {
	Path        string   `json:"path"`
	Operation   string   `json:"operation"` // read | write | sync | introspect | other
	DataSources []string `json:"data_sources,omitempty"`
	WriteTarget string   `json:"write_target,omitempty"`
	Destructive bool     `json:"destructive,omitempty"`
	Requires    []string `json:"requires,omitempty"`
}

// capabilityOverrides carries the safety-critical metadata that cannot be
// derived from Cobra annotations: preconditions, write targets, and
// destructiveness. Keys are full command paths (root name stripped). The
// builder merges these onto the annotation-derived base; a test asserts every
// key resolves to a real runnable command so the table never goes stale.
var capabilityOverrides = map[string]capabilityEntry{
	// Reads that need the live Zotero desktop / local API.
	"searches run":   {Requires: []string{preconditionLiveLocalAPI}},
	"items file":     {Requires: []string{preconditionLiveLocalAPI}},
	"items fulltext": {Requires: []string{preconditionLiveLocalAPI}},
	// Reads backed by the synced local store (degrade with a "run sync" hint).
	"library health":    {Requires: []string{preconditionSyncedStore}},
	"library stats":     {Requires: []string{preconditionSyncedStore}},
	"items audit":       {Requires: []string{preconditionSyncedStore}},
	"items missing-pdf": {Requires: []string{preconditionSyncedStore}},
	"items duplicates":  {Requires: []string{preconditionSyncedStore}},
	"items summarize":   {Requires: []string{preconditionSyncedStore}},
	"tags audit":        {Requires: []string{preconditionSyncedStore}},
	"tags inventory":    {Requires: []string{preconditionSyncedStore}},
	// Citation keys live in Better BibTeX's `extra` field.
	"items citekey-conflicts": {Requires: []string{preconditionSyncedStore, preconditionBetterBibTeX}},
	// Global /items/new template is served by the Web API only (not local).
	"schema new-item-template": {Requires: []string{preconditionWebAPIKey}},
	// Mutations: auto-routed to the Web API, so they need a key.
	"items create":             {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"items update":             {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"items move":               {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"items restore":            {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"items delete":             {Operation: "write", WriteTarget: "web_api", Destructive: true, Requires: []string{preconditionWebAPIKey}},
	"items enrich":             {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"items tags add":           {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"items tags remove":        {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"items duplicates resolve": {Operation: "write", WriteTarget: "web_api", Destructive: true, Requires: []string{preconditionWebAPIKey}},
	"collections create":       {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"collections update":       {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"collections move":         {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"collections delete":       {Operation: "write", WriteTarget: "web_api", Destructive: true, Requires: []string{preconditionWebAPIKey}},
	"tags rename":              {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"tags audit fix":           {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"reading-list add":         {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"reading-list start":       {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"reading-list done":        {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"searches materialize":     {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"import doi":               {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"import url":               {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"import file":              {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"import pmid":              {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"import arxiv":             {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"import isbn":              {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	// items new validates against /items/new (Web-only) then POSTs.
	"items new": {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	// import apply creates items / linked-file attachments via the Web API.
	"import apply": {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	// vault push/resolve write tool-owned child notes back to Zotero via the Web API.
	"vault push":    {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"vault resolve": {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	// Sync writes the local store (not a Zotero mutation).
	"sync":  {Operation: "sync"},
	"watch": {Operation: "sync"},
	// Undo replays inverse membership changes via the Web API.
	"journal undo": {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	// Introspection.
	"doctor":  {Operation: "introspect"},
	"which":   {Operation: "introspect"},
	"version": {Operation: "introspect"},
}

// dataSourcesForRequires derives the supported data sources from preconditions,
// so the field never drifts from the precondition truth.
func dataSourcesForRequires(requires []string) []string {
	has := func(p string) bool {
		for _, r := range requires {
			if r == p {
				return true
			}
		}
		return false
	}
	switch {
	case has(preconditionWebAPIKey):
		return []string{"web"}
	case has(preconditionLiveLocalAPI):
		return []string{"live"}
	case has(preconditionSyncedStore):
		return []string{"local", "live"}
	default:
		return nil
	}
}

// buildCapabilityRegistry walks the command tree and emits one entry per
// runnable command, deriving operation from the mcp:read-only annotation and
// merging the safety-critical overrides. Sorted by path for stable output.
func buildCapabilityRegistry(rootCmd *cobra.Command) []capabilityEntry {
	skip := map[string]bool{"help": true, "completion": true, "capabilities": true, "agent-context": true}
	entries := make([]capabilityEntry, 0, 64)

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if sub.Hidden || skip[sub.Name()] {
				continue
			}
			if sub.Runnable() {
				path := strings.TrimPrefix(sub.CommandPath(), rootCmd.Name()+" ")
				entry := capabilityEntry{Path: path, Operation: "other"}
				if sub.Annotations["mcp:read-only"] == "true" {
					entry.Operation = "read"
				}
				if ov, ok := capabilityOverrides[path]; ok {
					if ov.Operation != "" {
						entry.Operation = ov.Operation
					}
					entry.WriteTarget = ov.WriteTarget
					entry.Destructive = ov.Destructive
					entry.Requires = ov.Requires
				}
				entry.DataSources = dataSourcesForRequires(entry.Requires)
				entries = append(entries, entry)
			}
			walk(sub)
		}
	}
	walk(rootCmd)

	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries
}

// newCapabilitiesCmd emits the capability registry as JSON so agents and MCP
// hosts have one source of truth for command safety and preconditions.
// PATCH(glean roadmap-phase2): new introspection command.
func newCapabilitiesCmd(rootCmd *cobra.Command, flags ...*rootFlags) *cobra.Command {
	var pretty bool
	driftFlags := &rootFlags{}
	if len(flags) > 0 && flags[0] != nil {
		driftFlags = flags[0]
	}
	cmd := &cobra.Command{
		Use:         "capabilities",
		Short:       "Emit the machine-readable capability + preconditions registry",
		Annotations: map[string]string{"mcp:read-only": "true"},
		Long: `Outputs a typed registry describing each command's operation kind
(read/write/destructive), write target, supported data sources, and
preconditions (live_local_api, web_api_key, synced_store, better_bibtex) so
agents can select safe commands and pre-flight requirements without parsing
--help or guessing from names.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			enc := json.NewEncoder(os.Stdout)
			if pretty {
				enc.SetIndent("", "  ")
			}
			return enc.Encode(buildCapabilityRegistry(rootCmd))
		},
	}
	cmd.Flags().BoolVar(&pretty, "pretty", false, "indent JSON output for human reading")
	cmd.AddCommand(newCapabilitiesDriftCmd(driftFlags))
	return cmd
}
