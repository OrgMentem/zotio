// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
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
	Style        string `json:"style,omitempty"`
	Count        int    `json:"count"`
	Bibliography string `json:"bibliography"`
	Format       string `json:"format,omitempty"`
}

type bibliographySelection struct {
	Keys             []string
	CiteKeys         map[string]string
	ItemTypes        map[string]string
	CiteKeysComplete bool
}

type bibliographyCiteKeyError struct {
	Missing    []string
	Duplicates []string
}

func (e *bibliographyCiteKeyError) Error() string {
	parts := make([]string, 0, 2)
	if len(e.Missing) > 0 {
		parts = append(parts, fmt.Sprintf("%d item(s) have no Better BibTeX citation key: %s", len(e.Missing), summarizeBibliographyValues(e.Missing)))
	}
	if len(e.Duplicates) > 0 {
		parts = append(parts, fmt.Sprintf("%d citation key(s) are duplicated: %s", len(e.Duplicates), summarizeBibliographyValues(e.Duplicates)))
	}
	return strings.Join(parts, "; ")
}

func summarizeBibliographyValues(values []string) string {
	const maxValues = 10
	if len(values) <= maxValues {
		return strings.Join(values, ", ")
	}
	return strings.Join(values[:maxValues], ", ") + fmt.Sprintf(", and %d more", len(values)-maxValues)
}

func newItemsBibliographyCmd(flags *rootFlags) *cobra.Command {
	var flagScope string
	var flagStyle string
	var flagFormat string

	cmd := &cobra.Command{
		Use:   "bibliography",
		Short: "Render or export a bibliography for a scoped item selection",
		Long: `Render or export a bibliography for items selected with the shared scope grammar.

The default bib format uses Zotero's Web API CSL renderer, so named styles such
as apa, chicago, mla, nature, and journal-specific CSL IDs are honored.
Machine-readable csljson, bibtex, biblatex, and ris formats use Zotero's export
translators. CSL-JSON ids are rewritten to unique Better BibTeX citation keys
so Pandoc and Quarto citations resolve against the output.

The Web API limits itemKey batches, so large scopes are fetched in stable
50-key chunks and merged in scope order.`,
		Example: `  zotio items bibliography --scope collection:ABCD1234 --style apa
  zotio items bibliography --scope tag:to-submit --format csljson
  zotio items bibliography --scope collection:ABCD1234 --format bibtex
  zotio items bibliography --scope item:ABCD1234 --json`,
		Annotations: map[string]string{"zotio:endpoint": "items.list", "zotio:method": "GET", "zotio:path": "/items", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageErr(fmt.Errorf("items bibliography takes no positional arguments"))
			}
			format, err := normalizeBibliographyFormat(flagFormat)
			if err != nil {
				return usageErr(err)
			}
			if format != "bib" && strings.TrimSpace(flagStyle) != "" {
				return usageErr(fmt.Errorf("--style applies only to --format bib"))
			}

			wc, err := webAPIReadClient(cmd, flags, "items bibliography")
			if err != nil {
				return err
			}

			spec, err := parseScopeSpec(flagScope)
			if err != nil {
				return usageErr(err)
			}
			selection, err := bibliographyScopeSelection(wc, spec, flags)
			if err != nil {
				return err
			}

			renderKeys := selection.Keys
			var citeKeys map[string]string
			if format == "csljson" {
				renderKeys, citeKeys, err = bibliographyCSLSelection(wc, selection, flags)
				if err != nil {
					var keyErr *bibliographyCiteKeyError
					if errors.As(err, &keyErr) {
						return emitPreconditionUnmetWithRemediation(
							cmd.OutOrStdout(),
							flags,
							"items bibliography --format csljson",
							preconditionBetterBibTeX,
							keyErr.Error(),
							[]string{
								"Run 'zotio library health --for citation' and fix every citekey_missing or citekey_conflict finding.",
								"Pin dynamic Better BibTeX citation keys when they are not present in Zotero's synced item data.",
								"Retry the bibliography export after Zotero syncs the corrected items.",
							},
						)
					}
					return err
				}
			}

			bibliography, err := renderedBibliography(wc, renderKeys, format, flagStyle, citeKeys, flags)
			if err != nil {
				return err
			}
			if format == "csljson" {
				if flags.quiet {
					return nil
				}
				// The format is already a complete machine-readable artifact.
				// Do not apply --agent compacting or table rendering to it.
				return printOutput(cmd.OutOrStdout(), bibliography, true)
			}

			text := string(bibliography)
			if flags.asJSON || flags.csv || flags.selectFields != "" {
				report := bibliographyReport{Count: len(selection.Keys), Bibliography: text}
				if format == "bib" {
					report.Style = bibliographyStyleLabel(flagStyle)
				} else {
					report.Format = format
				}
				return printJSONFiltered(cmd.OutOrStdout(), report, flags)
			}
			return printRawTextOutput(cmd, flags, text)
		},
	}
	cmd.Flags().StringVar(&flagScope, "scope", "library", "Shared scope expression (library, collection:KEY, tag:NAME, item:KEY, query:TEXT)")
	cmd.Flags().StringVar(&flagStyle, "style", "", "CSL style ID for --format bib (default uses Zotero's default bibliography style)")
	cmd.Flags().StringVar(&flagFormat, "format", "bib", "Output format: bib, csljson, bibtex, biblatex, or ris")

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

