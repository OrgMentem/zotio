// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// the asserting guard the capabilityOverrides comment
// promises — every override key must resolve to a real runnable command, so a
// typo'd/renamed key can't silently fall through to a wrong (e.g. keyless)
// classification in the agent-facing capability registry.

package cli

import (
	"strings"
	"testing"

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
		if got := cmd.Annotations["mcp:inherited-flags"]; got != "via,connector-target" {
			t.Errorf("%s MCP inherited flags = %q, want via,connector-target", path, got)
		}
	}
}
