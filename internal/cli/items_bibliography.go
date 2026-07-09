// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"zotio/internal/client"

	"github.com/spf13/cobra"
)

const bibliographyChunkSize = 50

type bibliographyGetter interface {
	Get(path string, params map[string]string) (json.RawMessage, error)
}

type bibliographyReport struct {
	Style        string `json:"style"`
	Count        int    `json:"count"`
	Bibliography string `json:"bibliography"`
}

func newItemsBibliographyCmd(flags *rootFlags) *cobra.Command {
	var flagScope string
	var flagStyle string

	cmd := &cobra.Command{
		Use:   "bibliography",
		Short: "Render a formatted bibliography for a scoped item selection",
		Long: `Render a formatted bibliography for items selected with the shared scope grammar.

The bibliography is rendered by Zotero's Web API CSL renderer so named styles
such as apa, chicago, mla, nature, and journal-specific CSL IDs are honored.
The Web API limits itemKey batches, so large scopes are fetched in stable
50-key chunks and concatenated in scope order.`,
		Example: `  zotio items bibliography --scope collection:ABCD1234 --style apa
  zotio items bibliography --scope tag:to-submit --style nature
  zotio items bibliography --scope item:ABCD1234 --json`,
		Annotations: map[string]string{"zotio:endpoint": "items.list", "zotio:method": "GET", "zotio:path": "/items", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageErr(fmt.Errorf("items bibliography takes no positional arguments"))
			}

			wc, err := webAPIReadClient(cmd, flags, "items bibliography")
			if err != nil {
				return err
			}

			spec, err := parseScopeSpec(flagScope)
			if err != nil {
				return usageErr(err)
			}
			keys, err := bibliographyScopeKeys(wc, spec, flags)
			if err != nil {
				return err
			}

			bibliography, err := renderedBibliography(wc, keys, flagStyle, flags)
			if err != nil {
				return err
			}

			if flags.asJSON || flags.csv || flags.selectFields != "" {
				return printJSONFiltered(cmd.OutOrStdout(), bibliographyReport{
					Style:        bibliographyStyleLabel(flagStyle),
					Count:        len(keys),
					Bibliography: bibliography,
				}, flags)
			}
			return printRawTextOutput(cmd, flags, bibliography)
		},
	}
	cmd.Flags().StringVar(&flagScope, "scope", "library", "Shared scope expression (library, collection:KEY, tag:NAME, item:KEY, query:TEXT)")
	cmd.Flags().StringVar(&flagStyle, "style", "", "CSL style ID (default uses Zotero's default bibliography style)")

	return cmd
}

func webAPIReadClient(cmd *cobra.Command, flags *rootFlags, capability string) (*client.Client, error) {
	c, err := flags.newWebReadClient(cmd.Context())
	if errors.Is(err, errWebAPIKeyRequired) {
		return nil, webAPIKeyPrecondition(cmd, flags, capability)
	}
	return c, err
}

func webAPIKeyPrecondition(cmd *cobra.Command, flags *rootFlags, capability string) error {
	remediation := []string{
		"Export ZOTERO_API_KEY with a Zotero Web API key that can read the target library.",
		"Or save one with: printf %s \"$ZOTERO_API_KEY\" | zotio auth set-token --stdin",
	}
	message := fmt.Sprintf("%s requires the %s precondition for server-side CSL rendering", capability, preconditionWebAPIKey)
	if flags != nil && flags.asJSON && !flags.quiet {
		_ = json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
			"kind":         "precondition_unmet",
			"precondition": preconditionWebAPIKey,
			"title":        "Zotero Web API key required",
			"detail":       message,
			"remediation":  remediation,
		})
	}
	return preconditionErr(fmt.Errorf("%s; %s", message, remediation[0]))
}

func bibliographyStyleLabel(style string) string {
	style = strings.TrimSpace(style)
	if style == "" {
		return "default"
	}
	return style
}

func printRawTextOutput(cmd *cobra.Command, flags *rootFlags, text string) error {
	if flags != nil && flags.quiet {
		return nil
	}
	_, err := fmt.Fprint(cmd.OutOrStdout(), text)
	return err
}

func bibliographyScopeKeys(c bibliographyGetter, spec scopeSpec, flags *rootFlags) ([]string, error) {
	switch spec.Type {
	case "item":
		return []string{spec.Value}, nil
	case "saved-search":
		return nil, preconditionErr(fmt.Errorf("scope %q needs the %s precondition; items bibliography renders through the Web API and cannot materialize saved searches", "saved-search:"+spec.Value, preconditionLiveLocalAPI))
	}

	path, params, err := bibliographyScopePath(spec)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0)
	for start := 0; ; {
		pageParams := cloneStringMap(params)
		pageParams["format"] = "json"
		pageParams["limit"] = "100"
		if start > 0 {
			pageParams["start"] = strconv.Itoa(start)
		}
		data, err := c.Get(path, pageParams)
		if err != nil {
			return nil, classifyAPIError(err, flags)
		}
		pageKeys, err := decodeBibliographyKeys(data)
		if err != nil {
			return nil, err
		}
		if len(pageKeys) == 0 {
			break
		}
		keys = append(keys, pageKeys...)
		if len(pageKeys) < 100 {
			break
		}
		start += len(pageKeys)
	}
	return keys, nil
}

func bibliographyScopePath(spec scopeSpec) (string, map[string]string, error) {
	switch spec.Type {
	case "library":
		return "/items", map[string]string{}, nil
	case "collection":
		return "/collections/" + url.PathEscape(spec.Value) + "/items", map[string]string{}, nil
	case "tag":
		return "/items", map[string]string{"tag": spec.Value}, nil
	case "query":
		return "/items", map[string]string{"q": spec.Value}, nil
	default:
		return "", nil, usageErr(fmt.Errorf("unsupported bibliography scope %q; use library, collection:KEY, tag:NAME, item:KEY, or query:TEXT", spec.Type))
	}
}

func decodeBibliographyKeys(data json.RawMessage) ([]string, error) {
	var rows []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("decoding scoped item keys: %w", err)
	}
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Key != "" {
			keys = append(keys, row.Key)
		}
	}
	return keys, nil
}

func renderedBibliography(c bibliographyGetter, keys []string, style string, flags *rootFlags) (string, error) {
	var out strings.Builder
	for start := 0; start < len(keys); start += bibliographyChunkSize {
		end := start + bibliographyChunkSize
		if end > len(keys) {
			end = len(keys)
		}
		params := map[string]string{
			"format":  "bib",
			"itemKey": strings.Join(keys[start:end], ","),
		}
		if strings.TrimSpace(style) != "" {
			params["style"] = strings.TrimSpace(style)
		}
		data, err := c.Get("/items", params)
		if err != nil {
			return "", classifyAPIError(err, flags)
		}
		piece := string(data)
		out.WriteString(piece)
		if end < len(keys) && piece != "" && !strings.HasSuffix(piece, "\n") {
			out.WriteByte('\n')
		}
	}
	return out.String(), nil
}
