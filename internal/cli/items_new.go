// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// newItemsNewCmd creates schema-backed items from /items/new.
func newItemsNewCmd(flags *rootFlags) *cobra.Command {
	var flagItemType string
	var flagFields []string
	var flagStdin bool
	var flagCollection string

	cmd := &cobra.Command{
		Use:         "new --item-type <type>",
		Short:       "Create a schema-validated item from Zotero's blank item template",
		Annotations: map[string]string{"zotio:method": "POST", "zotio:path": "/items"},
		RunE: func(cmd *cobra.Command, args []string) error {
			itemType := strings.TrimSpace(flagItemType)
			if itemType == "" {
				return usageErr(fmt.Errorf("required flag %q not set", "item-type"))
			}

			template, err := fetchItemTemplate(cmd.Context(), flags, itemType)
			if err != nil {
				return err
			}
			item := make(map[string]any, len(template)+len(flagFields)+1)
			for key, value := range template {
				item[key] = value
			}

			for _, field := range flagFields {
				name, value, ok := strings.Cut(field, "=")
				name = strings.TrimSpace(name)
				if !ok || name == "" {
					return usageErr(fmt.Errorf("--field must be name=value, got %q", field))
				}
				item[name] = value
			}

			if flagStdin {
				stdinData, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("reading stdin: %w", err)
				}
				var stdinFields map[string]any
				if err := json.Unmarshal(stdinData, &stdinFields); err != nil {
					return usageErr(fmt.Errorf("parsing stdin JSON object: %w", err))
				}
				if stdinFields == nil {
					return usageErr(fmt.Errorf("--stdin must contain a JSON object of item fields"))
				}
				for key, value := range stdinFields {
					item[key] = value
				}
			}

			item["itemType"] = itemType
			if err := validateItemFields(template, item); err != nil {
				return usageErr(err)
			}
			addImportCollection(item, flagCollection)

			if flags.dryRun {
				return printImportDryRun(cmd, item, "items new ("+itemType+")", flags)
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}
			// Route item creates through the desktop connector when available.
			res, err := routeCreateItem(cmd.Context(), flags, c, item, itemCreateSourceURI(item), cmd.Flags().Changed("collection"))
			if err != nil {
				return err
			}
			if res.Via == "connector" {
				refreshItemsFromLocalAPI(cmd.Context(), flags)
			}
			return printCreateResult(cmd, flags, res, res.WebData)
		},
	}
	cmd.Flags().StringVar(&flagItemType, "item-type", "", "Zotero item type to create (e.g. journalArticle)")
	cmd.Flags().StringArrayVar(&flagFields, "field", nil, "Item field assignment name=value (repeatable)")
	cmd.Flags().BoolVar(&flagStdin, "stdin", false, "Read a JSON object of item fields from stdin")
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Collection key to add the item to")

	return cmd
}

// fetchItemTemplate retrieves Zotero Web API's schema-backed blank item template.
func fetchItemTemplate(ctx context.Context, flags *rootFlags, itemType string) (map[string]any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	c, err := newSchemaClient(flags)
	if err != nil {
		return nil, err
	}
	// The /items/new endpoint is global; the local API does not serve it. When
	// the CLI is pointed at the local API, redirect this template fetch to the Web
	// API using the same hybrid routing writes use (ResolveWriteBase), stripping
	// the library prefix since the endpoint is global. With no key,
	// ResolveWriteBase yields "" and the Get below fails into the precondition
	// error below.
	if c.ResolveWriteBase != nil {
		if base, rerr := c.ResolveWriteBase(); rerr == nil && base != "" {
			c.BaseURL = stripLibraryPrefix(base)
		}
		c.ResolveWriteBase = nil
	}
	// The template fetch is a read prerequisite for validation; it must execute
	// even under --dry-run (which only previews the item creation, a write).
	c.DryRun = false
	data, err := c.Get("/items/new", map[string]string{"itemType": itemType})
	if err != nil {
		return nil, preconditionErr(fmt.Errorf("schema-validated item creation needs the Zotero Web API (/items/new is not served by the local API); set an API key / run online: %w", err))
	}
	// Zotero's Web API GET /items/new returns a single template object.
	var template map[string]any
	if err := json.Unmarshal(data, &template); err != nil {
		return nil, fmt.Errorf("parsing /items/new response: %w", err)
	}
	if len(template) == 0 {
		return nil, fmt.Errorf("/items/new returned an empty template for %q", itemType)
	}
	return template, nil
}

// validateItemFields rejects fields absent from the schema template before POSTing.
func validateItemFields(template, item map[string]any) error {
	allowedSpecial := map[string]bool{
		"itemType":    true,
		"creators":    true,
		"tags":        true,
		"collections": true,
		"relations":   true,
	}
	var invalid []string
	for key := range item {
		if allowedSpecial[key] {
			continue
		}
		if _, ok := template[key]; !ok {
			invalid = append(invalid, key)
		}
	}
	if len(invalid) == 0 {
		return nil
	}
	sort.Strings(invalid)

	valid := make([]string, 0, len(template))
	for key := range template {
		if !allowedSpecial[key] {
			valid = append(valid, key)
		}
	}
	sort.Strings(valid)
	if len(valid) > 8 {
		valid = valid[:8]
	}

	return fmt.Errorf("unknown item field(s): %s; valid fields include: %s", strings.Join(invalid, ", "), strings.Join(valid, ", "))
}
