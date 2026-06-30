// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: shared connector/Web API route for item creation.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"zotero-pp-cli/internal/connector"
	"zotero-pp-cli/internal/store"
)

type itemPoster interface {
	Post(path string, body any) (json.RawMessage, int, error)
}

type itemCreateResult struct {
	Via         string
	WebKey      string
	WebData     json.RawMessage
	Session     string
	ConnKey     string
	OAPDFStatus string
	OAPDFTitle  string
	OAPDFError  string
}

// routeCreateItem creates one Zotero item via the selected route.
func routeCreateItem(ctx context.Context, flags *rootFlags, webClient itemPoster, item map[string]any, sourceURI string, collectionRequested bool) (itemCreateResult, error) {
	via, err := flags.resolveCreateVia(ctx, collectionRequested)
	if err != nil {
		return itemCreateResult{}, err
	}
	switch via {
	case "web":
		if webClient == nil {
			return itemCreateResult{}, fmt.Errorf("missing Web API write client")
		}
		data, _, err := webClient.Post("/items", []map[string]any{item})
		if err != nil {
			return itemCreateResult{}, classifyAPIError(err, flags)
		}
		createdKey, ok := createdItemKey(data)
		if !ok {
			return itemCreateResult{}, fmt.Errorf("could not read created item key from /items response")
		}
		return itemCreateResult{Via: "web", WebKey: createdKey, WebData: data}, nil
	case "connector":
		conn, err := flags.newConnector()
		if err != nil {
			return itemCreateResult{}, err
		}
		target, err := resolveConnectorTargetForItem(ctx, flags, conn, item, collectionRequested)
		if err != nil {
			if flags.via == "connector" {
				return itemCreateResult{}, err
			}
			if webClient == nil {
				return itemCreateResult{}, err
			}
			data, _, err := webClient.Post("/items", []map[string]any{item})
			if err != nil {
				return itemCreateResult{}, classifyAPIError(err, flags)
			}
			createdKey, ok := createdItemKey(data)
			if !ok {
				return itemCreateResult{}, fmt.Errorf("could not read created item key from /items response")
			}
			return itemCreateResult{Via: "web", WebKey: createdKey, WebData: data}, nil
		}
		sessionID, err := connector.NewID()
		if err != nil {
			return itemCreateResult{}, err
		}
		connectorKey, err := connector.NewID()
		if err != nil {
			return itemCreateResult{}, err
		}
		item["id"] = connectorKey
		if err := conn.SaveItems(ctx, sessionID, sourceURI, []map[string]any{item}); err != nil {
			return itemCreateResult{}, err
		}
		if target != "" {
			if err := conn.UpdateSession(ctx, sessionID, target, nil, ""); err != nil {
				return itemCreateResult{}, err
			}
		}
		return itemCreateResult{Via: "connector", Session: sessionID, ConnKey: connectorKey}, nil
	default:
		return itemCreateResult{}, fmt.Errorf("unsupported create route %q", via)
	}
}

// attachResolverPDF adds an open-access PDF to a connector-created item when
// Zotero reports an attachment resolver for the same save session.
func attachResolverPDF(ctx context.Context, flags *rootFlags, res *itemCreateResult) {
	if res == nil || res.Via != "connector" || res.Session == "" || res.ConnKey == "" {
		return
	}
	conn, err := flags.newConnector()
	if err != nil {
		res.OAPDFStatus = "error"
		res.OAPDFError = err.Error()
		return
	}
	ok, err := conn.HasAttachmentResolvers(ctx, res.Session, res.ConnKey)
	if err != nil {
		res.OAPDFStatus = "error"
		res.OAPDFError = err.Error()
		return
	}
	if !ok {
		res.OAPDFStatus = "none"
		return
	}
	title, err := conn.SaveAttachmentFromResolver(ctx, res.Session, res.ConnKey)
	if err != nil {
		res.OAPDFStatus = "error"
		res.OAPDFError = err.Error()
		return
	}
	res.OAPDFStatus = "attached"
	res.OAPDFTitle = title
}

func printCreateResult(cmd *cobra.Command, flags *rootFlags, res itemCreateResult, webData json.RawMessage) error {
	if res.Via != "connector" {
		return printOutputWithFlags(cmd.OutOrStdout(), webData, flags)
	}
	if flags.asJSON || flags.agent {
		payload := map[string]any{"via": "connector", "status": "created", "key": nil}
		if res.OAPDFStatus != "" {
			payload["oa_pdf"] = map[string]any{"status": res.OAPDFStatus, "title": res.OAPDFTitle, "error": res.OAPDFError}
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		return printOutput(cmd.OutOrStdout(), json.RawMessage(data), true)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Created in desktop Zotero (key assigned on save; syncs on next sync).")
	if res.OAPDFStatus != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "OA PDF: %s", res.OAPDFStatus)
		if res.OAPDFTitle != "" {
			fmt.Fprintf(cmd.OutOrStdout(), " (%s)", res.OAPDFTitle)
		}
		if res.OAPDFError != "" {
			fmt.Fprintf(cmd.OutOrStdout(), " — %s", res.OAPDFError)
		}
		fmt.Fprintln(cmd.OutOrStdout())
	}
	return nil
}

func itemCreateSourceURI(item map[string]any) string {
	if doi, ok := item["DOI"].(string); ok {
		if doi = strings.TrimSpace(doi); doi != "" {
			return "https://doi.org/" + doi
		}
	}
	if urlValue, ok := item["url"].(string); ok {
		return strings.TrimSpace(urlValue)
	}
	return ""
}

func importEntrySourceURL(entry importManifestEntry, item map[string]any) string {
	if strings.EqualFold(strings.TrimSpace(entry.IdentifierType), "doi") {
		if id := strings.TrimSpace(entry.Identifier); id != "" {
			return "https://doi.org/" + id
		}
	}
	return itemCreateSourceURI(item)
}

// refreshItemsFromLocalAPI performs a best-effort incremental sync after a
// connector write so local-store reads can see desktop-created items promptly.
func refreshItemsFromLocalAPI(ctx context.Context, flags *rootFlags) {
	c, err := flags.newClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: store refresh skipped: %v\n", err)
		return
	}
	c.NoCache = true
	db, err := store.OpenWithContext(ctx, defaultDBPath("zotero-pp-cli"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: store refresh skipped: %v\n", err)
		return
	}
	defer db.Close()
	oldHumanFriendly := humanFriendly
	humanFriendly = true
	defer func() { humanFriendly = oldHumanFriendly }()
	res := syncResource(c, db, "items", 0, false, 1000, false)
	if res.Err != nil {
		fmt.Fprintf(os.Stderr, "warning: store refresh skipped: %v\n", res.Err)
	} else if res.Warn != nil {
		fmt.Fprintf(os.Stderr, "warning: store refresh warning: %v\n", res.Warn)
	}
}
