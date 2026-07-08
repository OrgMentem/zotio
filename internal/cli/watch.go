// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

func newWatchCmd(flags *rootFlags) *cobra.Command {
	var interval time.Duration
	var once bool
	// PATCH(marketing-heroes-2): configure optional library-health drift checks after successful watch sync cycles.
	var health bool
	var healthFor string
	var healthWebhook string
	cmd := &cobra.Command{
		Use:         "watch [resource...]",
		Short:       "Keep the local store fresh with periodic incremental syncs",
		Annotations: map[string]string{"zotio:method": "GET"},
		Long: `Watch keeps the local store fresh by running incremental sync cycles on

a configurable interval. It starts with an immediate sync, logs one concise
status line per cycle to stderr, and exits gracefully on SIGINT or SIGTERM.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if interval < 10*time.Second {
				return usageErr(fmt.Errorf("--interval must be at least 10s"))
			}

			// PATCH(marketing-heroes-2): validate health drift options before entering the watch loop.
			healthMonitor, err := newWatchHealthMonitor(flags, health, healthFor, healthWebhook)
			if err != nil {
				return err
			}
			if !health && (cmd.Flags().Changed("health-for") || cmd.Flags().Changed("health-webhook")) {
				return usageErr(fmt.Errorf("--health-for and --health-webhook require --health"))
			}

			// PATCH(glean roadmap-phase7 watch-sync): isolate each watch tick by
			// constructing a fresh sync command, matching the one-shot CLI path while
			// keeping watch-mode cancellation and logging local to this wrapper.
			runCycle := func(ctx context.Context) error {
				syncCmd := newSyncCmd(flags)
				syncCmd.SetArgs(args)
				syncCmd.SetOut(cmd.OutOrStdout())
				syncCmd.SetErr(cmd.ErrOrStderr())

				err := syncCmd.ExecuteContext(ctx)
				now := time.Now().UTC()
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "[watch] %s cycle error: %v\n", now.Format(time.RFC3339), err)
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "[watch] %s cycle complete\n", now.Format(time.RFC3339))
				// PATCH(marketing-heroes-2): run health drift checks only after a successful sync cycle.
				healthMonitor.run(ctx, cmd, now)
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

			_ = runCycle(ctx)

			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return nil
				case <-ticker.C:
					_ = runCycle(ctx)
				}
			}
		},
	}

	cmd.Flags().DurationVar(&interval, "interval", 5*time.Minute, "Sync interval")
	cmd.Flags().BoolVar(&once, "once", false, "Run one sync cycle and exit")
	// PATCH(marketing-heroes-2): expose health drift notifications for watch mode.
	cmd.Flags().BoolVar(&health, "health", false, "Run quick library health checks after each successful sync")
	cmd.Flags().StringVar(&healthFor, "health-for", "quick", "Health preset for --health: quick, citation, systematic-review, all")
	cmd.Flags().StringVar(&healthWebhook, "health-webhook", "", "POST health drift JSON to this webhook URL")

	return cmd
}
