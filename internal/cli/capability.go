// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// typed capability + preconditions registry — the
// single machine-readable source of truth for what each command does (read/
// write/destructive), where it writes, and what it requires (live desktop API,
// a Web API key, a synced store, Better BibTeX). Agents select safe commands and
// pre-flight preconditions from this instead of parsing --help.

package cli

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// Precondition vocabulary. These strings are the contract shared with the
// per-command precondition_unmet envelopes (see library_health.go, ensure_live.go).
const (
	preconditionLiveLocalAPI     = "live_local_api"
	preconditionWebAPIKey        = "web_api_key"
	preconditionSyncedStore      = "synced_store"
	preconditionBetterBibTeX     = "better_bibtex"
	preconditionDesktopConnector = "desktop_connector"
	// preconditionZoteroFileStorage guards commands that upload attachment
	// bytes through the Zotero Web API file-upload protocol, which always
	// writes into Zotero's own cloud storage. It is unmet when Zotero desktop
	// is configured to keep files somewhere else (see file_storage_guard.go).
	preconditionZoteroFileStorage = "zotero_file_storage"
)

type capabilityEntry struct {
	Path        string   `json:"path"`
	Operation   string   `json:"operation"` // read | write | sync | introspect | other
	DataSources []string `json:"data_sources,omitempty"`
	WriteTarget string   `json:"write_target,omitempty"`
	Destructive bool     `json:"destructive,omitempty"`
	Requires    []string `json:"requires,omitempty"` // describes the default route; routes carry the full truth
	// Routes splits a command with more than one write path into one entry per
	// route, each carrying its own Requires. The first route must be the
	// default, and the top-level Requires/WriteTarget describe that route
	// alone — NOT a union — so consumers written before Routes keep reading
	// the same meaning they always had.
	Routes []capabilityRoute `json:"routes,omitempty"`
}

type capabilityRoute struct {
	Via      string   `json:"via"`
	Requires []string `json:"requires,omitempty"`
}

