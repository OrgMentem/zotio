// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// shared connector/Web API route for item creation.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/connector"
	"zotio/internal/mutation"
	"zotio/internal/store"
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
	return routeCreateItemVia(ctx, flags, via, webClient, item, sourceURI, collectionRequested)
}

// singleItemCreate describes one proposed item create for the commands that
// resolve exactly one item from metadata: items new and import url/pmid/arxiv/isbn.
type singleItemCreate struct {
	// operation names the mutation, e.g. "import.pmid".
	operation string
	// key echoes the identifier the item was resolved from.
	key string
	// source labels where the metadata came from, e.g. "PubMed (12345)".
	source string
	// item is the proposed Zotero item body.
	item map[string]any
	// fetchPDF attaches an open-access PDF after the item is created.
	fetchPDF bool
}

// runSingleItemCreate previews the create by default and writes only under
// --yes, so every one-item importer shares the same gate, envelope, journal
// entry, and --max-changes accounting. The local mirror is refreshed when the
// desktop connector handled the write, since that path bypasses the Web API.
func runSingleItemCreate(cmd *cobra.Command, flags *rootFlags, spec singleItemCreate) error {
	var res itemCreateResult
	ops := []mutation.Op{{
		ID:   spec.operation,
		Key:  spec.key,
		Kind: "item_create",
		Changes: []mutation.Change{
			{Field: "source", Add: spec.source},
			{Field: "item", Add: spec.item},
		},
		Apply: func() (string, any, error) {
			c, err := flags.newClient()
			if err != nil {
				return "failed", nil, err
			}
			res, err = routeCreateItem(cmd.Context(), flags, c, spec.item, itemCreateSourceURI(spec.item), cmd.Flags().Changed("collection"))
			if err != nil {
				return "failed", nil, err
			}
			return "applied", map[string]any{"via": res.Via, "key": createdItemKeyOf(res)}, nil
		},
	}}
	if spec.fetchPDF {
		ops = append(ops, mutation.Op{
			ID:   spec.operation + ":resolver-pdf",
			Key:  spec.key,
			Kind: "attachment_create",
			Changes: []mutation.Change{{Field: "attachment", Add: map[string]any{
				"source":    "resolver",
				"condition": "when an open-access PDF resolver is available",
			}}},
			Apply: func() (string, any, error) {
				if res.Via != "connector" {
					return "failed", nil, preconditionErr(fmt.Errorf("--fetch-pdf requires the desktop connector; use --via connector"))
				}
				attachResolverPDF(cmd.Context(), flags, &res)
				detail := map[string]any{
					"status": res.OAPDFStatus,
					"title":  res.OAPDFTitle,
					"error":  res.OAPDFError,
				}
				if res.OAPDFStatus == "attached" {
					return "applied", detail, nil
				}
				return "no_op", detail, nil
			},
		})
	}

	env, runErr := runMutation(cmd.Context(), flags, spec.operation, ops)
	if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
		return renderErr
	}
	if runErr == nil && res.Via == "connector" {
		refreshItemsFromLocalAPI(cmd.Context(), flags)
	}
	return runErr
}

// createdItemKeyOf reports the new item's key from whichever route created it.
func createdItemKeyOf(res itemCreateResult) string {
	if res.Via == "connector" {
		return res.ConnKey
	}
	return res.WebKey
}

// routeCreateItemVia creates one Zotero item through an already resolved route.
func routeCreateItemVia(ctx context.Context, flags *rootFlags, via string, webClient itemPoster, item map[string]any, sourceURI string, collectionRequested bool) (itemCreateResult, error) {
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
	dbPath, err := defaultDBPath("zotio")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: store refresh skipped: %v\n", err)
		return
	}
	db, err := store.OpenWithContext(ctx, dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: store refresh skipped: %v\n", err)
		return
	}
	defer db.Close()
	oldHumanFriendly := humanFriendly
	humanFriendly = true
	defer func() { humanFriendly = oldHumanFriendly }()
	res := syncResource(ctx, c, db, "items", 0, false, 1000, false)
	if res.Err != nil {
		fmt.Fprintf(os.Stderr, "warning: store refresh skipped: %v\n", res.Err)
	} else if res.Warn != nil {
		fmt.Fprintf(os.Stderr, "warning: store refresh warning: %v\n", res.Warn)
	}
}
