// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"

	"github.com/spf13/cobra"
)

func TestCapabilityPreflightRefusesSyncedStoreCommandWithoutStore(t *testing.T) {
	root, _, out, _ := newPreflightTestRoot(t)

	audit := mustFindPreflightCommand(t, root, "items", "audit")
	runExecuted := false
	audit.RunE = func(cmd *cobra.Command, args []string) error {
		runExecuted = true
		return nil
	}

	root.SetArgs([]string{"--json", "items", "audit"})
	err := root.Execute()
	if err == nil {
		t.Fatal("items audit without a synced store succeeded, want precondition error")
	}
	if runExecuted {
		t.Fatal("items audit RunE executed after synced_store preflight failed")
	}
	assertPreconditionExitCode(t, err, 9)

	var env preconditionUnmetEnvelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		t.Fatalf("decode precondition envelope: %v; output=%q", err, out.String())
	}
	if env.Kind != "precondition_unmet" {
		t.Fatalf("kind = %q, want precondition_unmet", env.Kind)
	}
	if env.Capability != "items audit" {
		t.Fatalf("capability = %q, want items audit", env.Capability)
	}
	if env.Precondition != preconditionSyncedStore {
		t.Fatalf("precondition = %q, want %s", env.Precondition, preconditionSyncedStore)
	}
	if env.Detail == "" {
		t.Fatal("detail is empty")
	}
	if len(env.Remediation) == 0 {
		t.Fatal("remediation is empty")
	}
	if !env.RetryAfterRemediation {
		t.Fatal("retry_after_remediation = false, want true")
	}
}

func TestCapabilityPreflightSkipAnnotationsBypassStorePreconditions(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "doctor", args: []string{"doctor"}},
		{name: "sync", args: []string{"sync"}},
		{name: "init", args: []string{"init"}},
		{name: "demo", args: []string{"demo"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, _, _, _ := newPreflightTestRoot(t)
			cmd := mustFindPreflightCommand(t, root, tc.args...)
			path := commandRegistryPath(cmd)
			if path == "" {
				t.Fatalf("empty registry path for %s", cmd.CommandPath())
			}
			withTemporaryCapabilityOverride(t, path, capabilityEntry{Requires: []string{preconditionSyncedStore}})

			runExecuted := false
			cmd.RunE = func(cmd *cobra.Command, args []string) error {
				runExecuted = true
				return nil
			}

			root.SetArgs(tc.args)
			if err := root.Execute(); err != nil {
				t.Fatalf("%s with preflight skip annotation returned error: %v", tc.name, err)
			}
			if !runExecuted {
				t.Fatalf("%s RunE did not execute", tc.name)
			}
		})
	}
}

func TestCapabilityPreflightWritePreviewBypassesWebAPIKeyRequirement(t *testing.T) {
	root, flags, _, _ := newPreflightTestRoot(t)

	create := mustFindPreflightCommand(t, root, "items", "create")
	runExecuted := false
	create.RunE = func(cmd *cobra.Command, args []string) error {
		runExecuted = true
		if !flags.dryRun {
			t.Fatal("items create RunE saw dryRun=false, want preview path")
		}
		return nil
	}

	root.SetArgs([]string{"--json", "--dry-run", "items", "create", "--items", `[{"itemType":"book","title":"preview"}]`})
	if err := root.Execute(); err != nil {
		t.Fatalf("items create preview without a web API key returned error: %v", err)
	}
	if !runExecuted {
		t.Fatal("items create RunE did not execute; web_api_key was enforced before preview")
	}
}

func TestCapabilityPreflightOverrideRequiresKnownPreconditions(t *testing.T) {
	known := map[string]struct{}{
		preconditionLiveLocalAPI:     {},
		preconditionWebAPIKey:        {},
		preconditionSyncedStore:      {},
		preconditionBetterBibTeX:     {},
		preconditionDesktopConnector: {},
	}
	for req := range preconditionCheckers {
		if _, ok := known[req]; !ok {
			t.Fatalf("preconditionCheckers declares %q outside the known precondition set", req)
		}
	}

	tests := make([]struct {
		name string
		path string
		req  string
	}, 0)
	for path, entry := range capabilityOverrides {
		for _, req := range entry.Requires {
			tests = append(tests, struct {
				name string
				path string
				req  string
			}{name: path + " requires " + req, path: path, req: req})
		}
	}
	if len(tests) == 0 {
		t.Fatal("capabilityOverrides has no Requires entries to validate")
	}
	sort.Slice(tests, func(i, j int) bool { return tests[i].name < tests[j].name })

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := known[tc.req]; !ok {
				t.Fatalf("capabilityOverrides[%q] declares unknown precondition %q", tc.path, tc.req)
			}
		})
	}
}

