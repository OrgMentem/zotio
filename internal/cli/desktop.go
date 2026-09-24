// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Zotero desktop presence: report whether Zotero is running, and wait for its
// connector to accept requests without polling while it is closed.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/config"
	"zotio/internal/connector"
	"zotio/internal/desktop"
)

// errDesktopWaitTimeout and errDesktopWaitStdinClosed are the context causes
// that tell a --timeout expiry and a closed --watch-stdin pipe apart from an
// interrupt.
var (
	errDesktopWaitTimeout     = errors.New("desktop wait timed out")
	errDesktopWaitStdinClosed = errors.New("stdin closed")
)

// Wait outcomes, the `outcome` field of `desktop wait` JSON.
const (
	desktopOutcomeReady       = "ready"
	desktopOutcomeTimeout     = "timeout"
	desktopOutcomeNoProfile   = "no_profile"
	desktopOutcomeWatchFailed = "watch_failed"
)

// desktopWaitResult is `desktop wait` JSON: the status that ended the wait,
// plus how it ended and after how long.
type desktopWaitResult struct {
	desktop.Status
	Outcome  string `json:"outcome"`
	WaitedMS int64  `json:"waited_ms"`
}

const desktopRunningDefinition = `Two signals, reported separately:

  running              Zotero's process is up: another process holds the
                       profile lock (.parentlock via fcntl on macOS and Linux,
                       parent.lock opened exclusively on Windows), or the
                       connector answered.
  connector_reachable  GET <connector>/ping answered 200 during this check.
                       Imports and every other connector write need this.

state is "ready" when the connector answers, "stopped" when neither signal
holds, and "starting" when the process holds its lock but the connector does
not answer. Zotero takes the lock about 3s after launch and its connector
listens a few seconds later, so "starting" is normal briefly after a launch;
it persists if the connector is disabled (Settings -> Advanced -> "Allow other
applications to communicate with Zotero"), moved to another port, or Zotero
is hung. evidence names the strongest signal: connector, profile_lock, none.`

func newDesktopCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "desktop",
		Short:       "Report whether Zotero desktop is running, or wait for it to start",
		Annotations: map[string]string{"mcp:read-only": "true"},
	}
	cmd.AddCommand(newDesktopStatusCmd(flags))
	cmd.AddCommand(newDesktopWaitCmd(flags))
	return cmd
}

func newDesktopStatusCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether Zotero desktop is running and its connector accepts requests",
		Long: `Report whether Zotero desktop is running and whether its connector accepts
requests. Cheap and local: it reads the profile lock of every discovered
Zotero profile and sends one ping to the local connector. It exits 0 whatever
it finds; read the fields, not the exit code.

` + desktopRunningDefinition + `

Profiles are discovered from profiles.ini in the platform's Zotero directory
(ZOTERO_PROFILE_DIR pins one); data_dir comes from the profile's prefs.js.`,
		Example: `  zotio desktop status
  zotio desktop status --agent`,
		Args:        cobra.NoArgs,
		Annotations: map[string]string{"mcp:read-only": "true", "zotio:preflight": "skip"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			prober, err := newDesktopProber(flags)
			if err != nil {
				return err
			}
			st := prober.Probe(cmd.Context())
			if flags.asJSON || flags.agent {
				return printJSONFiltered(cmd.OutOrStdout(), st, flags)
			}
			renderDesktopStatus(cmd.OutOrStdout(), st)
			return nil
		},
	}
}

