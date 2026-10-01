// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func newWatchCmd(flags *rootFlags) *cobra.Command {
	var interval time.Duration
	var once bool
	var health bool
	var healthFor string
	var healthWebhook string
	var healthCheckRetractions bool
	var healthScope string
	var workflowPath string
	var workflowOnChange bool
	cmd := &cobra.Command{
		Use:         "watch [resource...]",
		Short:       "Keep the local store fresh with periodic incremental syncs",
		Annotations: map[string]string{"zotio:method": "GET", "zotio:preflight": "skip"},
		Long: `Watch keeps the local store fresh by running incremental sync cycles on

a configurable interval. It starts with an immediate sync, logs one concise
status line per cycle to stderr, and exits gracefully on SIGINT or SIGTERM.

Each resource argument takes a name that sync --resources accepts, such as
items or collections; with none, watch syncs the default set. An unknown name
exits 2 before the first cycle.

When --workflow <spec.json> is set, watch runs the workflow after every
successful sync cycle. It previews unless this watch invocation carries --yes.
A failed applied run leaves its checkpoint: subsequent applied triggers refuse
until it is resumed or deleted with zotio workflow run <spec> --yes --resume.

Add --workflow-on-change to run the workflow only after a cycle that changed
the mirror: a row stored, rewritten with different content, or reaped because
its object no longer exists upstream. An unchanged cycle logs that it skipped
the workflow. A cycle whose sync failed, or in which any resource failed, never
runs it; its changes wait for the next complete cycle.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if interval < 10*time.Second {
				return usageErr(fmt.Errorf("--interval must be at least 10s"))
			}
			// Reject an unknown resource name once, before any cycle runs:
			// watch forwards these to sync every interval, and a name sync
			// cannot dispatch would otherwise fail every cycle. Same
			// dispatch as syncResource: dependent schema resources first,
			// then the flat resource registry that tail also validates against.
			for _, resource := range args {
				if _, dependent := dependentSchemaSyncResources[resource]; dependent {
					continue
				}
				if _, err := syncResourcePath(resource); err != nil {
					return usageErr(err)
				}
			}
			if workflowOnChange && workflowPath == "" {
				return usageErr(fmt.Errorf("--workflow-on-change requires --workflow"))
			}
			if workflowPath != "" {
				if _, err := readWorkflowRunSpec(workflowPath); err != nil {
					return err
				}
			}

			healthMonitor, err := newWatchHealthMonitorWithContext(cmd.Context(), flags, health, healthFor, healthWebhook)
			if err != nil {
				return err
			}
			if !health && (cmd.Flags().Changed("health-for") || cmd.Flags().Changed("health-webhook")) {
				return usageErr(fmt.Errorf("--health-for and --health-webhook require --health"))
			}
			if !health && healthCheckRetractions {
				return usageErr(fmt.Errorf("--health-check-retractions requires --health"))
			}
			if !health && cmd.Flags().Changed("health-scope") {
				return usageErr(fmt.Errorf("--health-scope requires --health"))
			}
			if err := healthMonitor.setScope(healthScope); err != nil {
				return err
			}
			healthMonitor.enableRetractionCheck(healthCheckRetractions)

			// Isolate each watch tick by constructing a fresh sync command, matching
			// the one-shot CLI path while keeping watch-mode cancellation and logging
			// local to this wrapper.
			//
			// pending holds mirror changes no workflow has handled yet. A cycle that
			// fails after storing rows still adds them, so the next complete cycle
			// runs the workflow even when it finds nothing new itself.
			var pending syncChangeSummary
			runCycle := func(ctx context.Context) error {
				var summary *syncChangeSummary
				if workflowOnChange {
					summary = &syncChangeSummary{}
				}
				syncCmd := newSyncCmdWithSummary(flags, summary)
				syncCmd.SilenceErrors, syncCmd.SilenceUsage = true, true
				syncCmd.SetArgs(watchSyncArgs(args))
				syncCmd.SetOut(cmd.OutOrStdout())
				syncCmd.SetErr(cmd.ErrOrStderr())
				err := syncCmd.ExecuteContext(ctx)
				now := time.Now().UTC()
				if summary != nil {
					pending.Upserted += summary.Upserted
					pending.Deleted += summary.Deleted
				}
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "[watch] %s cycle error: %v\n", now.Format(time.RFC3339), err)
					return err
				}
				healthMonitor.run(ctx, cmd, now)
				if workflowPath != "" {
					if reason := watchWorkflowSkipReason(summary, pending); reason != "" {
						fmt.Fprintf(cmd.ErrOrStderr(), "[watch] %s workflow skipped: %s\n", now.Format(time.RFC3339), reason)
					} else {
						if summary != nil {
							fmt.Fprintf(cmd.ErrOrStderr(), "[watch] %s library changed: %d upserted, %d deleted row(s)\n",
								now.Format(time.RFC3339), pending.Upserted, pending.Deleted)
						}
						if werr := runTriggeredWorkflowE(ctx, cmd, "watch", workflowPath, workflowRunInvocation{
							Yes:     flags.yes,
							DryRun:  flags.dryRun,
							Agent:   flags.agent,
							NoInput: flags.noInput,
						}); werr != nil {
							fmt.Fprintf(cmd.ErrOrStderr(), "[watch] %s cycle error: triggered workflow failed: %v\n", now.Format(time.RFC3339), werr)
							return werr
						}
						pending = syncChangeSummary{}
					}
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "[watch] %s cycle complete\n", now.Format(time.RFC3339))
				return nil
			}

			if once {
				return runCycle(cmd.Context())
			}

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
			defer signal.Stop(sig)

			go func() {
				select {
				case <-sig:
					cancel()
				case <-ctx.Done():
				}
			}()

			lastCycleErr := runCycle(ctx)
			hadSuccessfulCycle := lastCycleErr == nil

			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					if !hadSuccessfulCycle && lastCycleErr != nil {
						return fmt.Errorf("watch stopped without a successful sync cycle: %w", lastCycleErr)
					}
					return nil
				case <-ticker.C:
					lastCycleErr = runCycle(ctx)
					hadSuccessfulCycle = hadSuccessfulCycle || lastCycleErr == nil
				}
			}
		},
	}

	cmd.Flags().DurationVar(&interval, "interval", 5*time.Minute, "Sync interval")
	cmd.Flags().BoolVar(&once, "once", false, "Run one sync cycle and exit")
	cmd.Flags().BoolVar(&health, "health", false, "Run quick library health checks after each successful sync")
	cmd.Flags().StringVar(&healthFor, "health-for", "quick", "Health preset for --health: quick, citation, systematic-review, vault, all")
	cmd.Flags().StringVar(&healthWebhook, "health-webhook", "", "POST health drift JSON to this webhook URL")
	cmd.Flags().BoolVar(&healthCheckRetractions, "health-check-retractions", false, "With --health: also run the Crossref retraction check each cycle (same as library health --check-retractions); each DOI is re-checked at most once per 24h")
	cmd.Flags().StringVar(&healthScope, "health-scope", "library", "Health cohort for --health: "+scopeFlagUsageDefaultLibrary+"; resolved each cycle, and findings for items leaving the cohort count as resolved")
	cmd.Flags().StringVar(&workflowPath, "workflow", "", "Run this workflow after every successful sync; previews unless --yes, and failed applied runs require zotio workflow run <spec> --yes --resume")
	cmd.Flags().BoolVar(&workflowOnChange, "workflow-on-change", false, "With --workflow: run it only after a complete sync cycle that stored, rewrote, or reaped mirror rows; unchanged cycles log a skip")

	return cmd
}

func watchSyncArgs(args []string) []string {
	if len(args) == 0 {
		return []string{}
	}
	return []string{"--resources", strings.Join(args, ",")}
}

// watchWorkflowSkipReason says why a successful cycle does not run the
// workflow, or returns "" to run it. A nil summary is the default policy:
// run after every successful cycle.
func watchWorkflowSkipReason(summary *syncChangeSummary, pending syncChangeSummary) string {
	switch {
	case summary == nil:
		return ""
	case !summary.Complete:
		return fmt.Sprintf("sync incomplete (a resource failed); %d upserted and %d deleted row(s) wait for a complete cycle", pending.Upserted, pending.Deleted)
	case !pending.changed():
		return "library unchanged"
	default:
		return ""
	}
}

// runTriggeredWorkflowE runs a workflow and reports its outcome. The caller
// decides whether a failed workflow stops its sync or tail cycle.
func runTriggeredWorkflowE(ctx context.Context, cmd *cobra.Command, source, specPath string, inv workflowRunInvocation) error {
	report, err := runWorkflowRunFile(ctx, specPath, inv)
	mode := report.Mode
	if mode == "" {
		mode = workflowRunModePreview
		if inv.Yes && !inv.DryRun {
			mode = workflowRunModeApply
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "[%s] %s workflow %s failed: %v\n", source, now, mode, err)
		return err
	}
	if report.RunID != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "[%s] %s workflow %s ok run_id=%s\n", source, now, mode, report.RunID)
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "[%s] %s workflow %s ok\n", source, now, mode)
	return nil
}
