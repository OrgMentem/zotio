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

	cmd := &cobra.Command{
		Use:         "watch [resource...]",
		Short:       "Keep the local store fresh with periodic incremental syncs",
		Annotations: map[string]string{"pp:method": "GET"},
		Long: `Watch keeps the local store fresh by running incremental sync cycles on

a configurable interval. It starts with an immediate sync, logs one concise
status line per cycle to stderr, and exits gracefully on SIGINT or SIGTERM.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if interval < 10*time.Second {
				return usageErr(fmt.Errorf("--interval must be at least 10s"))
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
				now := time.Now().UTC().Format(time.RFC3339)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "[watch] %s cycle error: %v\n", now, err)
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "[watch] %s cycle complete\n", now)
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

	return cmd
}
