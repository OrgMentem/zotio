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

			m, err := readImportManifest(args[0])
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

			var writeClient importApplyPoster
			var storedClient *client.Client
			if resolveMutationMode(flags).Apply {
				storedCreateVia := ""
				if attachMode == "stored" && manifestHasResolvedCreate(m) {
					storedCreateVia, err = flags.resolveCreateVia(cmd.Context(), false)
					if err != nil {
						return err
					}
				}
				needsStoredWeb := attachMode == "stored" &&
					(manifestHasAttachEntries(m) || storedCreateVia == "web")
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
						res, err := routeCreateItem(cmd.Context(), flags, writeClient, item, importEntrySourceURL(entry, item), connectorCollectionKeyFromItem(item) != "" || strings.TrimSpace(flags.connectorTarget) != "")
						if err != nil {
							return "failed", nil, err
						}
						switch res.Via {
						case "connector":
							data, err := os.ReadFile(entryPath)
							if err != nil {
								return "failed", nil, fmt.Errorf("reading attachment %s: %w", entryPath, err)
							}
							conn, err := flags.newConnector()
							if err != nil {
								return "failed", nil, err
							}
							if err := conn.SaveAttachment(cmd.Context(), res.Session, res.ConnKey, "Full Text PDF", importEntrySourceURL(entry, item), "application/pdf", data); err != nil {
								return "failed", nil, err
							}
							if fetchPDF {
								attachResolverPDF(cmd.Context(), flags, &res)
							}
							return "applied", map[string]any{"via": "connector"}, nil
						case "web":
							req, err := newStoredUploadRequest(res.WebKey, entryPath, "")
							if err != nil {
								return "failed", map[string]any{"parent_key": res.WebKey}, err
							}
							status, reason, err := applyStoredUpload(cmd.Context(), storedClient, req, flags)
							if err != nil {
								return "failed", map[string]any{"parent_key": res.WebKey}, err
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
							return "applied", detail, nil
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