func normalizeBibliographyFormat(format string) (string, error) {
	format = strings.ToLower(strings.TrimSpace(format))
	if format == "" {
		format = "bib"
	}
	switch format {
	case "bib", "csljson", "bibtex", "biblatex", "ris":
		return format, nil
	default:
		return "", fmt.Errorf("invalid --format %q: must be bib, csljson, bibtex, biblatex, or ris", format)
	}
}

func bibliographyScopeSelection(c bibliographyGetter, spec scopeSpec, flags *rootFlags) (bibliographySelection, error) {
	switch spec.Type {
	case "item":
		return bibliographySelection{Keys: []string{spec.Value}}, nil
	case "saved-search":
		return bibliographySelection{}, preconditionErr(fmt.Errorf("scope %q needs the %s precondition; items bibliography renders through the Web API and cannot materialize saved searches", "saved-search:"+spec.Value, preconditionLiveLocalAPI))
	}

	path, params, err := bibliographyScopePath(spec)
	if err != nil {
		return bibliographySelection{}, err
	}
	selection := bibliographySelection{
		Keys:             make([]string, 0),
		CiteKeys:         make(map[string]string),
		ItemTypes:        make(map[string]string),
		CiteKeysComplete: true,
	}
	for start := 0; ; {
		pageParams := cloneStringMap(params)
		pageParams["format"] = "json"
		pageParams["limit"] = "100"
		if start > 0 {
			pageParams["start"] = strconv.Itoa(start)
		}
		data, err := c.Get(path, pageParams)
		if err != nil {
			return bibliographySelection{}, classifyAPIError(err, flags)
		}
		page, err := decodeBibliographySelection(data)
		if err != nil {
			return bibliographySelection{}, err
		}
		if len(page.Keys) == 0 {
			break
		}
		selection.Keys = append(selection.Keys, page.Keys...)
		for key, citeKey := range page.CiteKeys {
			selection.CiteKeys[key] = citeKey
			selection.ItemTypes[key] = page.ItemTypes[key]
		}
		if len(page.Keys) < 100 {
			break
		}
		start += len(page.Keys)
	}
	return selection, nil
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

func decodeBibliographySelection(data json.RawMessage) (bibliographySelection, error) {
	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		return bibliographySelection{}, fmt.Errorf("decoding scoped items: %w", err)
	}
	selection := bibliographySelection{
		Keys:             make([]string, 0, len(rows)),
		CiteKeys:         make(map[string]string, len(rows)),
		ItemTypes:        make(map[string]string, len(rows)),
		CiteKeysComplete: true,
	}
	for _, row := range rows {
		key := strings.TrimSpace(jsonStringFieldFromMap(row, "key"))
		if key == "" {
			continue
		}
		selection.Keys = append(selection.Keys, key)
		selection.CiteKeys[key] = resolveCiteKey(
			jsonStringFieldFromMap(row, "citationKey"),
			jsonStringFieldFromMap(row, "extra"),
		)
		selection.ItemTypes[key] = strings.TrimSpace(jsonStringFieldFromMap(row, "itemType"))
	}
	return selection, nil
}

