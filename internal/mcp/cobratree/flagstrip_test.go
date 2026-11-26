// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cobratree

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The write-safety gate flags stay reachable for
// MUTATING commands (so applies work over MCP) and are stripped from read-only
// commands. Formatting/ops globals and hidden globals stay stripped everywhere.
var writeGatingGlobalFlagNames = []string{
	"allow-destructive",
	"allow-zotero-cloud",
	"continue-on-error",
	"dry-run",
	"max-changes",
	"max-failures",
	"yes",
}

var formattingGlobalFlagNames = []string{
	"agent",
	"compact",
	"csv",
	"data-source",
	"human-friendly",
	"idempotent",
	"ignore-missing",
	"json",
	"no-cache",
	"no-color",
	"no-input",
	"plain",
	"quiet",
	"rate-limit",
	"select",
	"timeout",
}

func allGlobalFlagNames() []string {
	out := append([]string{}, writeGatingGlobalFlagNames...)
	return append(out, formattingGlobalFlagNames...)
}

var stripHiddenGlobalFlagNames = []string{
	"config",
	"deliver",
	"group",
	"profile",
}

// stripNewRoot builds a root with every global persistent flag plus a child.
// readOnly toggles the mcp:read-only annotation that decides whether the child
// is treated as mutating (write-gating flags exposed) or read-only (stripped).
func stripNewRoot(readOnly bool) (*cobra.Command, *cobra.Command) {
	root := &cobra.Command{Use: "zotero"}
	for _, name := range allGlobalFlagNames() {
		switch name {
		case "max-changes", "max-failures":
			root.PersistentFlags().Int(name, 0, "global "+name)
		default:
			root.PersistentFlags().Bool(name, false, "global "+name)
		}
	}

	child := &cobra.Command{
		Use: "child",
		Run: func(*cobra.Command, []string) {},
	}
	if readOnly {
		child.Annotations = map[string]string{ReadOnlyAnnotation: "true"}
	}
	child.Flags().String("limit", "", "local limit")
	root.AddCommand(child)

	return root, child
}

func TestStripMutatingKeepsWriteGatingAndLocalDropsFormatting(t *testing.T) {
	_, child := stripNewRoot(false)
	allowed := safeFlagNames(child)

	if _, ok := allowed["limit"]; !ok {
		t.Fatal("safeFlagNames(mutating child) missing local flag limit")
	}
	for _, name := range writeGatingGlobalFlagNames {
		if _, ok := allowed[name]; !ok {
			t.Errorf("safeFlagNames(mutating child) dropped write-gating flag %q", name)
		}
	}
	for _, name := range formattingGlobalFlagNames {
		if _, ok := allowed[name]; ok {
			t.Errorf("safeFlagNames(mutating child) exposed formatting global %q", name)
		}
	}
}

func TestStripReadOnlyDropsAllInheritedGlobals(t *testing.T) {
	_, child := stripNewRoot(true)
	allowed := safeFlagNames(child)

	if _, ok := allowed["limit"]; !ok {
		t.Fatal("safeFlagNames(read-only child) missing local flag limit")
	}
	for _, name := range allGlobalFlagNames() {
		if _, ok := allowed[name]; ok {
			t.Errorf("safeFlagNames(read-only child) exposed inherited global flag %q", name)
		}
	}
}

func TestStripSafeToolOptionsCounts(t *testing.T) {
	_, mut := stripNewRoot(false)
	if got, want := len(safeToolOptionsForFlags(mut)), 1+len(writeGatingGlobalFlagNames); got != want {
		t.Fatalf("safeToolOptionsForFlags(mutating) length = %d, want %d", got, want)
	}
	_, ro := stripNewRoot(true)
	if got, want := len(safeToolOptionsForFlags(ro)), 1; got != want {
		t.Fatalf("safeToolOptionsForFlags(read-only) length = %d, want %d", got, want)
	}
}

func TestStripValidateAcceptsWriteGatingOnMutating(t *testing.T) {
	_, child := stripNewRoot(false)
	allowed := safeFlagNames(child)

	for _, name := range writeGatingGlobalFlagNames {
		name := name
		t.Run(name, func(t *testing.T) {
			value := any(true)
			if allowed[name] != "bool" {
				value = float64(1)
			}
			if err := validateMirrorArguments(map[string]any{name: value}, allowed); err != nil {
				t.Fatalf("validateMirrorArguments rejected write-gating flag %q on mutating command: %v", name, err)
			}
		})
	}
}

