// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Expose read-only Zotero desktop translator diagnostics.

package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

func newImportTranslatorsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "translators <url>",
		Short: "Detect Zotero desktop translators for a URL",
		Long: `Fetch a page and ask Zotero desktop which web translators match it.

This is diagnostic-only: Zotero's desktop connector can detect matching web
translators, but browser-side web translation is not run by the desktop server.`,
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			pageURL := strings.TrimSpace(args[0])
			conn, err := flags.newConnector()
			if err != nil {
				return preconditionErr(err)
			}
			html, err := fetchTranslatorHTML(cmd.Context(), pageURL, flags)
			if err != nil {
				return err
			}
			matches, err := conn.DetectTranslators(cmd.Context(), pageURL, html)
			if err != nil {
				return preconditionErr(fmt.Errorf("desktop connector translator detection failed: %w", err))
			}
			if flags.asJSON || flags.agent || flags.csv || flags.plain {
				return printJSONFiltered(cmd.OutOrStdout(), map[string]any{
					"url":        pageURL,
					"matches":    matches,
					"count":      len(matches),
					"diagnostic": "detects matching translators only; desktop connector does not run browser-side web translators",
				}, flags)
			}
			if flags.quiet {
				return nil
			}
			rows := make([][]string, 0, len(matches))
			for _, match := range matches {
				rows = append(rows, []string{match.Label, match.TranslatorID, fmt.Sprint(match.Priority)})
			}
			return flags.printTable(cmd, []string{"LABEL", "ID", "PRIORITY"}, rows)
		},
	}
	return cmd
}

func fetchTranslatorHTML(ctx context.Context, pageURL string, flags *rootFlags) (string, error) {
	// Translator diagnostics fetch caller-supplied
	// URLs, so apply the same public HTTP(S) + redirect gate as import url.
	if err := validateExternalHTTPURL(pageURL, false); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "zotio/1.0 translator-diagnostics")
	client := externalFetchHTTPClient(&http.Client{Timeout: flags.timeout}, false)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", pageURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetching %s: HTTP %d", pageURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", pageURL, err)
	}
	return string(body), nil
}
