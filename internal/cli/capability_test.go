// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// the asserting guard the capabilityOverrides comment
// promises — every override key must resolve to a real runnable command, so a
// typo'd/renamed key can't silently fall through to a wrong (e.g. keyless)
// classification in the agent-facing capability registry.

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zotio/internal/store"

	"github.com/spf13/cobra"
)

func TestCapabilityOverridesResolveToRealCommands(t *testing.T) {
	entries := buildCapabilityRegistry(RootCmd())
	paths := make(map[string]bool, len(entries))
	for _, e := range entries {
		paths[e.Path] = true
	}
	for key := range capabilityOverrides {
		if !paths[key] {
			t.Errorf("capabilityOverrides key %q does not resolve to a runnable command (stale or typo'd?)", key)
		}
	}
}

func TestMutableCapabilityOverridesHaveWriteMetadata(t *testing.T) {
	want := map[string]struct {
		target  string
		require string
	}{
		"creators audit fix":       {target: "web_api", require: preconditionWebAPIKey},
		"items preprint-check fix": {target: "web_api", require: preconditionWebAPIKey},
		"vault pull":               {target: "local_vault", require: preconditionWebAPIKey},
		"vault sync":               {target: "local_vault", require: preconditionSyncedStore},
	}
	for _, entry := range buildCapabilityRegistry(RootCmd()) {
		expected, ok := want[entry.Path]
		if !ok {
			continue
		}
		delete(want, entry.Path)
		if entry.Operation != "write" || entry.WriteTarget != expected.target {
			t.Errorf("capability %q = operation=%q write_target=%q, want write to %q", entry.Path, entry.Operation, entry.WriteTarget, expected.target)
		}
		found := false
		for _, requirement := range entry.Requires {
			if requirement == expected.require {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("capability %q requires %q, want %q", entry.Path, entry.Requires, expected.require)
		}
	}
	for path := range want {
		t.Errorf("capability registry omitted mutable command %q", path)
	}
}

func TestCapabilityRoutesConsistentWithTopLevel(t *testing.T) {
	for _, entry := range buildCapabilityRegistry(RootCmd()) {
		if len(entry.Routes) == 0 {
			continue
		}
		if entry.Routes[0].Via != "default" {
			t.Errorf("capability %q routes[0].via = %q; the first route must be the default so it lines up with write_target", entry.Path, entry.Routes[0].Via)
		}
		want := map[string]bool{}
		for _, r := range entry.Routes[0].Requires {
			want[r] = true
		}
		got := map[string]bool{}
		for _, r := range entry.Requires {
			got[r] = true
		}
		for r := range want {
			if !got[r] {
				t.Errorf("capability %q default route requires %q but the top-level requires %v does not — the top-level fields must describe the default route", entry.Path, r, entry.Requires)
			}
		}
		for r := range got {
			if !want[r] {
				t.Errorf("capability %q top-level requires %q but the default route does not (%v) — the top-level fields must describe the default route, not a union", entry.Path, r, entry.Routes[0].Requires)
			}
		}
	}
}

func TestAttachmentsAddCarriesConnectorRoute(t *testing.T) {
	for _, entry := range buildCapabilityRegistry(RootCmd()) {
		if entry.Path != "attachments add" {
			continue
		}
		var connector *capabilityRoute
		for i := range entry.Routes {
			if entry.Routes[i].Via == "connector" {
				connector = &entry.Routes[i]
			}
		}
		if connector == nil {
			t.Fatalf("attachments add routes = %v, want a connector route", entry.Routes)
		}
		for _, banned := range []string{preconditionZoteroFileStorage, preconditionWebAPIKey} {
			for _, r := range connector.Requires {
				if r == banned {
					t.Errorf("attachments add connector route requires %q; the route exists to avoid Zotero cloud storage and never touches the Web API uploader", banned)
				}
			}
		}
		hasConnector := false
		for _, r := range connector.Requires {
			if r == preconditionDesktopConnector {
				hasConnector = true
			}
		}
		if !hasConnector {
			t.Errorf("attachments add connector route requires %v, want desktop_connector", connector.Requires)
		}
		return
	}
	t.Fatal("capability registry omitted attachments add")
}

func TestConnectorBackedCreateCommandsCarryBothRoutes(t *testing.T) {
	want := map[string]bool{
		"items create": false,
		"import apply": false,
		"import arxiv": false,
		"import doi":   false,
		"import file":  false,
		"import isbn":  false,
		"import pmid":  false,
		"import url":   false,
	}
	for _, entry := range buildCapabilityRegistry(RootCmd()) {
		if _, ok := want[entry.Path]; !ok {
			continue
		}
		want[entry.Path] = true
		if entry.WriteTarget != "web_api" {
			t.Errorf("%s write_target = %q, want the compatible default web_api", entry.Path, entry.WriteTarget)
		}
		if len(entry.Routes) != 2 {
			t.Errorf("%s routes = %+v, want default and connector", entry.Path, entry.Routes)
			continue
		}
		if entry.Routes[0].Via != "default" || !stringSliceContains(entry.Routes[0].Requires, preconditionWebAPIKey) {
			t.Errorf("%s default route = %+v, want web_api_key", entry.Path, entry.Routes[0])
		}
		if entry.Routes[1].Via != "connector" || !stringSliceContains(entry.Routes[1].Requires, preconditionDesktopConnector) {
			t.Errorf("%s connector route = %+v, want desktop_connector", entry.Path, entry.Routes[1])
		}
		if stringSliceContains(entry.Routes[1].Requires, preconditionWebAPIKey) {
			t.Errorf("%s connector route requires a Web API key: %+v", entry.Path, entry.Routes[1])
		}
		if !stringSliceContains(entry.DataSources, "web") || !stringSliceContains(entry.DataSources, "live") {
			t.Errorf("%s data_sources = %v, want web and live", entry.Path, entry.DataSources)
		}
	}
	for path, found := range want {
		if !found {
			t.Errorf("capability registry omitted connector-backed create command %q", path)
		}
	}
}

func TestItemsNewCarriesConnectorRouteWithTemplateKey(t *testing.T) {
	for _, entry := range buildCapabilityRegistry(RootCmd()) {
		if entry.Path != "items new" {
			continue
		}
		if len(entry.Routes) != 2 {
			t.Fatalf("items new routes = %+v, want default and connector", entry.Routes)
		}
		connector := entry.Routes[1]
		if connector.Via != "connector" ||
			!stringSliceContains(connector.Requires, preconditionWebAPIKey) ||
			!stringSliceContains(connector.Requires, preconditionDesktopConnector) {
			t.Fatalf("items new connector route = %+v, want web_api_key and desktop_connector", connector)
		}
		if !stringSliceContains(entry.DataSources, "web") || !stringSliceContains(entry.DataSources, "live") {
			t.Fatalf("items new data_sources = %v, want web and live", entry.DataSources)
		}
		return
	}
	t.Fatal("capability registry omitted items new")
}

// TestRoutedCapabilitiesAnnotateMCPRouteSelectors covers WRITE-routed
// capabilities. `via`/`connector-target` select a write plane, so a read
// command whose routes describe data planes (`collections export`,
// `items fulltext`, `items audit`) must NOT advertise them.
func TestRoutedCapabilitiesAnnotateMCPRouteSelectors(t *testing.T) {
	root := RootCmd()
	byPath := make(map[string]*cobra.Command)
	var walk func(*cobra.Command)
	walk = func(parent *cobra.Command) {
		for _, cmd := range parent.Commands() {
			byPath[strings.TrimPrefix(cmd.CommandPath(), root.Name()+" ")] = cmd
			walk(cmd)
		}
	}
	walk(root)

	for path, entry := range capabilityOverrides {
		if len(entry.Routes) < 2 {
			continue
		}
		cmd := byPath[path]
		if cmd == nil {
			t.Errorf("routed capability %q has no command", path)
			continue
		}
		got := cmd.Annotations["mcp:inherited-flags"]
		if entry.WriteTarget == "" {
			if got != "" {
				t.Errorf("%s has data-plane routes and no write target, but advertises route selectors %q", path, got)
			}
			continue
		}
		if got != "via,connector-target" {
			t.Errorf("%s MCP inherited flags = %q, want via,connector-target", path, got)
		}
	}
}

// capabilityOfflineProbe declares how to exercise one DECLARED capability with
// Zotero closed. Every registry entry that claims a precondition needs one, so
// a new claim cannot be added without stating how it is verified.
//
// routes carries the extra argv for each non-default route, so a route that
// declares the live plane is checked against the invocation that actually
// selects it (`--verify-files`, `--refresh`, `--format json`).
type capabilityOfflineProbe struct {
	args   []string
	routes map[string][]string
	// planeErr pins, per route, a substring proving a refusal was about the
	// plane for commands that refuse in their own words instead of through the
	// shared precondition_unmet envelope.
	planeErr map[string]string
	// degradesQuietly names routes whose command still exits 0 with Zotero
	// closed, and says what that costs. The registry claim is right — the route
	// does need the live plane — while the command's silent degradation is a
	// separate defect owned by that command. The gate asserts the note is
	// current: it fails as soon as the route starts refusing, which is the
	// signal to delete the note.
	degradesQuietly map[string]string
	skip            string
}

// Placeholders resolved from the seeded mirror, so probes reference rows that
// exist instead of keys invented here.
const (
	probeItemKey       = "{item}"
	probeCollectionKey = "{collection}"
	probeTempFile      = "{tmpfile}"
)

var capabilityOfflineProbes = map[string]capabilityOfflineProbe{
	// Live-plane reads.
	"searches run":             {args: []string{"searches", "run", "PROBESK"}},
	"items file":               {args: []string{"items", "file", probeItemKey}},
	"schema new-item-template": {args: []string{"schema", "new-item-template", "--item-type", "journalArticle"}},
	// The connector commands refuse on the base URL before any request, in
	// their own wording rather than through the shared envelope.
	"import targets":     {args: []string{"import", "targets"}, planeErr: map[string]string{"default": "local Zotero base URL"}},
	"import translators": {args: []string{"import", "translators", "https://example.org/paper"}, planeErr: map[string]string{"default": "local Zotero base URL"}},
	// collections export renders live and enumerates local, which is exactly
	// the two-route claim this test exists to keep honest.
	"collections export": {
		args:   []string{"collections", "export", probeCollectionKey},
		routes: map[string][]string{"json": {"collections", "export", probeCollectionKey, "--format", "json", "--data-source", "local"}},
	},
	// items fulltext is local-first with a live --refresh escape, so neither
	// plane is required on its own and only the named routes carry claims.
	"items fulltext": {
		routes: map[string][]string{
			"local":   {"items", "fulltext", probeItemKey, "--data-source", "local"},
			"refresh": {"items", "fulltext", probeItemKey, "--refresh"},
		},
	},
	// Store-backed reads.
	"library health":    {args: []string{"library", "health"}},
	"library stats":     {args: []string{"library", "stats"}},
	"library prisma":    {args: []string{"library", "prisma"}},
	"items missing-pdf": {args: []string{"items", "missing-pdf"}},
	"items duplicates":  {args: []string{"items", "duplicates"}},
	"items related":     {args: []string{"items", "related", probeItemKey}},
	"items similar":     {args: []string{"items", "similar", probeItemKey}},
	"items summarize":   {args: []string{"items", "summarize", probeItemKey}},
	"creators audit":    {args: []string{"creators", "audit"}},
	"tags audit":        {args: []string{"tags", "audit"}},
	"tags inventory":    {args: []string{"tags", "inventory"}},
	"items audit": {
		args:   []string{"items", "audit"},
		routes: map[string][]string{"verify-files": {"items", "audit", "--verify-files"}},
		degradesQuietly: map[string]string{
			// attachmentFileStatus (items_audit.go) resolves each path through
			// fetchAttachmentFileURL, the error-DISCARDING wrapper, so an
			// unreachable local API reads as "unresolved" and every PDF
			// attachment in the mirror is reported broken at exit 0. The route
			// declaration is right; the command's loudness is the open defect.
			"verify-files": "reports every mirrored PDF attachment as broken instead of refusing when the local API is unreachable",
		},
	},
	"items citekey-conflicts": {args: []string{"items", "citekey-conflicts"}},
	"items bibcheck":          {args: []string{"items", "bibcheck", probeTempFile}},
	"export snapshot verify": {
		skip: "verifies a lockfile artifact produced by a prior export run; its own tests cover the offline read",
	},
	"import discover": {
		skip: "chases citations through external metadata providers, a plane no precondition in this vocabulary describes",
	},
	// Writes: offline behaviour is the write guard's contract, not this test's,
	// EXCEPT where a write depends on a live READ to build its plan.
	"searches materialize": {args: []string{"searches", "materialize", "PROBESK", "--to", probeCollectionKey, "--dry-run"}},
}

func init() {
	for path, entry := range capabilityOverrides {
		if entry.WriteTarget == "" && entry.Operation != "write" {
			continue
		}
		if _, ok := capabilityOfflineProbes[path]; ok {
			continue
		}
		capabilityOfflineProbes[path] = capabilityOfflineProbe{
			skip: "write route; a missing key is enforced by the apply-time write guard, not by offline behaviour",
		}
	}
}

// TestCapabilityRequiresMatchesOfflineBehaviour is the drift gate for the
// registry's central promise: what a command DECLARES is what it can actually
// do with Zotero closed.
//
// Each declared capability runs against a synced mirror on an unreachable base
// URL, with central preflight switched off. Preflight has to be off: it refuses
// on the strength of the declaration itself, so with it on an over-claim
// (declaring the live plane for a read the mirror serves) would be masked by
// the very claim under test.
//
// The verdict per entry/route:
//   - claims local  -> the run must NOT fail on a plane (no network error, no
//     live/connector refusal). Data-absence and other content errors are fine:
//     the claim is about the plane, not the library's contents.
//   - claims live only -> the run MUST fail on a plane. Succeeding is silent
//     degradation; failing for another reason means the mirror could have
//     served it and the entry over-claims.
func TestCapabilityRequiresMatchesOfflineBehaviour(t *testing.T) {
	for _, entry := range buildCapabilityRegistry(RootCmd()) {
		if len(entry.Requires) == 0 && len(entry.Routes) == 0 {
			// Declares nothing, so there is no claim to verify.
			continue
		}
		if len(entry.Requires) == 0 && !anyRouteDeclares(entry.Routes) {
			t.Errorf("capability %q declares neither top-level preconditions nor any route precondition; the entry claims nothing", entry.Path)
			continue
		}
		probe, ok := capabilityOfflineProbes[entry.Path]
		if !ok {
			t.Errorf("capability %q declares %v but has no offline probe; add one to capabilityOfflineProbes so the claim stays verified", entry.Path, entry.Requires)
			continue
		}
		if probe.skip != "" {
			continue
		}
		for _, route := range entry.Routes {
			if route.Via == "default" || len(route.Requires) == 0 {
				continue
			}
			if _, ok := probe.routes[route.Via]; !ok {
				t.Errorf("capability %q route %q declares %v but the probe has no argv selecting that route", entry.Path, route.Via, route.Requires)
			}
		}

		// An entry whose top-level Requires is empty makes no unconditional
		// claim (items fulltext: neither plane is required on its own), so
		// there is nothing to verify for the default invocation. Its named
		// routes carry the claims and are probed below.
		if len(entry.Requires) > 0 {
			t.Run(entry.Path, func(t *testing.T) {
				assertCapabilityOfflineClaim(t, entry.Path, "default", entry.Requires, probe)
			})
		}
		for _, route := range entry.Routes {
			if route.Via == "default" || len(route.Requires) == 0 {
				continue
			}
			if _, ok := probe.routes[route.Via]; !ok {
				continue
			}
			t.Run(entry.Path+" via "+route.Via, func(t *testing.T) {
				assertCapabilityOfflineClaim(t, entry.Path, route.Via, route.Requires, probe)
			})
		}
	}
}

func anyRouteDeclares(routes []capabilityRoute) bool {
	for _, r := range routes {
		if len(r.Requires) > 0 {
			return true
		}
	}
	return false
}

func assertCapabilityOfflineClaim(t *testing.T, path, via string, requires []string, probe capabilityOfflineProbe) {
	t.Helper()
	args := probe.args
	if via != "default" {
		args = probe.routes[via]
	}
	if len(args) == 0 {
		t.Fatalf("capability %q route %q has an empty probe argv", path, via)
	}
	fixture := seedCapabilityProbeStore(t)
	out, err := runRootCommandOffline(t, resolveProbeArgs(t, args, fixture)...)
	planeFailure, detail := offlinePlaneFailure(out, err, probe.planeErr[via])

	if capabilityClaimsLocal(requires) {
		if planeFailure {
			t.Fatalf("%s (route %q) declares %v, which includes a mirror-served plane, but it failed offline on the plane: %s\noutput: %s",
				path, via, requires, detail, out)
		}
		return
	}
	if note := probe.degradesQuietly[via]; note != "" {
		// A recorded quiet degradation must stay current: once the route
		// refuses, the note is stale and has to go.
		if err != nil {
			t.Fatalf("%s (route %q) is recorded as degrading quietly (%s) but now fails offline (%v) — delete that degradesQuietly entry",
				path, via, note, err)
		}
		return
	}
	if err == nil {
		t.Fatalf("%s (route %q) declares live-only %v but succeeded with Zotero closed — either the claim is wrong or this is a silent empty success\noutput: %s",
			path, via, requires, out)
	}
	if !planeFailure {
		t.Fatalf("%s (route %q) declares live-only %v but offline it failed for an unrelated reason (%v); if the mirror can serve this read, the entry over-claims\noutput: %s",
			path, via, requires, err, out)
	}
}

// capabilityClaimsLocal reports whether a precondition set can be satisfied by
// the synced mirror alone. It mirrors dataSourcesForRequires rather than
// re-deriving it, so the test and the emitted registry agree by construction.
func capabilityClaimsLocal(requires []string) bool {
	for _, ds := range dataSourcesForRequires(requires) {
		if ds == "local" {
			return true
		}
	}
	return false
}

// offlinePlaneFailure reports whether a failure was caused by an unreachable
// live plane rather than by the library's contents. planeErr carries the
// command's own refusal wording for the commands that do not use the shared
// precondition_unmet envelope.
func offlinePlaneFailure(out string, err error, planeErr string) (bool, string) {
	if err == nil {
		return false, ""
	}
	if isNetworkError(err) {
		return true, "unreachable live API"
	}
	var env preconditionUnmetEnvelope
	if json.Unmarshal([]byte(out), &env) == nil && env.Kind == "precondition_unmet" {
		switch env.Precondition {
		case preconditionLiveLocalAPI, preconditionDesktopConnector:
			return true, "precondition_unmet " + env.Precondition
		}
	}
	for _, plane := range []string{preconditionLiveLocalAPI, preconditionDesktopConnector} {
		if strings.Contains(err.Error(), plane) {
			return true, "refusal naming " + plane
		}
	}
	if planeErr != "" && strings.Contains(err.Error(), planeErr) {
		return true, "refusal matching " + planeErr
	}
	return false, ""
}

type capabilityProbeFixture struct {
	itemKey       string
	collectionKey string
}

// seedCapabilityProbeStore builds the synced mirror every probe runs against:
// the bundled demo library plus one saved-search DEFINITION, which is what sync
// actually stores for a saved search (never its membership).
func seedCapabilityProbeStore(t *testing.T) capabilityProbeFixture {
	t.Helper()
	isolateDemoEnv(t, "0")
	dbPath := helpersTestDefaultDBPath(t, "zotio")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir store dir: %v", err)
	}
	if _, err := seedDemoStore(t.Context(), dbPath); err != nil {
		t.Fatalf("seed demo library: %v", err)
	}
	db, err := store.OpenWithContext(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if _, _, err := db.UpsertBatch("searches", []json.RawMessage{
		json.RawMessage(`{"key":"PROBESK","version":1,"data":{"key":"PROBESK","name":"probe search","conditions":[{"condition":"title","operator":"contains","value":"a"}]}}`),
	}); err != nil {
		t.Fatalf("seed saved search: %v", err)
	}
	if err := db.SaveSyncState("searches", "", 1); err != nil {
		t.Fatalf("record searches sync state: %v", err)
	}
	qs := localQueryStore{db}
	fixture := capabilityProbeFixture{
		itemKey:       probeFirstKey(t, qs, `SELECT id AS key FROM resources WHERE resource_type='items' AND json_extract(data,'$.data.itemType') NOT IN ('attachment','annotation','note') ORDER BY id LIMIT 1`),
		collectionKey: probeFirstKey(t, qs, `SELECT id AS key FROM resources WHERE resource_type='collections' ORDER BY id LIMIT 1`),
	}
	if fixture.itemKey == "" || fixture.collectionKey == "" {
		t.Fatalf("demo fixture seeded no item/collection to probe with: %+v", fixture)
	}
	return fixture
}

func probeFirstKey(t *testing.T, qs localQueryStore, query string) string {
	t.Helper()
	rows, err := qs.QueryRawContext(t.Context(), query)
	if err != nil {
		t.Fatalf("probe fixture query: %v", err)
	}
	if len(rows) == 0 {
		return ""
	}
	return sqlStringValue(rows[0]["key"])
}

func resolveProbeArgs(t *testing.T, args []string, fixture capabilityProbeFixture) []string {
	t.Helper()
	out := make([]string, 0, len(args))
	for _, arg := range args {
		switch arg {
		case probeItemKey:
			out = append(out, fixture.itemKey)
		case probeCollectionKey:
			out = append(out, fixture.collectionKey)
		case probeTempFile:
			path := filepath.Join(t.TempDir(), "manuscript.tex")
			if err := os.WriteFile(path, []byte(`\cite{probeKey}`), 0o600); err != nil {
				t.Fatalf("write probe manuscript: %v", err)
			}
			out = append(out, path)
		default:
			out = append(out, arg)
		}
	}
	return out
}

// runRootCommandOffline executes one command through a fresh root tree with the
// central preflight gate disabled, so what is measured is the command's own
// offline behaviour. The annotation goes on the root because
// commandPreflightSkipped walks the parent chain.
func runRootCommandOffline(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := RootCmd()
	if root.Annotations == nil {
		root.Annotations = map[string]string{}
	}
	root.Annotations[preflightAnnotationKey] = preflightAnnotationSkip
	root.SilenceErrors, root.SilenceUsage = true, true
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(append([]string{"--json", "--timeout", "5s"}, args...))
	err := root.Execute()
	return out.String(), err
}