func TestStripValidateRejectsWriteGatingOnReadOnly(t *testing.T) {
	_, child := stripNewRoot(true)
	allowed := safeFlagNames(child)

	for _, name := range writeGatingGlobalFlagNames {
		name := name
		t.Run(name, func(t *testing.T) {
			if err := validateMirrorArguments(map[string]any{name: true}, allowed); err == nil {
				t.Fatalf("validateMirrorArguments accepted write-gating flag %q on read-only command", name)
			}
		})
	}
}

func TestStripValidateRejectsFormattingGlobals(t *testing.T) {
	_, child := stripNewRoot(false)
	allowed := safeFlagNames(child)

	for _, name := range formattingGlobalFlagNames {
		name := name
		t.Run(name, func(t *testing.T) {
			if err := validateMirrorArguments(map[string]any{name: true}, allowed); err == nil {
				t.Fatalf("validateMirrorArguments accepted forged formatting global flag %q", name)
			}
		})
	}
}

func TestStripValidateRejectsRawFlagTokenInArgs(t *testing.T) {
	_, child := stripNewRoot(false)
	allowed := safeFlagNames(child)

	if err := validateMirrorArguments(map[string]any{"args": "--limit"}, allowed); err == nil {
		t.Fatal("validateMirrorArguments accepted raw flag token in args")
	}
}

func TestStripValidateAcceptsLocalFlag(t *testing.T) {
	_, child := stripNewRoot(false)
	allowed := safeFlagNames(child)

	if err := validateMirrorArguments(map[string]any{"limit": "5"}, allowed); err != nil {
		t.Fatalf("validateMirrorArguments rejected valid local flag: %v", err)
	}
}

// A non-string positional-args value is a malformed call, never a silent
// no-op: the executor would otherwise drop it and run a different command
// shape. Null stays absent (matching workflow_submit), every other
// non-string is refused with a clear error.
func TestStripValidateRejectsNonStringPositionalArgs(t *testing.T) {
	_, child := stripNewRoot(false)
	allowed := safeFlagNames(child)

	for name, value := range map[string]any{
		"numeric": float64(123),
		"boolean": true,
		"array":   []any{"KEY1"},
		"object":  map[string]any{"key": "KEY1"},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateMirrorArguments(map[string]any{"args": value}, allowed)
			if err == nil {
				t.Fatalf("validateMirrorArguments accepted %T positional args", value)
			}
			if !strings.Contains(err.Error(), "must be a string") {
				t.Fatalf("validateMirrorArguments error = %q, want it to name the string contract", err)
			}
		})
	}

	for name, args := range map[string]map[string]any{
		"absent":       {},
		"null":         {"args": nil},
		"empty string": {"args": ""},
		"string":       {"args": "KEY1"},
	} {
		t.Run("accepts "+name, func(t *testing.T) {
			if err := validateMirrorArguments(args, allowed); err != nil {
				t.Fatalf("validateMirrorArguments(%v) = %v, want nil", args, err)
			}
		})
	}
}

func TestStripValidateRejectsUnregisteredHiddenGlobals(t *testing.T) {
	_, child := stripNewRoot(false)
	allowed := safeFlagNames(child)

	for _, name := range stripHiddenGlobalFlagNames {
		name := name
		t.Run(name, func(t *testing.T) {
			if err := validateMirrorArguments(map[string]any{name: true}, allowed); err == nil {
				t.Fatalf("validateMirrorArguments accepted unregistered hidden global flag %q", name)
			}
		})
	}
}

func TestAnnotatedInheritedRouteFlagsAreExposedAndAccepted(t *testing.T) {
	root, child := stripNewRoot(false)
	root.PersistentFlags().String("via", "auto", "route")
	root.PersistentFlags().String("connector-target", "", "target")
	child.Annotations = map[string]string{
		inheritedMirrorFlagsAnnotation: "via,connector-target",
	}

	allowed := safeFlagNames(child)
	for _, name := range []string{"via", "connector-target"} {
		if _, ok := allowed[name]; !ok {
			t.Errorf("annotated route flag %q is not exposed", name)
			continue
		}
		if err := validateMirrorArguments(map[string]any{name: "connector"}, allowed); err != nil {
			t.Errorf("annotated route flag %q was rejected: %v", name, err)
		}
	}
}