func newDesktopWaitCmd(flags *rootFlags) *cobra.Command {
	var timeout time.Duration
	var watchStdin bool
	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Block until Zotero desktop's connector accepts requests",
		Long: `Block until Zotero desktop's connector accepts requests, then print the
status that proved it. If the connector already answers, it returns at once.

While Zotero is closed nothing runs on a timer: the command sleeps on
filesystem notifications for the Zotero profile and data directories and
probes only after a change. Once the profile lock is seen held, the connector
is re-checked on a capped backoff for up to 2 minutes, because it starts
listening a few seconds after the lock and its start writes no file. If Zotero
stays up with the connector silent after that, only filesystem changes cause
further checks.

` + desktopRunningDefinition + `

Exit codes and the JSON outcome field:
  0   outcome "ready": the connector answers.
  14  outcome "timeout": --timeout passed first; wait again.
  9   outcome "no_profile" (no Zotero profile found and the connector does not
      answer) or "watch_failed" (the directories could not be watched).
  10  the configured base URL is not a local Zotero, so there is no connector
      to wait for.
  1   interrupted (SIGINT/SIGTERM) or stdin closed under --watch-stdin; nothing
      is printed on stdout.

--timeout replaces the global request timeout for this command; a connector
ping is always bounded to 3s.`,
		Example: `  zotio desktop wait
  zotio desktop wait --agent --timeout 6h
  # Supervised: exit when the supervisor's pipe closes
  zotio desktop wait --agent --watch-stdin --timeout 1h`,
		Args: cobra.NoArgs,
		// mcp:hidden: it blocks for as long as Zotero stays closed, like
		// watch and tail, and cannot serve as a request/response MCP tool.
		Annotations: map[string]string{"mcp:read-only": "true", "mcp:hidden": "true", "zotio:preflight": "skip"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if timeout < 0 {
				return usageErr(fmt.Errorf("--timeout must not be negative, got %s", timeout))
			}
			prober, err := newDesktopProber(flags)
			if err != nil {
				return err
			}
			if prober.Ping == nil {
				return configErr(fmt.Errorf("desktop wait needs a local Zotero base URL: %w", prober.ConnectorErr))
			}

			ctx := cmd.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeoutCause(ctx, timeout, errDesktopWaitTimeout)
				defer cancel()
			}
			if watchStdin {
				var cancel context.CancelCauseFunc
				ctx, cancel = context.WithCancelCause(ctx)
				defer cancel(nil)
				stdin := cmd.InOrStdin()
				go func() {
					_, _ = io.Copy(io.Discard, stdin)
					cancel(errDesktopWaitStdinClosed)
				}()
			}

			jsonOut := flags.asJSON || flags.agent
			watchDirs := prober.Install.WatchDirs()
			if !jsonOut && isTerminal(cmd.ErrOrStderr()) {
				fmt.Fprintf(cmd.ErrOrStderr(), "Waiting for Zotero desktop to start (watching %d Zotero directories)...\n", len(watchDirs))
			}

			start := time.Now()
			st, waitErr := desktop.Wait(ctx, desktop.WaitOptions{Prober: prober, WatchDirs: watchDirs})
			result := desktopWaitResult{Status: st, WaitedMS: time.Since(start).Milliseconds()}

			var exitErr error
			switch {
			case waitErr == nil:
				result.Outcome = desktopOutcomeReady
			case errors.Is(waitErr, errDesktopWaitTimeout):
				result.Outcome = desktopOutcomeTimeout
				exitErr = timeoutErr(fmt.Errorf("Zotero desktop's connector did not answer within %s (state %q)", timeout, st.State))
			case errors.Is(waitErr, desktop.ErrNoProfile):
				result.Outcome = desktopOutcomeNoProfile
				exitErr = preconditionErr(desktopNoProfileError(st))
			case ctx.Err() != nil:
				// Interrupted or stdin closed: the caller is going away and
				// wants no answer on stdout.
				return fmt.Errorf("desktop wait canceled: %w", waitErr)
			default:
				result.Outcome = desktopOutcomeWatchFailed
				exitErr = preconditionErr(fmt.Errorf("cannot watch the Zotero directories for a start: %w", waitErr))
			}

			if jsonOut {
				if err := printJSONFiltered(cmd.OutOrStdout(), result, flags); err != nil {
					return err
				}
				return exitErr
			}
			if exitErr != nil {
				return exitErr
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Zotero desktop is ready: the connector answers at %s (waited %s).\n",
				st.ConnectorURL, (time.Duration(result.WaitedMS) * time.Millisecond).Round(100*time.Millisecond))
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Give up after this long and exit 14, e.g. 30m or 6h (0 = wait indefinitely)")
	cmd.Flags().BoolVar(&watchStdin, "watch-stdin", false, "Exit when stdin reaches end of file (for supervisors that hold a pipe open); input is discarded")
	return cmd
}

// newDesktopProber discovers the installation and resolves the connector
// from the configured base URL, the same resolution `import` uses. A base URL
// that is not a local Zotero leaves no connector to ping; that is reported in
// the status rather than failing it, because the profile lock still answers
// whether Zotero runs.
func newDesktopProber(flags *rootFlags) (*desktop.Prober, error) {
	cfg, err := config.Load(flags.configPath)
	if err != nil {
		return nil, configErr(err)
	}
	prober := &desktop.Prober{Install: desktop.Discover(), PingTimeout: desktop.DefaultPingTimeout}
	base, ok := connectorBaseFromAPIBase(cfg.BaseURL)
	if !ok {
		prober.ConnectorErr = fmt.Errorf("the desktop connector is only available with a local Zotero base URL")
		return prober, nil
	}
	conn := connector.New(base, desktop.DefaultPingTimeout)
	prober.ConnectorURL = base
	prober.Ping = func(ctx context.Context) error { return connectorPing(ctx, conn) }
	return prober, nil
}

func desktopNoProfileError(st desktop.Status) error {
	msg := "no Zotero desktop profile directory was found and the connector does not answer; install and start Zotero once, or set ZOTERO_PROFILE_DIR"
	if st.DiscoveryError != "" {
		msg += " (discovery: " + st.DiscoveryError + ")"
	}
	return errors.New(msg)
}

func renderDesktopStatus(w io.Writer, st desktop.Status) {
	switch st.State {
	case desktop.StateReady:
		fmt.Fprintf(w, "Zotero desktop: ready (the connector answers at %s)\n", st.ConnectorURL)
	case desktop.StateStarting:
		fmt.Fprintln(w, "Zotero desktop: starting (the process holds its profile lock; the connector does not answer yet)")
	default:
		fmt.Fprintln(w, "Zotero desktop: stopped")
	}
	if st.ConnectorError != "" {
		fmt.Fprintf(w, "Connector: %s\n", SanitizeForTerminal(st.ConnectorError))
	}
	for _, p := range st.Profiles {
		lock := string(p.Lock)
		switch {
		case p.Lock == desktop.LockHeld && p.LockPID > 0:
			lock = fmt.Sprintf("held by pid %d", p.LockPID)
		case p.LockError != "":
			lock = "error: " + p.LockError
		}
		fmt.Fprintf(w, "Profile: %s (lock %s)\n", SanitizeForTerminal(p.Path), SanitizeForTerminal(lock))
	}
	if len(st.Profiles) == 0 {
		fmt.Fprintln(w, "Profile: none found")
	}
	if st.DataDir != "" {
		fmt.Fprintf(w, "Data directory: %s\n", SanitizeForTerminal(st.DataDir))
	}
	if st.DiscoveryError != "" {
		fmt.Fprintf(w, "Discovery: %s\n", SanitizeForTerminal(strings.TrimSpace(st.DiscoveryError)))
	}
}
