// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// shared connector/Web API route for item creation.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/connector"
	"zotio/internal/mutation"
	"zotio/internal/store"
)

type itemPoster interface {
	Post(path string, body any) (json.RawMessage, int, error)
}

// connectorForCreate is the connector factory for the single-item route.
// Tests replace it with an httptest-backed client so recovery can be exercised
// without contacting the desktop connector.
var connectorForCreate = func(flags *rootFlags) (*connector.Client, error) {
	return flags.newConnector()
}

type itemCreateResult struct {
	Via     string
	WebKey  string
	WebData json.RawMessage
	Session string
	ConnKey string
	// CreatedAfter bounds post-attachment recovery of the permanent Zotero key.
	// Connector writes can remain invisible until SaveAttachment completes, so
	// import apply must be able to repeat the same title/type lookup afterwards.
	CreatedAfter time.Time
	OAPDFStatus  string
	OAPDFTitle   string
	OAPDFError   string
	// ConnectorError records that the connector reported a failure for a write
	// that nevertheless landed, so the result can say so instead of claiming a
	// clean create.
	ConnectorError string
	// FilingFailed distinguishes SaveItems-committed-but-filing-failed from a
	// true SaveItems failure. The Apply closure checks this explicitly rather
	// than inferring committed state from non-empty strings.
	FilingFailed bool
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
			return singleItemCreateApplyResult(res, err, spec.key)
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
	// The real key reaches the envelope AND the journal via the reason's "key"
	// field, which mutation.Run adopts into ResultItem.Key before journalling.
	if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
		return renderErr
	}
	if runErr == nil && res.Via == "connector" {
		refreshItemsFromLocalAPI(cmd.Context(), flags)
	}
	return runErr
}

// createdItemKeyOf reports the new item's key when the write route returned a
// Zotero key. Connector session IDs are deliberately not a fallback: they are
// local correlation IDs, not keys accepted by the Zotero item endpoint. A
// connector create whose key cannot be confirmed therefore remains explicitly
// non-undoable instead of journaling a misleading target.
func createdItemKeyOf(res itemCreateResult) string {
	return res.WebKey
}