func bibliographyCSLSelection(c bibliographyGetter, selection bibliographySelection, flags *rootFlags) ([]string, map[string]string, error) {
	metadata := selection
	if !selection.CiteKeysComplete {
		var err error
		metadata, err = fetchBibliographySelection(c, selection.Keys, flags)
		if err != nil {
			return nil, nil, err
		}
	}
	if metadata.CiteKeys == nil {
		metadata.CiteKeys = make(map[string]string)
	}

	keys := make([]string, 0, len(selection.Keys))
	for _, key := range selection.Keys {
		if bibliographyItemTypeIsCiteable(metadata.ItemTypes[key]) {
			keys = append(keys, key)
		}
	}
	missing := make([]string, 0)
	duplicateSet := make(map[string]bool)
	owners := make(map[string]string, len(metadata.CiteKeys))
	for _, key := range keys {
		citeKey := strings.TrimSpace(metadata.CiteKeys[key])
		if citeKey == "" {
			missing = append(missing, key)
			continue
		}
		if first, exists := owners[citeKey]; exists && first != key {
			duplicateSet[citeKey] = true
		} else {
			owners[citeKey] = key
		}
	}
	duplicates := make([]string, 0, len(duplicateSet))
	for citeKey := range duplicateSet {
		duplicates = append(duplicates, citeKey)
	}
	sort.Strings(missing)
	sort.Strings(duplicates)
	if len(missing) > 0 || len(duplicates) > 0 {
		return nil, nil, &bibliographyCiteKeyError{Missing: missing, Duplicates: duplicates}
	}
	return keys, metadata.CiteKeys, nil
}

func bibliographyItemTypeIsCiteable(itemType string) bool {
	switch itemType {
	case "attachment", "note", "annotation":
		return false
	default:
		return true
	}
}

func fetchBibliographySelection(c bibliographyGetter, keys []string, flags *rootFlags) (bibliographySelection, error) {
	selection := bibliographySelection{
		Keys:             make([]string, 0, len(keys)),
		CiteKeys:         make(map[string]string, len(keys)),
		ItemTypes:        make(map[string]string, len(keys)),
		CiteKeysComplete: true,
	}
	seen := make(map[string]bool, len(keys))
	for start := 0; start < len(keys); start += bibliographyChunkSize {
		end := start + bibliographyChunkSize
		if end > len(keys) {
			end = len(keys)
		}
		params := map[string]string{
			"format":  "json",
			"itemKey": strings.Join(keys[start:end], ","),
			"limit":   strconv.Itoa(end - start),
		}
		data, err := c.Get("/items", params)
		if err != nil {
			return bibliographySelection{}, classifyAPIError(err, flags)
		}
		page, err := decodeBibliographySelection(data)
		if err != nil {
			return bibliographySelection{}, err
		}
		for _, key := range page.Keys {
			seen[key] = true
			selection.Keys = append(selection.Keys, key)
			selection.CiteKeys[key] = page.CiteKeys[key]
			selection.ItemTypes[key] = page.ItemTypes[key]
		}
	}
	for _, key := range keys {
		if !seen[key] {
			return bibliographySelection{}, fmt.Errorf("citation-key metadata response omitted item %q", key)
		}
	}
	return selection, nil
}

