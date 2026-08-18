// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Apply reviewed import manifests via the mutation engine.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/mutation"
)

// Keep preview tests independent from concrete HTTP clients.
type importApplyPoster interface {
	Post(path string, body any) (json.RawMessage, int, error)
}

// Add reviewable manifest application with opt-in file attachment.
func newImportApplyCmd(flags *rootFlags) *cobra.Command {
	var attachMode string
	var fetchPDF bool

	cmd := &cobra.Command{
		Use:   "apply <manifest>",
		Short: "Apply a reviewed import manifest",
		Args:  cobra.ExactArgs(1),
		Long: `Apply a reviewed import manifest, optionally attaching each entry's file.

--attach-mode stored routes two different ways. For an entry that CREATES its
item, --via connector hands the item and its file to Zotero desktop in one
session, so Zotero files the bytes wherever it is configured to — including a
personal WebDAV server. Every other stored case (attaching to an item that
already exists, or a create that resolves to the Web route) uploads through the
Zotero Web API, which always lands in Zotero's own cloud storage; that upload is
refused when the desktop keeps files elsewhere, unless --allow-zotero-cloud.

An entry with no DOI or source URL records the file's own file:// URI as the
attachment's provenance, so the local path becomes item metadata and syncs.

By default this previews the planned changes; apply with --yes.`,
		Annotations: map[string]string{
			"zotio:method": "POST",
			"zotio:path":   "/items",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch attachMode {
			case "none", "linked-file", "stored":
			default:
				return fmt.Errorf("--attach-mode must be one of none, linked-file, stored")
			}

			m, err := readImportManifest(args[0], cmd.InOrStdin())
			if err != nil {
				return err
			}
			// Stored CREATES, PDF recognition, and resolver fetches require the desktop
			// Connector API; stored attach to an EXISTING item uses the Web API
			// file-upload protocol instead (shared with `attachments add`).
			if fetchPDF {
				via, err := flags.resolveCreateVia(cmd.Context(), false)
				if err != nil || via != "connector" {
					return preconditionErr(fmt.Errorf("--fetch-pdf requires the desktop connector (local base URL + Zotero running)"))
				}
			}
			if manifestHasRecognize(m) {
				via, err := flags.resolveCreateVia(cmd.Context(), false)
				if err != nil || via != "connector" {
					return preconditionErr(fmt.Errorf("action recognize requires the desktop connector (local base URL + Zotero running)"))
				}
			}
			// A stored attachment that lands on an already-existing item can
			// only go through the Web API file-upload protocol, which always
			// writes into Zotero's cloud storage. That is knowable without
			// resolving the create route, so refuse it in preview too rather
			// than presenting a plan that apply will reject.
			if attachMode == "stored" && (manifestHasAttachEntries(m) || (manifestHasResolvedCreate(m) && flags.via == "web")) {
				// Attach entries are the harder constraint: no route reaches a
				// non-cloud store for them, whatever --via says.
				//
				// Preview does not resolve --via auto, so the only create that
				// reaches here is one the operator explicitly sent to the Web
				// uploader. That cause is known from the flag, not probed.
				route := storedUploadCreateFellBack(createRoute{via: "web", cause: webRouteCauseExplicitWeb})
				if manifestHasAttachEntries(m) {
					route = storedUploadToExistingItem
				}
				if err := refuseStoredWebUpload(cmd, flags, "import apply", route); err != nil {
					return err
				}
			}

			var writeClient importApplyPoster
			var storedClient *client.Client
			if resolveMutationMode(flags).Apply {
				storedCreateVia := ""
				var storedCreateRoute createRoute
				if attachMode == "stored" && manifestHasResolvedCreate(m) {
					storedCreateRoute, err = flags.resolveCreateRoute(cmd.Context(), false)
					if err != nil {
						return err
					}
					storedCreateVia = storedCreateRoute.via
				}
				needsStoredWeb := attachMode == "stored" &&
					(manifestHasAttachEntries(m) || storedCreateVia == "web")
				if needsStoredWeb {
					// Route resolution can still land on the Web uploader for
					// creates under --via auto; catch that before any bytes move.
					// A create that fell back this way has a local route that is
					// merely unavailable, which is worth saying differently —
					// using the cause recorded when the route was chosen, since
					// re-probing now would describe a different moment.
					route := storedUploadCreateFellBack(storedCreateRoute)
					if manifestHasAttachEntries(m) {
						route = storedUploadToExistingItem
					}
					if err := refuseStoredWebUpload(cmd, flags, "import apply", route); err != nil {
						return err
					}
				}
				if needsStoredWeb {
					c, err := flags.newWriteClient()
					if err != nil {
						return err
					}
					storedClient = c
					writeClient = c
				} else if attachMode != "stored" && !fetchPDF {
					c, err := flags.newWriteClient()
					if err != nil {
						return err
					}
					writeClient = c
				}
			}

			ops := importApplyOps(cmd, flags, writeClient, storedClient, m, attachMode, fetchPDF)
			env, runErr := runMutation(cmd.Context(), flags, "import.apply", ops)
			if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
				return renderErr
			}
			if (attachMode == "stored" || fetchPDF) && env.Result != nil && env.Result.Summary.Applied > 0 {
				refreshItemsFromLocalAPI(cmd.Context(), flags)
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&attachMode, "attach-mode", "none", "Attachment handling: none, linked-file, or stored")
	cmd.Flags().BoolVar(&fetchPDF, "fetch-pdf", false, "Attach an open-access PDF via Zotero's desktop resolver (requires --via connector)")

	return cmd
}

func manifestHasRecognize(m importManifest) bool {
	for _, entry := range m.Entries {
		if entry.Action == "recognize" {
			return true
		}
	}
	return false
}

// manifestHasResolvedCreate reports whether any manifest entry would run the
// connector-backed stored-create path (mirrors the create filter in importApplyOps).
func manifestHasResolvedCreate(m importManifest) bool {
	for _, entry := range m.Entries {
		if entry.Action == "create" && entry.Status == "resolved" && entry.Item != nil {
			return true
		}
	}
	return false
}

// manifestHasAttachEntries reports whether any entry attaches to an existing item.
func manifestHasAttachEntries(m importManifest) bool {
	for _, entry := range m.Entries {
		if entry.Action == "attach" && entry.MatchedKey != "" {
			return true
		}
	}
	return false
}

// Build mutation ops without network or disk I/O.
func importApplyOps(cmd *cobra.Command, flags *rootFlags, writeClient importApplyPoster, storedClient *client.Client, m importManifest, attachMode string, fetchPDF bool) []mutation.Op {
	ops := make([]mutation.Op, 0, len(m.Entries))
	for i := range m.Entries {
		entry := m.Entries[i]
		switch entry.Action {
		case "create":
			if entry.Status != "resolved" || entry.Item == nil {
				continue
			}
			item := copyImportApplyItem(entry.Item)
			entryTitle := importApplyEntryTitle(entry, item)
			entryPath := entry.Path
			entryNumber := i + 1
			ops = append(ops, mutation.Op{
				ID:      fmt.Sprintf("import.apply:%03d:create", entryNumber),
				Kind:    "import_create",
				Changes: []mutation.Change{{Field: "item", Add: entryTitle}},
				Apply: func() (string, any, error) {
					itemType, _ := item["itemType"].(string)
					itemType = strings.TrimSpace(itemType)
					if itemType == "" {
						return "failed", nil, fmt.Errorf("manifest entry %d item missing itemType", entryNumber)
					}
					// Stored creates use the selected route for the parent. The
					// connector can save both objects in one desktop session; the
					// Web route creates the parent, then delegates the child bytes
					// to the same exactly-once uploader as `attachments add`.
					if attachMode == "stored" {
						if entryPath == "" {
							return "failed", nil, fmt.Errorf("manifest entry %d attachment path is empty", entryNumber)
						}
						if err := validateImportStoredAttachment(entryPath); err != nil {
							return "failed", nil, err
						}
						res, err := routeCreateItem(cmd.Context(), flags, writeClient, item, importEntrySourceURL(entry, item), connectorCollectionKeyFromItem(item) != "" || strings.TrimSpace(flags.connectorTarget) != "")
						if err != nil {
							// routeCreateItem deliberately returns a POPULATED
							// result alongside its error when the item was
							// already committed and only a later step failed
							// (create_route.go: SaveItems succeeded but the
							// collection filing did not). Dropping res here
							// would discard the only evidence of that parent
							// and report a clean failure over an orphan.
							if res.Session != "" || res.WebKey != "" {
								return "failed", orphanedParentDetail(res, entryTitle, err), orphanedParentError(entryTitle, err)
							}
							return "failed", nil, err
						}
						switch res.Via {
						case "connector":
							data, err := os.ReadFile(entryPath)
							if err != nil {
								cause := fmt.Errorf("reading attachment %s: %w", entryPath, err)
								return "failed", orphanedConnectorParentDetail(res, entryTitle, cause), orphanedParentError(entryTitle, cause)
							}
							conn, err := flags.newConnector()
							if err != nil {
								return "failed", orphanedConnectorParentDetail(res, entryTitle, err), orphanedParentError(entryTitle, err)
							}
							// Zotero's importFromNetworkStream hard-rejects an
							// empty url ("'url' not provided"), which the
							// connector surfaces as an opaque HTTP 500 AFTER the
							// parent item was already created. A locally scanned
							// PDF has no DOI or web source, so fall back to the
							// file's own URI — the same provenance import pdf
							// already records for standalone attachments.
							attachmentURL := importEntrySourceURL(entry, item)
							if strings.TrimSpace(attachmentURL) == "" {
								attachmentURL = localFileURL(entryPath)
							}
							if err := conn.SaveAttachment(cmd.Context(), res.Session, res.ConnKey, "Full Text PDF", attachmentURL, "application/pdf", data); err != nil {
								return "failed", orphanedConnectorParentDetail(res, entryTitle, err), orphanedParentError(entryTitle, err)
							}
							if fetchPDF {
								attachResolverPDF(cmd.Context(), flags, &res)
							}
							return "applied", map[string]any{"via": "connector"}, nil
						case "web":
							req, err := newStoredUploadRequest(res.WebKey, entryPath, "")
							if err != nil {
								return "failed", orphanedWebParentDetail(res.WebKey, entryTitle, err), orphanedParentError(entryTitle, err)
							}
							status, reason, err := applyStoredUpload(cmd.Context(), storedClient, req, flags)
							if err != nil {
								return "failed", orphanedWebParentDetail(res.WebKey, entryTitle, err), orphanedParentError(entryTitle, err)
							}
							detail := map[string]any{
								"via":               "web",
								"parent_key":        res.WebKey,
								"attachment_result": reason,
							}
							if upload, ok := reason.(map[string]any); ok {
								detail["attachment_key"] = upload["item_key"]
							}
							if status == "conflict" || status == "failed" {
								// The parent is committed and the file did not
								// land. This returns nil error by design (the
								// outcome is a status, not a transport
								// failure), so the engine's human renderer has
								// only this map - and it prints just the
								// "message" key. Without it the operator sees a
								// bare Go map and no way to find the item.
								detail["title"] = entryTitle
								detail["message"] = orphanedParentError(entryTitle,
									fmt.Errorf("attachment upload %s: %v", status, reason)).Error()
								return status, detail, nil
							}
							return "applied", detail, nil
						default:
							return "failed", nil, fmt.Errorf("unsupported stored-create route %q", res.Via)
						}
					}
					if fetchPDF {
						res, err := routeCreateItem(cmd.Context(), flags, nil, item, importEntrySourceURL(entry, item), connectorCollectionKeyFromItem(item) != "" || strings.TrimSpace(flags.connectorTarget) != "")
						if err != nil {
							return "failed", nil, err
						}
						if res.Via != "connector" {
							return "failed", nil, fmt.Errorf("--fetch-pdf requires the desktop connector")
						}
						attachResolverPDF(cmd.Context(), flags, &res)
						return "applied", map[string]any{"via": "connector", "oa_pdf": map[string]any{"status": res.OAPDFStatus, "title": res.OAPDFTitle, "error": res.OAPDFError}}, nil
					}
					if writeClient == nil {
						return "failed", nil, fmt.Errorf("missing write client")
					}
					tmpl, err := fetchItemTemplate(cmd.Context(), flags, itemType)
					if err != nil {
						return "failed", nil, err
					}
					if err := validateItemFields(tmpl, item); err != nil {
						return "failed", nil, err
					}

					data, _, err := writeClient.Post("/items", []map[string]any{item})
					if err != nil {
						return "failed", nil, classifyAPIError(err, flags)
					}
					createdKey, ok := createdItemKey(data)
					if !ok {
						return "failed", nil, fmt.Errorf("could not read created item key from /items response")
					}
					detail := map[string]any{"parent_key": createdKey}
					if attachMode == "linked-file" && entryPath != "" {
						attachmentKey, err := postLinkedFileAttachment(writeClient, createdKey, entryPath, flags)
						if err != nil {
							detail["attachment_error"] = err.Error()
							return "failed", detail, nil
						}
						detail["attachment_key"] = attachmentKey
					}
					return "applied", detail, nil
				},
			})
		case "recognize":
			if entry.Path == "" {
				continue
			}
			entryPath := entry.Path
			entryNumber := i + 1
			// Recognize unidentified PDFs through Zotero's desktop Connector API.
			ops = append(ops, importPDFOp(cmd, flags, nil, entryPath, filepath.Base(entryPath), entryNumber))
		case "attach":
			if entry.MatchedKey == "" {
				continue
			}
			matchedKey := entry.MatchedKey
			entryPath := entry.Path
			entryNumber := i + 1
			op := mutation.Op{
				ID:   fmt.Sprintf("import.apply:%03d:attach", entryNumber),
				Key:  matchedKey,
				Kind: "import_attach",
			}
			if attachMode == "none" {
				ops = append(ops, op)
				continue
			}
			op.Changes = []mutation.Change{{Field: "attachment", Add: filepath.Base(entryPath)}}
			op.Apply = func() (string, any, error) {
				if entryPath == "" {
					return "failed", nil, fmt.Errorf("manifest entry %d attachment path is empty", entryNumber)
				}
				if attachMode == "stored" {
					req, err := newStoredUploadRequest(matchedKey, entryPath, "")
					if err != nil {
						return "failed", nil, err
					}
					return applyStoredUpload(cmd.Context(), storedClient, req, flags)
				}
				if writeClient == nil {
					return "failed", nil, fmt.Errorf("missing write client")
				}
				attachmentKey, err := postLinkedFileAttachment(writeClient, matchedKey, entryPath, flags)
				if err != nil {
					return "failed", nil, err
				}
				return "applied", map[string]any{"parent_key": matchedKey, "attachment_key": attachmentKey}, nil
			}
			ops = append(ops, op)
		case "skip":
			if entry.Classification != "duplicate" || entry.MatchedKey == "" {
				continue
			}
			ops = append(ops, mutation.Op{
				ID:   fmt.Sprintf("import.apply:%03d:duplicate", i+1),
				Key:  entry.MatchedKey,
				Kind: "import_duplicate",
			})
		}
	}
	return ops
}

// validateImportStoredAttachment checks that a stored upload can load the
// named attachment before creating a parent that would otherwise be orphaned.
func validateImportStoredAttachment(path string) error {
	file, err := os.Open(path) //nolint:gosec // G304: importing a user-named local file is the command's purpose.
	if err != nil {
		return fmt.Errorf("reading attachment %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("reading attachment %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("reading attachment %s: not a regular file", path)
	}
	if info.Size() >= int64(^uint(0)>>1) {
		return fmt.Errorf("reading attachment %s: file is too large to load", path)
	}
	return nil
}

// A stored create commits the parent item before it attaches the file, and
// this package deliberately does not roll back or retry (the mutation model is
// non-transactional). Both routes can therefore leave a parent behind with no
// file, so both report the same thing: what was created, and how to finish or
// undo it by hand.
//
// "message" carries the human text because reasonText, the engine's
// human-output renderer, prints only a structured reason's "message" field —
// without it the failure renders as a bare Go map.

// orphanedConnectorParentDetail reports the evidence needed to find a parent
// the connector route created. The connector protocol never returns a Zotero
// item key for it (SaveAttachment takes only the connector-local ConnKey), so
// session/key/title is the best available correlation.
func orphanedConnectorParentDetail(res itemCreateResult, entryTitle string, cause error) map[string]any {
	detail := map[string]any{
		"via":           "connector",
		"session":       res.Session,
		"connector_key": res.ConnKey,
		"title":         entryTitle,
		"message":       orphanedParentError(entryTitle, cause).Error(),
	}
	if res.WebKey != "" {
		detail["parent_key"] = res.WebKey
	}
	return detail
}

// orphanedWebParentDetail reports the same for the Web API route, which does
// know the real Zotero item key — strictly better evidence than the connector
// case, and previously the only thing reported, with no sentence to render.
func orphanedWebParentDetail(parentKey, entryTitle string, cause error) map[string]any {
	return map[string]any{
		"via":        "web",
		"parent_key": parentKey,
		"title":      entryTitle,
		"message":    orphanedParentError(entryTitle, cause).Error(),
	}
}

// orphanedParentDetail dispatches on the route actually taken. It exists for
// the failure sites that sit ABOVE the route switch — a create can commit its
// parent and then fail before the caller has branched — where the route is
// known only from the result.
func orphanedParentDetail(res itemCreateResult, entryTitle string, cause error) map[string]any {
	if res.Via == "web" {
		return orphanedWebParentDetail(res.WebKey, entryTitle, cause)
	}
	return orphanedConnectorParentDetail(res, entryTitle, cause)
}

// orphanedParentError states plainly that the parent item already exists
// unattached, and gives a deterministic next step.
func orphanedParentError(entryTitle string, cause error) error {
	return fmt.Errorf("item %q was created but the file was not attached: %w; find it by title in Zotero, then either attach the file directly (right-click the item -> Add Attachment -> Attach Stored Copy of File) or delete the item and re-run", entryTitle, cause)
}

// Isolate manifest item maps before closure capture.
func copyImportApplyItem(item map[string]any) map[string]any {
	copy := make(map[string]any, len(item))
	for key, value := range item {
		copy[key] = value
	}
	return copy
}

// Choose stable human-readable mutation preview labels.
func importApplyEntryTitle(entry importManifestEntry, item map[string]any) string {
	if strings.TrimSpace(entry.Title) != "" {
		return entry.Title
	}
	if title, ok := item["title"].(string); ok && strings.TrimSpace(title) != "" {
		return title
	}
	if itemType, ok := item["itemType"].(string); ok && strings.TrimSpace(itemType) != "" {
		return itemType
	}
	return "item"
}

// Post linked-file attachment children through the write client.
func postLinkedFileAttachment(c importApplyPoster, parentKey, absPath string, flags *rootFlags) (string, error) {
	// Child items are created by POSTing the attachment (with parentItem set)
	// to /items. /items/{key}/children is GET-only on the Web API and rejects
	// POST with HTTP 405.
	data, _, err := c.Post("/items", []map[string]any{linkedFileAttachmentItem(parentKey, absPath)})
	if err != nil {
		return "", classifyAPIError(err, flags)
	}
	key, ok := createdItemKey(data)
	if !ok {
		return "", fmt.Errorf("could not read created attachment key from /items response")
	}
	return key, nil
}

// Construct Zotero's linked-file attachment child payload.
func linkedFileAttachmentItem(parentKey, absPath string) map[string]any {
	return map[string]any{
		"itemType":    "attachment",
		"linkMode":    "linked_file",
		"parentItem":  parentKey,
		"title":       filepath.Base(absPath),
		"path":        absPath,
		"contentType": "application/pdf",
	}
}

// Extract the created item key from Zotero batch-create responses.
func createdItemKey(resp json.RawMessage) (string, bool) {
	var body struct {
		Success    map[string]string `json:"success"`
		Successful map[string]struct {
			Key string `json:"key"`
		} `json:"successful"`
	}
	if err := json.Unmarshal(resp, &body); err != nil {
		return "", false
	}
	if key := strings.TrimSpace(body.Success["0"]); key != "" {
		return key, true
	}
	if row, ok := body.Successful["0"]; ok {
		if key := strings.TrimSpace(row.Key); key != "" {
			return key, true
		}
	}
	return "", false
}