func singleItemCreateApplyResult(res itemCreateResult, err error, fallbackKey string) (string, any, error) {
	if err != nil {
		if res.FilingFailed {
			k := createdItemKeyOf(res)
			fallback := res.Session
			if fallback == "" {
				fallback = res.ConnKey
			}
			display := k
			if display == "" {
				display = fallback
			}
			if display == "" {
				display = fallbackKey
			}
			reason := map[string]any{
				"via":     res.Via,
				"session": res.Session,
				"message": fmt.Sprintf("created item %s; target filing failed: %v; retry filing only, do not re-create the item", display, err),
			}
			if k != "" {
				reason["key"] = k
			}
			return "applied", reason, nil
		}
		return "failed", nil, err
	}
	return "applied", map[string]any{"via": res.Via, "key": createdItemKeyOf(res)}, nil
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
		conn, err := connectorForCreate(flags)
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
		// SaveItems requires a non-empty source URI even for metadata-only
		// creates. Prefer the item's DOI/URL when callers did not supply one;
		// connector.SaveItems supplies a valid synthetic URI only for a truly
		// source-less schema template.
		if strings.TrimSpace(sourceURI) == "" {
			sourceURI = itemCreateSourceURI(item)
		}
		// Zotero's connector can return an error AFTER having already created the
		// item (observed: HTTP 500 with the item present on the server at that
		// instant). Reporting `failed` for a write that succeeded makes a
		// retrying caller mint a duplicate on every attempt while appearing to
		// make no progress, so confirm against the library before believing it.
		createdAfter := time.Now().UTC().Add(-recentItemClockSkew)
		if saveErr := conn.SaveItems(ctx, sessionID, sourceURI, []map[string]any{item}); saveErr != nil {
			recovered, matched, lookupErr := confirmConnectorCreate(flags, item, createdAfter)
			if lookupErr != nil || recovered == "" {
				if matched > 1 {
					return itemCreateResult{}, fmt.Errorf("connector reported %w, and %d recently added items share this title, so whether it was created is unresolved; check the library before retrying", saveErr, matched)
				}
				return itemCreateResult{}, saveErr
			}
			return itemCreateResult{
				Via:            "connector",
				Session:        sessionID,
				ConnKey:        connectorKey,
				WebKey:         recovered,
				CreatedAfter:   createdAfter,
				ConnectorError: saveErr.Error(),
			}, nil
		}
		if target != "" {
			if err := conn.UpdateSession(ctx, sessionID, target, nil, ""); err != nil {
				// SaveItems has already committed the item; a transaction does
				// not span the two connector calls. Returning a zero value
				// here would erase Session/ConnKey/WebKey and make the caller
				// record the whole mutation as failed, inviting a duplicating
				// retry. Recover the best-effort WebKey and return the
				// populated result alongside the filing error so the create is
				// journaled and the target failure is reported separately.
				resolved, _, _ := confirmConnectorCreate(flags, item, createdAfter)
				return itemCreateResult{Via: "connector", Session: sessionID, ConnKey: connectorKey, WebKey: resolved, CreatedAfter: createdAfter, FilingFailed: true}, err
			}
		}
		// Resolve the real Zotero key on success as well, not only when the
		// connector errored. ConnKey is the id zotio generated for the session and
		// means nothing to the API, so without this a connector create reported an
		// empty key: the journal recorded nothing actionable and `journal undo`
		// had no item to trash. The item is created asynchronously by the desktop,
		// so an unresolved lookup is reported as such rather than failing a write
		// that already succeeded.
		resolved, _, _ := confirmConnectorCreate(flags, item, createdAfter)
		return itemCreateResult{Via: "connector", Session: sessionID, ConnKey: connectorKey, WebKey: resolved, CreatedAfter: createdAfter}, nil
	default:
		return itemCreateResult{}, fmt.Errorf("unsupported create route %q", via)
	}
}

// confirmConnectorCreate asks the library whether a connector write that
// reported an error actually landed. Best-effort: an unresolvable answer leaves
// the original error in place.
func confirmConnectorCreate(flags *rootFlags, item map[string]any, createdAfter time.Time) (string, int, error) {
	title, _ := item["title"].(string)
	itemType, _ := item["itemType"].(string)
	if title == "" || itemType == "" {
		return "", 0, nil
	}
	c, err := flags.newClient()
	if err != nil {
		return "", 0, err
	}
	// Poll rather than look once. SaveItems already succeeded, so the item
	// EXISTS; a miss here means it has not surfaced in /items/top yet. A single
	// lookup therefore reports "no key" for a create that worked, and a caller
	// that treats an empty key as a failed apply can re-derive the write and
	// duplicate the item. Reported by papio, and reproduced here.
	//
	// Ambiguity is NOT retried: more than one match will not resolve itself, and
	// waiting only delays a refusal that is already correct.
	deadline := time.Now().Add(connectorCreateRecoveryWindow)
	for {
		key, matched, err := findRecentlyAddedItemKey(c, title, itemType, createdAfter)
		if err != nil || key != "" || matched > 1 || time.Now().After(deadline) {
			return key, matched, err
		}
		if err := sleepWithContext(context.Background(), connectorCreateRecoveryInterval); err != nil {
			return "", 0, err
		}
	}
}

// connectorCreateRecoveryWindow bounds the wait for a connector-created item to
// surface on the plane this process reads. The desktop commits immediately, but
// the item still has to appear in /items/top.
var connectorCreateRecoveryWindow = 30 * time.Second

// connectorCreateRecoveryInterval paces that wait. Both are vars so tests can
// exercise the loop without spending real seconds in it.
var connectorCreateRecoveryInterval = 2 * time.Second

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