// connectorCreateCapability describes item-creation commands whose default
// machine-readable contract remains the Web API route, but which can also
// create in a personal library through a running Zotero desktop connector.
// Keep this value immutable: capabilityOverrides shares its route slices.
var connectorCreateCapability = capabilityEntry{
	Operation:   "write",
	WriteTarget: "web_api",
	Requires:    []string{preconditionWebAPIKey},
	Routes: []capabilityRoute{
		{Via: "default", Requires: []string{preconditionWebAPIKey}},
		{Via: "connector", Requires: []string{preconditionDesktopConnector}},
	},
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
	// Reads backed by the synced local store.
	"library health":         {Requires: []string{preconditionSyncedStore}},
	"library stats":          {Requires: []string{preconditionSyncedStore}},
	"items audit":            {Requires: []string{preconditionSyncedStore}},
	"items missing-pdf":      {Requires: []string{preconditionSyncedStore}},
	"items duplicates":       {Requires: []string{preconditionSyncedStore}},
	"library prisma":         {Requires: []string{preconditionSyncedStore}},
	"items related":          {Requires: []string{preconditionSyncedStore}},
	"items similar":          {Requires: []string{preconditionSyncedStore}},
	"items summarize":        {Requires: []string{preconditionSyncedStore}},
	"creators audit":         {Operation: "read", Requires: []string{preconditionSyncedStore}},
	"creators audit fix":     {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionSyncedStore, preconditionWebAPIKey}},
	"tags audit":             {Requires: []string{preconditionSyncedStore}},
	"tags inventory":         {Requires: []string{preconditionSyncedStore}},
	"export snapshot verify": {Requires: []string{preconditionSyncedStore}},
	// Citation keys live in Better BibTeX's `extra` field.
	"items citekey-conflicts": {Requires: []string{preconditionSyncedStore, preconditionBetterBibTeX}},
	"items bibcheck":          {Requires: []string{preconditionSyncedStore, preconditionBetterBibTeX}},
	// Schema templates are built from global endpoints served by the local API.
	"schema new-item-template": {Requires: []string{preconditionLiveLocalAPI}},
	// Mutations: auto-routed to the Web API, so they need a key.
	"items create":             connectorCreateCapability,
	"items update":             {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"items move":               {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"items add-to-collection":  {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
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
	"import doi":               connectorCreateCapability,
	"import url":               connectorCreateCapability,
	"import file":              connectorCreateCapability,
	"import pmid":              connectorCreateCapability,
	"import arxiv":             connectorCreateCapability,
	"import isbn":              connectorCreateCapability,
	"import discover":          {Operation: "read", Requires: []string{preconditionSyncedStore}},
	"import pdf":               {Operation: "write", WriteTarget: "desktop_connector", Requires: []string{preconditionDesktopConnector}},
	"import targets":           {Operation: "read", Requires: []string{preconditionDesktopConnector}},
	"import translators":       {Operation: "read", Requires: []string{preconditionDesktopConnector}},
	// items new validates against /items/new (Web-only) then POSTs.
	"items new":                {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"items preprint-check fix": {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	// import apply can create items through either route. Some manifest
	// actions add stronger runtime checks, but every connector create needs
	// only the desktop_connector precondition at the registry level.
	"import apply": connectorCreateCapability,
	// attachments add uploads stored files via the Zotero Web API file-upload
	// protocol. That always targets Zotero's cloud storage, so the upload is
	// refused when Zotero keeps its files elsewhere. `--via connector` bypasses
	// the uploader entirely (temporary connector parent, then re-parent), and
	// checkZoteroFileStoragePrecondition lets that route through.
	//
	// Routes carries the per-route truth: the default (web uploader) route needs
	// the key and Zotero cloud storage; the connector route needs a running
	// desktop and NOT Zotero cloud storage. The top-level Requires/WriteTarget
	// describe the default route, preserving their pre-Routes meaning.
	// Do NOT make WriteTarget conditional — two reviewers and the consumer all
	// rejected it, because a scalar that sometimes means "default" and sometimes
	// "one of several" cannot be read safely without knowing which release
	// wrote it. Reasoning in dev/field-report-2026-08-23-papio-capability-routes.md.
	"attachments add": {
		Operation:   "write",
		WriteTarget: "web_api",
		Requires:    []string{preconditionWebAPIKey, preconditionZoteroFileStorage},
		Routes: []capabilityRoute{
			{Via: "default", Requires: []string{preconditionWebAPIKey, preconditionZoteroFileStorage}},
			{Via: "connector", Requires: []string{preconditionDesktopConnector}},
		},
	},
	// vault push writes to Zotero; pull and sync write the local vault.
	"vault push":    {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"vault pull":    {Operation: "write", WriteTarget: "local_vault", Requires: []string{preconditionWebAPIKey}},
	"vault resolve": {Operation: "write", WriteTarget: "web_api", Requires: []string{preconditionWebAPIKey}},
	"vault sync":    {Operation: "write", WriteTarget: "local_vault", Requires: []string{preconditionSyncedStore}},
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

// dataSourcesForEntry derives the supported data sources from preconditions,
// so the field never drifts from the precondition truth. For routed commands it
// unions across routes, so `attachments add` reports both "web" (default) and
// "live" (connector) even though the top-level Requires alone maps to web only.
func dataSourcesForEntry(entry capabilityEntry) []string {
	seen := map[string]bool{}
	var out []string
	add := func(requires []string) {
		for _, ds := range dataSourcesForRequires(requires) {
			if !seen[ds] {
				seen[ds] = true
				out = append(out, ds)
			}
		}
	}
	add(entry.Requires)
	for _, r := range entry.Routes {
		add(r.Requires)
	}
	return out
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
	case has(preconditionDesktopConnector):
		return []string{"live"}
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
					entry.Routes = ov.Routes
				}
				entry.DataSources = dataSourcesForEntry(entry)
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
preconditions (live_local_api, web_api_key, synced_store, better_bibtex,
desktop_connector, zotero_file_storage) so agents can select safe commands
and pre-flight requirements without parsing --help or guessing from names.
Commands with more than one write route carry a routes list where each route
names its own preconditions; the top-level requires and write_target describe
the default route (the first entry in routes).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := json.Marshal(buildCapabilityRegistry(rootCmd))
			if err != nil {
				return err
			}
			wrapped, err := wrapWithProvenance(json.RawMessage(data), DataProvenance{
				Source:       "local",
				Reason:       "generated_registry",
				ResourceType: "capabilities",
			})
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			if pretty {
				enc.SetIndent("", "  ")
			}
			return enc.Encode(wrapped)
		},
	}
	cmd.Flags().BoolVar(&pretty, "pretty", false, "indent JSON output for human reading")
	cmd.AddCommand(newCapabilitiesDriftCmd(driftFlags))
	return cmd
}