func TestUnannotatedInheritedRouteFlagRemainsRejected(t *testing.T) {
	root, child := stripNewRoot(false)
	root.PersistentFlags().String("via", "auto", "route")
	allowed := safeFlagNames(child)
	if err := validateMirrorArguments(map[string]any{"via": "connector"}, allowed); err == nil {
		t.Fatal("unannotated inherited route flag was accepted")
	}
}

func TestAnnotatedUnsafeInheritedFlagRemainsRejected(t *testing.T) {
	root, child := stripNewRoot(false)
	root.PersistentFlags().String("config", "", "config")
	child.Annotations = map[string]string{inheritedMirrorFlagsAnnotation: "config"}
	if _, ok := safeFlagNames(child)["config"]; ok {
		t.Fatal("unsafe config flag was exposed through an annotation")
	}
}

// A read-only command that writes local files keeps --dry-run reachable and
// nothing else from the write-safety gate: an agent must be able to preview
// the file it would write, and has no apply gate to pass.
func TestStripFileWriterKeepsOnlyDryRun(t *testing.T) {
	_, child := stripNewRoot(true)
	child.Annotations[WritesFilesAnnotation] = "true"
	allowed := safeFlagNames(child)

	if _, ok := allowed["dry-run"]; !ok {
		t.Error("safeFlagNames(read-only file writer) dropped --dry-run")
	}
	for _, name := range writeGatingGlobalFlagNames {
		if name == "dry-run" {
			continue
		}
		if _, ok := allowed[name]; ok {
			t.Errorf("safeFlagNames(read-only file writer) exposed write-gating flag %q", name)
		}
	}
}

// A flag value whose JSON type does not match the Cobra flag is refused. The
// dangerous case is a string aimed at a bool flag: pflag gives a bool flag no
// value token, so a "--name value" pair would reparse the value as a new flag
// and smuggle in --deliver, --config, --group or --profile.
func TestValidateMirrorArgumentsRefusesFlagTypeMismatch(t *testing.T) {
	cmd := &cobra.Command{Use: "leaf"}
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().Int("limit", 0, "")
	cmd.Flags().Float64("ratio", 0, "")
	cmd.Flags().String("via", "", "")
	cmd.Flags().StringArray("tag", nil, "")
	allowed := safeFlagNames(cmd)

	refused := map[string]any{
		"dry-run": "--deliver=file:/tmp/x",
		"limit":   1.5,
		"ratio":   "high",
		"via":     true,
		"tag":     []any{"ok", 3.0},
	}
	for name, value := range refused {
		if err := validateMirrorArguments(map[string]any{name: value}, allowed); err == nil {
			t.Errorf("validateMirrorArguments accepted %T %v for flag --%s", value, value, name)
		}
	}
	for _, value := range []any{[]any{"--deliver=x"}, 1.0, map[string]any{"a": "b"}} {
		if err := validateMirrorArguments(map[string]any{"dry-run": value}, allowed); err == nil {
			t.Errorf("validateMirrorArguments accepted %T for bool flag --dry-run", value)
		}
	}
	accepted := map[string]any{
		"dry-run": false,
		"limit":   5.0,
		"ratio":   1.5,
		"via":     "",
		"tag":     []any{"a", ""},
	}
	for name, value := range accepted {
		if err := validateMirrorArguments(map[string]any{name: value}, allowed); err != nil {
			t.Errorf("validateMirrorArguments rejected %T %v for flag --%s: %v", value, value, name, err)
		}
	}
}

// command_run reaches the leaf through the shared validator, so a bool flag
// holding a string must be refused before any command runs and must never put
// a value token on argv.
func TestCommandRunRefusesStringForBoolFlag(t *testing.T) {
	root := &cobra.Command{Use: "zotio"}
	root.PersistentFlags().String("deliver", "", "")
	ran := false
	leaf := &cobra.Command{Use: "leaf", RunE: func(*cobra.Command, []string) error { ran = true; return nil }}
	leaf.Flags().Bool("dry-run", false, "")
	root.AddCommand(leaf)

	err := validateMirrorArguments(map[string]any{"dry-run": "--deliver=file:/tmp/x"}, safeFlagNames(leaf))
	if err == nil {
		t.Fatal("string value for bool flag was accepted")
	}
	if ran {
		t.Fatal("leaf ran")
	}
	for _, token := range cliArgsFromMCP(map[string]any{"via": "--deliver=file:/tmp/x", "dry-run": true}) {
		if strings.HasPrefix(token, "--deliver") {
			t.Fatalf("argv carries a standalone value-derived flag token %q", token)
		}
	}
}
