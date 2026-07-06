// Copyright 2026 OrgMentem and contributors. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean roadmap-phase2): share cross-platform Zotero desktop launch and local-API readiness checks.

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

// PATCH(glean roadmap-phase2): centralize OS-specific desktop URI launch commands for tests and reuse.
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

// PATCH(glean roadmap-phase2): provide one side-effect-gated URI launcher for desktop integrations.
func launchURI(uri string) error {
	if cliutil.IsVerifyEnv() {
		fmt.Fprintf(os.Stdout, "would open: %s\n", uri)
		return nil
	}
	name, args := launchCommand(runtime.GOOS, uri)
	if err := exec.Command(name, args...).Run(); err != nil {
		return fmt.Errorf("launching URI %q: %w", uri, err)
	}
	return nil
}

// PATCH(glean roadmap-phase2): classify Zotero local API reachability by transport success, not HTTP status.
func localAPIReachable(c *client.Client) bool {
	_, err := c.Get("/", nil)
	if err == nil {
		return true
	}
	var apiErr *client.APIError
	return errors.As(err, &apiErr)
}

// PATCH(glean roadmap-phase2): implement the doctor --ensure-live precondition remediation primitive.
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
	if err := launchURI("zotero://select/library"); err != nil {
		return preconditionErr(fmt.Errorf("launching Zotero: %w", err))
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
	defer cancel()
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