func TestCapabilityPreflightSyncedStoreCommandRunsWithHealthyFixture(t *testing.T) {
	root, _, _, _ := newPreflightTestRoot(t)
	if count, err := seedDemoStore(context.Background(), defaultDBPath("zotio")); err != nil {
		t.Fatalf("seed healthy store fixture: %v", err)
	} else if count == 0 {
		t.Fatal("seed healthy store fixture produced zero items")
	}

	audit := mustFindPreflightCommand(t, root, "items", "audit")
	runExecuted := false
	audit.RunE = func(cmd *cobra.Command, args []string) error {
		runExecuted = true
		return nil
	}

	root.SetArgs([]string{"--json", "items", "audit"})
	if err := root.Execute(); err != nil {
		t.Fatalf("items audit with healthy synced store returned error: %v", err)
	}
	if !runExecuted {
		t.Fatal("items audit RunE did not execute with healthy synced store")
	}
}

func TestCapabilityPreflightBetterBibTeXPassesWithCitationKeyFieldOnlyItems(t *testing.T) {
	root, _, _, _ := newPreflightTestRoot(t)
	seedSyncedBibcheckItems(t, []json.RawMessage{
		json.RawMessage(`{"key":"FIELD1","version":1,"data":{"key":"FIELD1","itemType":"journalArticle","title":"Field Key Only","citationKey":"fieldonly"}}`),
	})

	citekeys := mustFindPreflightCommand(t, root, "items", "citekey-conflicts")
	runExecuted := false
	citekeys.RunE = func(cmd *cobra.Command, args []string) error {
		runExecuted = true
		return nil
	}

	root.SetArgs([]string{"--json", "items", "citekey-conflicts"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Better BibTeX preflight rejected citationKey-field-only item: %v", err)
	}
	if !runExecuted {
		t.Fatal("items citekey-conflicts RunE did not execute after citationKey-field-only preflight")
	}
}

func TestCapabilityPreflightBetterBibTeXFailsEnvelopeWhenNoCitationKeySourceExists(t *testing.T) {
	root, _, out, _ := newPreflightTestRoot(t)
	seedSyncedBibcheckItems(t, []json.RawMessage{
		json.RawMessage(`{"key":"NOKEY1","version":1,"data":{"key":"NOKEY1","itemType":"journalArticle","title":"No Better BibTeX Key","extra":"ordinary notes"}}`),
	})

	citekeys := mustFindPreflightCommand(t, root, "items", "citekey-conflicts")
	runExecuted := false
	citekeys.RunE = func(cmd *cobra.Command, args []string) error {
		runExecuted = true
		return nil
	}

	root.SetArgs([]string{"--json", "items", "citekey-conflicts"})
	err := root.Execute()
	if err == nil {
		t.Fatal("Better BibTeX preflight succeeded without citationKey fields or Citation Key Extra lines")
	}
	if runExecuted {
		t.Fatal("items citekey-conflicts RunE executed after Better BibTeX preflight failed")
	}
	assertPreconditionExitCode(t, err, 9)

	var env preconditionUnmetEnvelope
	if decodeErr := json.Unmarshal(out.Bytes(), &env); decodeErr != nil {
		t.Fatalf("decode precondition envelope: %v; output=%q", decodeErr, out.String())
	}
	if env.Kind != "precondition_unmet" {
		t.Fatalf("kind = %q, want precondition_unmet", env.Kind)
	}
	if env.Capability != "items citekey-conflicts" {
		t.Fatalf("capability = %q, want items citekey-conflicts", env.Capability)
	}
	if env.Precondition != preconditionBetterBibTeX {
		t.Fatalf("precondition = %q, want %s", env.Precondition, preconditionBetterBibTeX)
	}
	if env.Detail == "" {
		t.Fatal("detail is empty")
	}
	if len(env.Remediation) == 0 {
		t.Fatal("remediation is empty")
	}
}

func newPreflightTestRoot(t *testing.T) (*cobra.Command, *rootFlags, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	isolateDemoEnv(t, "0")
	flags := &rootFlags{}
	root := newRootCmd(flags)
	root.SilenceErrors = true
	root.SilenceUsage = true
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(errOut)
	return root, flags, out, errOut
}

func mustFindPreflightCommand(t *testing.T, root *cobra.Command, path ...string) *cobra.Command {
	t.Helper()
	cmd, remaining, err := root.Find(path)
	if err != nil {
		t.Fatalf("find command %v: %v", path, err)
	}
	if cmd == nil || len(remaining) != 0 || cmd == root {
		t.Fatalf("find command %v = %v remaining %v, want runnable child", path, cmd, remaining)
	}
	if cmd.Run == nil && cmd.RunE == nil {
		t.Fatalf("command %s is not runnable", cmd.CommandPath())
	}
	return cmd
}

func withTemporaryCapabilityOverride(t *testing.T, path string, entry capabilityEntry) {
	t.Helper()
	old, hadOld := capabilityOverrides[path]
	capabilityOverrides[path] = entry
	t.Cleanup(func() {
		if hadOld {
			capabilityOverrides[path] = old
		} else {
			delete(capabilityOverrides, path)
		}
	})
}

func assertPreconditionExitCode(t *testing.T, err error, want int) {
	t.Helper()
	var cliErr *cliError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error = %T %[1]v, want *cliError", err)
	}
	if cliErr.code != want {
		t.Fatalf("cli error code = %d, want %d", cliErr.code, want)
	}
	if got := ExitCode(err); got != want {
		t.Fatalf("ExitCode(error) = %d, want %d", got, want)
	}
}
