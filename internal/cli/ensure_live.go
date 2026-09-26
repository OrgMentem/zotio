// Copyright 2026 OrgMentem and contributors. Licensed under MIT. See LICENSE.
// share cross-platform Zotero desktop launch and local-API readiness checks.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"zotio/internal/client"
	"zotio/internal/cliutil"

	"github.com/spf13/cobra"
)

// centralize OS-specific desktop URI launch commands for tests and reuse.
func launchCommand(goos, uri string) (name string, args []string) {
	switch goos {
	case "darwin":
		return "open", []string{uri}
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", uri}
	case "linux":
		return "xdg-open", []string{uri}
	default:
		return "xdg-open", []string{uri}
	}
}

// launchURITimeout bounds a desktop URI launch when the caller has no
// deadline of its own (items open). ensureLive supplies its own 15s overall
// deadline, which still caps the launch via parent cancellation.
const launchURITimeout = 15 * time.Second

// provide one side-effect-gated URI launcher for desktop integrations.
// The command context bounds the child: cancellation kills it instead of
// hanging on a stalled OS URI handler.
func launchURI(ctx context.Context, uri string) error {
	if cliutil.IsVerifyEnv() {
		fmt.Fprintf(os.Stdout, "would open: %s\n", uri)
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, launchURITimeout)
		defer cancel()
	}
	name, args := launchCommand(runtime.GOOS, uri)
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil {
		return fmt.Errorf("launching URI %q: %w", uri, err)
	}
	return nil
}

// classify Zotero local API reachability by transport success, not HTTP status.
//
// ProbeGet, not Get: Get reads through the response cache, which serves any GET
// younger than 5 minutes straight off disk with no network contact. A probe that
// answers from the cache reports the desktop reachable after it has exited, so
// doctor --ensure-live and init never offer the --launch remediation and the
// live_local_api precondition passes for a plane nothing can reach.
func localAPIReachable(c *client.Client) bool {
	_, err := c.ProbeGet("/")
	if err == nil {
		return true
	}
	var apiErr *client.APIError
	return errors.As(err, &apiErr)
}

// implement the doctor --ensure-live precondition remediation primitive.
func ensureLive(cmd *cobra.Command, flags *rootFlags, launch bool) error {
	c, err := flags.newClient()
	if err != nil {
		return err
	}
	if localAPIReachable(c) {
		if flags.asJSON {
			return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(`{"status":"reachable"}`), flags)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Zotero local API: reachable")
		return nil
	}
	if !launch {
		return preconditionErr(fmt.Errorf("Zotero desktop / local API not reachable; pass --launch to start it, or open Zotero and enable Settings -> Advanced -> 'Allow other applications to communicate with Zotero'"))
	}
	if cliutil.IsVerifyEnv() {
		return nil
	}
	// Start the 15s overall deadline before invoking the OS launcher so a
	// stalled open/xdg-open cannot hang forever: the launch child inherits
	// this context and is killed on cancellation or deadline.
	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
	defer cancel()
	if err := launchURI(ctx, "zotero://select/library"); err != nil {
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return preconditionErr(fmt.Errorf("launched Zotero but the local API did not become reachable within 15s; ensure Settings -> Advanced -> 'Allow other applications' is enabled"))
			}
			return ctx.Err()
		}
		return preconditionErr(fmt.Errorf("launching Zotero: %w", err))
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return preconditionErr(fmt.Errorf("launched Zotero but the local API did not become reachable within 15s; ensure Settings -> Advanced -> 'Allow other applications' is enabled"))
			}
			return ctx.Err()
		case <-ticker.C:
			if localAPIReachable(c) {
				if flags.asJSON {
					return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(`{"status":"reachable"}`), flags)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Zotero local API: reachable")
				return nil
			}
		}
	}
}
