// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestWorkflowMcpMixedReaderDestructiveConsent(t *testing.T) {
	for _, mode := range []string{workflowRunModePreview, workflowRunModeApply} {
		t.Run(mode, func(t *testing.T) {
			reads, writes := 0, 0
			factory := func() *cobra.Command {
				flags := &rootFlags{maxChanges: -1}
				root := &cobra.Command{Use: "zotio"}
				root.PersistentFlags().BoolVar(&flags.allowDestructive, "allow-destructive", false, "")
				root.PersistentFlags().BoolVar(&flags.yes, "yes", false, "")
				root.PersistentFlags().BoolVar(&flags.dryRun, "dry-run", false, "")
				root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
					return rejectWriteFlagsOnReadCommand(cmd, flags)
				}
				items := &cobra.Command{Use: "items"}
				items.AddCommand(&cobra.Command{
					Use: "list", Annotations: map[string]string{"mcp:read-only": "true"},
					Run: func(_ *cobra.Command, _ []string) { reads++ },
				})
				root.AddCommand(items, &cobra.Command{
					Use: "merge", Annotations: map[string]string{"mcp:read-only": "false"},
					RunE: func(_ *cobra.Command, _ []string) error {
						if !flags.allowDestructive {
							return fmt.Errorf("step lost its destructive declaration")
						}
						if flags.yes && !flags.dryRun {
							writes++
						}
						return nil
					},
				})
				return root
			}
			report, err := executeWorkflowRunSpecWithRootFactory(context.Background(), workflowRunSpec{
				Steps: []workflowRunStepSpec{
					{Name: "read", Args: []string{"items", "list"}},
					{Name: "merge", Args: []string{"merge", "--allow-destructive"}},
				},
			}, workflowRunExecution{Mode: mode, AllowDestructive: true}, factory)
			wantWrites := 0
			if mode == workflowRunModeApply {
				wantWrites = 1
			}
			if err != nil || !report.OK || reads != 1 || writes != wantWrites {
				t.Fatalf("report=%+v error=%v reads=%d writes=%d", report, err, reads, writes)
			}
		})
	}
}

func TestWorkflowMcpTriggersForwardDestructiveConsent(t *testing.T) {
	for _, source := range []string{"watch", "tail"} {
		for _, allow := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/allow=%t", source, allow), func(t *testing.T) {
				lib := &woLibrary{version: 1, items: map[string]woObject{
					"ABCD2345": woItem("ABCD2345", 1, "Example", false),
				}}
				woServe(t, lib)
				// version is an introspection command with no side effects. Its
				// declaration exercises the runner's consent gate without a write.
				specPath := writeWorkflowRunTestSpec(t, workflowRunSpec{Steps: []workflowRunStepSpec{
					{Name: "declared-destructive", Args: []string{"version", "--allow-destructive"}},
				}})
				args := []string{source, "items", "--workflow", specPath, "--yes"}
				if source == "watch" {
					args = append(args, "--once", "--workflow-on-change")
				} else {
					args = append(args, "--follow=false")
				}
				if allow {
					args = append(args, "--allow-destructive")
				}
				cmd := RootCmd()
				var out bytes.Buffer
				cmd.SetOut(&out)
				cmd.SetErr(&out)
				cmd.SetArgs(args)
				err := ExecuteRootInProcess(context.Background(), cmd)
				if allow {
					if err != nil || !strings.Contains(out.String(), "workflow apply ok") {
						t.Fatalf("approved trigger error=%v output=%s", err, out.String())
					}
				} else if ExitCode(err) != 9 || !strings.Contains(err.Error(), "declared-destructive") {
					t.Fatalf("unapproved trigger error=%v output=%s", err, out.String())
				}
			})
		}
	}
}

func TestWorkflowMcpTailConsentRequiresWorkflow(t *testing.T) {
	cmd := RootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"tail", "items", "--follow=false", "--allow-destructive"})
	err := ExecuteRootInProcess(context.Background(), cmd)
	if ExitCode(err) != 2 || !strings.Contains(err.Error(), "--allow-destructive requires --workflow") {
		t.Fatalf("tail consent error=%v output=%s", err, out.String())
	}
}
