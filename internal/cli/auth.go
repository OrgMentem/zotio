// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"zotio/internal/cliutil"
	"zotio/internal/config"

	"github.com/spf13/cobra"
)

func newAuthCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage authentication for Zotero",
	}

	cmd.AddCommand(newAuthStatusCmd(flags))
	cmd.AddCommand(newAuthSetTokenCmd(flags))
	cmd.AddCommand(newAuthLogoutCmd(flags))

	return cmd
}

func newAuthStatusCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Short:   "Show authentication status",
		Example: "  zotio auth status",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(flags.configPath)
			if err != nil {
				return configErr(err)
			}

			w := cmd.OutOrStdout()
			header := cfg.AuthHeader()
			authed := header != ""
			// A keyless config is fine for the local desktop API, which needs
			// no key; doctor reports the same state as "not required".
			optional := !authed && isLocalZoteroAPI(cfg.BaseURL)
			if flags.asJSON {
				out := map[string]any{
					"authenticated": authed,
					"verified":      false,
					"source":        cfg.AuthSource,
					"config":        cfg.Path,
				}
				if optional {
					out["api_key"] = "not set (optional for the local API)"
				}
				if printErr := printJSONFiltered(w, out, flags); printErr != nil {
					return printErr
				}
				if !authed && !optional {
					return authErr(fmt.Errorf("no credentials configured"))
				}
				return nil
			}
			if optional {
				fmt.Fprintln(w, "api_key: not set (optional for the local API)")
				fmt.Fprintf(w, "  Config: %s\n", cfg.Path)
				return nil
			}
			if !authed {
				fmt.Fprintln(w, red("Not authenticated"))
				fmt.Fprintln(w, "")
				fmt.Fprintln(w, "Set your token:")
				fmt.Fprintln(w, "  export ZOTERO_API_KEY=\"your-token-here\"")
				fmt.Fprintf(w, "  printf %%s \"$ZOTERO_API_KEY\" | zotio auth set-token --stdin\n")
				return authErr(fmt.Errorf("no credentials configured"))
			}

			fmt.Fprintln(w, green("Credentials present (not verified)"))
			fmt.Fprintf(w, "  Source: %s\n", cfg.AuthSource)
			fmt.Fprintf(w, "  Config: %s\n", cfg.Path)
			return nil
		},
	}
}

func newAuthSetTokenCmd(flags *rootFlags) *cobra.Command {
	var fromStdin bool
	cmd := &cobra.Command{
		Use:     "set-token --stdin",
		Short:   "Save an API token to the config file",
		Example: "  printf %s \"$ZOTERO_API_KEY\" | zotio auth set-token --stdin",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usageErr(fmt.Errorf("refusing token on command line; pipe it with --stdin"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(flags.configPath)
			if err != nil {
				return configErr(err)
			}

			// do not accept API
			// keys as positional arguments, where they leak via shell history and
			// process listings. Read the secret from stdin only.
			if !fromStdin {
				return usageErr(fmt.Errorf("refusing token on command line; pipe it with --stdin"))
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			tokenBytes, readErr := readAllContext(ctx, cmd.InOrStdin())
			if readErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return fmt.Errorf("reading token from stdin: %w", ctxErr)
				}
				return authErr(fmt.Errorf("reading token from stdin: %w", readErr))
			}
			token := strings.TrimSpace(string(tokenBytes))
			if token == "" {
				return authErr(fmt.Errorf("empty token on stdin"))
			}

			// --dry-run reports the target and writes nothing.
			if flags.dryRun {
				if flags.asJSON {
					return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
						"saved":       false,
						"dry_run":     true,
						"config_path": cfg.Path,
					}, flags)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "dry run: would save the token to %s; nothing written\n", cfg.Path)
				return nil
			}

			// api_key auth: AuthHeader() reads the env-var-derived field, not
			// AccessToken. Writing the token to AccessToken would persist the
			// bytes but leave doctor reporting "not configured" — the slot the
			// header builder consults stays empty.
			//
			// A cancellation that landed while the read completed must not
			// still replace the stored credential: the operator already
			// interrupted this command.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("saving token: %w", ctxErr)
			}
			if err := cfg.SaveCredential(token); err != nil {
				return configErr(fmt.Errorf("saving token: %w", err))
			}

			// JSON envelope: {saved, config_path}.
			if flags.asJSON {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"saved":       true,
					"config_path": cfg.Path,
				}, flags)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Token saved to %s\n", cfg.Path)
			return nil
		},
	}
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "Read the API token from stdin")
	return cmd
}

// readAllContext reads r to EOF but returns ctx.Err() as soon as ctx is done.
// A blocked read on a pipe or terminal cannot be interrupted portably, so the
// read keeps running in its own goroutine and its result is discarded; the
// goroutine ends when the input closes or the process exits. What matters to
// the caller is that it stops waiting, so the writer lock its command holds is
// released by the surrounding wrapper instead of staying held behind a stalled
// producer.
func readAllContext(ctx context.Context, r io.Reader) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(r)
		done <- result{data, err}
	}()
	select {
	case res := <-done:
		return res.data, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func newAuthLogoutCmd(flags *rootFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "logout",
		Short:   "Clear stored credentials",
		Long:    "Previews by default: shows what would be cleared and changes nothing. Pass --yes to clear the stored API key from the config file and remove the credentials file.",
		Example: "  zotio auth logout\n  zotio auth logout --yes",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(flags.configPath)
			if err != nil {
				return configErr(err)
			}

			// Preview by default; --yes applies. --dry-run always previews.
			if !flags.yes || flags.dryRun {
				credentialsPath, err := cliutil.CredentialsFilePath()
				if err != nil {
					return configErr(fmt.Errorf("resolving credentials path: %w", err))
				}
				if flags.asJSON {
					out := map[string]any{
						"cleared":          false,
						"preview":          true,
						"config_path":      cfg.Path,
						"credentials_path": credentialsPath,
					}
					if flags.dryRun {
						out["dry_run"] = true
					}
					return printJSONFiltered(cmd.OutOrStdout(), out, flags)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Preview: would clear stored credentials in %s and remove %s. Pass --yes to apply.\n", cfg.Path, credentialsPath)
				return nil
			}

			if err := cfg.ClearTokens(); err != nil {
				return configErr(fmt.Errorf("clearing tokens: %w", err))
			}

			// ZOTERO_API_KEY is the only env var that can keep supplying a
			// credential after the config is cleared, so name it directly:
			// reporting it lets the JSON envelope and the human prose both
			// explain why `auth status` may still look authenticated.
			envStillSet := ""
			if os.Getenv("ZOTERO_API_KEY") != "" {
				envStillSet = "ZOTERO_API_KEY"
			}

			// JSON envelope: {cleared: true, note?: "<env_var> env var is still set"}.
			if flags.asJSON {
				out := map[string]any{"cleared": true}
				if envStillSet != "" {
					out["note"] = envStillSet + " env var is still set"
				}
				return printJSONFiltered(cmd.OutOrStdout(), out, flags)
			}

			if envStillSet != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Config cleared. Note: %s env var is still set.\n", envStillSet)
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Logged out. Credentials cleared.")
			return nil
		},
	}
}