func renderedBibliography(c bibliographyGetter, keys []string, format, style string, citeKeys map[string]string, flags *rootFlags) (json.RawMessage, error) {
	if format == "csljson" {
		merged := make([]json.RawMessage, 0, len(keys))
		for start := 0; start < len(keys); start += bibliographyChunkSize {
			end := start + bibliographyChunkSize
			if end > len(keys) {
				end = len(keys)
			}
			params := map[string]string{
				"format":  format,
				"itemKey": strings.Join(keys[start:end], ","),
				"limit":   strconv.Itoa(end - start),
			}
			data, err := c.Get("/items", params)
			if err != nil {
				return nil, classifyAPIError(err, flags)
			}
			page, err := rewriteCSLJSONCiteKeys(data, citeKeys, keys[start:end])
			if err != nil {
				return nil, err
			}
			merged = append(merged, page...)
		}
		data, err := json.Marshal(merged)
		if err != nil {
			return nil, fmt.Errorf("marshaling merged CSL-JSON: %w", err)
		}
		return data, nil
	}

	var out bytes.Buffer
	for start := 0; start < len(keys); start += bibliographyChunkSize {
		end := start + bibliographyChunkSize
		if end > len(keys) {
			end = len(keys)
		}
		params := map[string]string{
			"format":  format,
			"itemKey": strings.Join(keys[start:end], ","),
		}
		if format == "bib" {
			if strings.TrimSpace(style) != "" {
				params["style"] = strings.TrimSpace(style)
			}
		} else {
			params["limit"] = strconv.Itoa(end - start)
		}
		data, err := c.Get("/items", params)
		if err != nil {
			return nil, classifyAPIError(err, flags)
		}
		out.Write(data)
		if end < len(keys) && len(data) > 0 && data[len(data)-1] != '\n' {
			out.WriteByte('\n')
		}
	}
	return out.Bytes(), nil
}

func rewriteCSLJSONCiteKeys(data json.RawMessage, citeKeys map[string]string, scopedKeys []string) ([]json.RawMessage, error) {
	var entries []map[string]any
	trimmed := bytes.TrimSpace(data)
	switch {
	case len(trimmed) > 0 && trimmed[0] == '[':
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, fmt.Errorf("decoding CSL-JSON export array: %w", err)
		}
	case len(trimmed) > 0 && trimmed[0] == '{':
		var wrapped map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &wrapped); err != nil {
			return nil, fmt.Errorf("decoding CSL-JSON export object: %w", err)
		}
		if rawItems, ok := wrapped["items"]; ok {
			if err := json.Unmarshal(rawItems, &entries); err != nil {
				return nil, fmt.Errorf("decoding CSL-JSON export items: %w", err)
			}
		} else {
			var entry map[string]any
			if err := json.Unmarshal(trimmed, &entry); err != nil {
				return nil, fmt.Errorf("decoding CSL-JSON export entry: %w", err)
			}
			entries = []map[string]any{entry}
		}
	default:
		return nil, fmt.Errorf("decoding CSL-JSON export: expected an object or array")
	}

	requested := make(map[string]bool, len(scopedKeys))
	for _, key := range scopedKeys {
		requested[key] = true
	}
	byKey := make(map[string]json.RawMessage, len(entries))
	for _, entry := range entries {
		rawID, ok := entry["id"].(string)
		if !ok || strings.TrimSpace(rawID) == "" {
			fields := make([]string, 0, len(entry))
			for field := range entry {
				fields = append(fields, field)
			}
			sort.Strings(fields)
			return nil, fmt.Errorf("CSL-JSON entry has no Zotero item-key id; fields: %s", strings.Join(fields, ", "))
		}

		itemKey := rawID
		if !requested[itemKey] {
			// Zotero's CSL translator prefixes the item key with the numeric
			// library ID (for example, 12345/ABCD1234).
			if slash := strings.LastIndexByte(rawID, '/'); slash >= 0 {
				itemKey = rawID[slash+1:]
			}
		}
		if !requested[itemKey] {
			return nil, fmt.Errorf("CSL-JSON entry id %q does not match this export chunk", rawID)
		}
		citeKey := strings.TrimSpace(citeKeys[itemKey])
		if citeKey == "" {
			return nil, fmt.Errorf("CSL-JSON entry id %q has no scoped citation key", rawID)
		}
		if _, duplicate := byKey[itemKey]; duplicate {
			return nil, fmt.Errorf("CSL-JSON export returned item %q more than once", itemKey)
		}
		entry["id"] = citeKey
		raw, err := json.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("encoding CSL-JSON entry %q: %w", rawID, err)
		}
		byKey[itemKey] = raw
	}

	out := make([]json.RawMessage, 0, len(scopedKeys))
	for _, key := range scopedKeys {
		raw, ok := byKey[key]
		if !ok {
			return nil, fmt.Errorf("CSL-JSON export omitted scoped item %q", key)
		}
		out = append(out, raw)
	}
	return out, nil
}
