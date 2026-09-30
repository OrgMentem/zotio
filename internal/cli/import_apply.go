// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Apply reviewed import manifests via the mutation engine.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/connector"
	"zotio/internal/mutation"
	"zotio/internal/store"
)

// Keep preview tests independent from concrete HTTP clients.
type importApplyPoster interface {
	Post(path string, body any) (json.RawMessage, int, error)
	PostWithHeaders(path string, body any, headers map[string]string) (json.RawMessage, int, error)
}

// Add reviewable manifest application with opt-in file attachment.
func newImportApplyCmd(flags *rootFlags) *cobra.Command {
	var attachMode string
	var fetchPDF bool
	var flagCollection string

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

By default this previews the planned changes; apply with --yes.

--via connector hands work to the running Zotero desktop, which surfaces its
own progress UI; zotio cannot dismiss it and the connector protocol exposes no
endpoint that closes or completes a save session. Observed 2026-08-22: roughly
78 consecutive one-per-item invocations left Zotero unresponsive with progress
windows accumulating; no proven mechanism has been established. One invocation
therefore saves every connector-bound stored create in one session, as import
file --via connector and items create do; an entry whose own collection needs a
different save target still gets its own session. Because the batch commits
together, a failed entry does not stop the entries after it. Prefer one
invocation with many records. --rate-limit governs only Web API requests and
does not pace connector calls; pace your own invocations.

--collection KEY files every created item into that collection. It overrides
the manifest's "collection" default for this run; an entry's own "collection"
still wins over both. The destination applies to created items only: attach
entries add a file to an item that already exists and do not re-file it. The
key must exist in the synced local store, or the run is refused before any
write. A connector save session has one target, so on the connector route an
entry whose own collection differs from the run's destination is refused
before any item is created; apply it in a separate run.`,
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

			if attachMode == "stored" && fetchPDF {
				return fmt.Errorf("--fetch-pdf cannot be combined with --attach-mode stored; the manifest already supplies the PDF")
			}

			m, err := readImportManifest(args[0], cmd.InOrStdin())
			if err != nil {
				return err
			}
			defaultCollection := strings.TrimSpace(flagCollection)
			if defaultCollection == "" {
				defaultCollection = strings.TrimSpace(m.Collection)
			}
			if err := verifyImportApplyCollections(cmd.Context(), m, defaultCollection); err != nil {
				return err
			}
			if fetchPDF || (attachMode == "stored" && flags.via == "connector") {
				if err := refuseImportApplySplitSession(m, defaultCollection); err != nil {
					return err
				}
			}
			if attachMode == "stored" {
				if err := validateStoredCreateTitles(m); err != nil {
					return err
				}
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
			storedCreateVia := ""
			if resolveMutationMode(flags).Apply {
				var storedCreateRoute createRoute
				if attachMode == "stored" && manifestHasResolvedCreate(m) {
					storedCreateRoute, err = flags.resolveCreateRoute(cmd.Context(), false)
					if err != nil {
						return err
					}
					storedCreateVia = storedCreateRoute.via
					if storedCreateVia == "connector" {
						if err := refuseImportApplySplitSession(m, defaultCollection); err != nil {
							return err
						}
					}
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

			ops := importApplyOps(cmd, flags, writeClient, storedClient, m, attachMode, fetchPDF, defaultCollection)
			// A shared connector session commits every batch-mate in one
			// SaveItems call, so stopping at the first failed entry would report
			// items that already exist as not_attempted and invite a duplicating
			// re-run. Every entry reports its own outcome, as in items create.
			var runOptions []func(*mutation.Options)
			if storedCreateVia == "connector" {
				runOptions = append(runOptions, func(o *mutation.Options) { o.ContinueOnError = true })
			}
			env, runErr := runMutation(cmd.Context(), flags, "import.apply", ops, runOptions...)
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
	cmd.Flags().StringVar(&flagCollection, "collection", "", "Collection key to file created items into (overrides the manifest default; attach entries are not re-filed)")

	return cmd
}

// importApplyEntryCollection is the destination for one create entry: the
// entry's own key, else the run/manifest default.
func importApplyEntryCollection(entry importManifestEntry, defaultCollection string) string {
	if key := strings.TrimSpace(entry.Collection); key != "" {
		return key
	}
	return defaultCollection
}

// importApplyCreates reports whether entry is a create that apply will run.
func importApplyCreates(entry importManifestEntry) bool {
	return entry.Action == "create" && entry.Status == "resolved" && entry.Item != nil
}

// verifyImportApplyCollections refuses the run before any write when a
// destination key is not a collection in the synced local store.
func verifyImportApplyCollections(ctx context.Context, m importManifest, defaultCollection string) error {
	var keys []string
	seen := map[string]bool{}
	for _, entry := range m.Entries {
		if !importApplyCreates(entry) {
			continue
		}
		if key := importApplyEntryCollection(entry, defaultCollection); key != "" && !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	db, err := openStoreForRead(ctx, "zotio")
	if err != nil {
		return fmt.Errorf("opening local store: %w", err)
	}
	if db == nil {
		return preconditionErr(fmt.Errorf("run 'zotio sync' first so destination collection %s can be verified", keys[0]))
	}
	defer db.Close()
	for _, key := range keys {
		if _, err := db.GetContext(ctx, "collections", key); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return usageErr(fmt.Errorf("destination collection %s is not in the local store; check the key or run 'zotio sync'", key))
			}
			return fmt.Errorf("looking up destination collection %s: %w", key, err)
		}
	}
	return nil
}

// refuseImportApplySplitSession refuses a connector-bound run in which an
// entry names a destination other than the run's: the shared save session has
// one target, and a second session is what the shared session exists to avoid.
func refuseImportApplySplitSession(m importManifest, defaultCollection string) error {
	for i, entry := range m.Entries {
		if !importApplyCreates(entry) {
			continue
		}
		if key := importApplyEntryCollection(entry, defaultCollection); key != defaultCollection {
			target := defaultCollection
			if target == "" {
				target = "(none)"
			}
			return usageErr(fmt.Errorf("manifest entry %d %q sets collection %s but the connector save session targets %s; one session has one target, so apply that entry in a separate run",
				i+1, importApplyEntryTitle(entry, entry.Item), key, target))
		}
	}
	return nil
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

func validateStoredCreateTitles(m importManifest) error {
	for i, entry := range m.Entries {
		if entry.Action != "create" || entry.Status != "resolved" || entry.Item == nil {
			continue
		}
		title, _ := entry.Item["title"].(string)
		if strings.TrimSpace(title) == "" {
			return fmt.Errorf("manifest entry %d stored create missing title", i+1)
		}
	}
	return nil
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

var storedConnectorRecoveryTimeout = connectorReparentVisibilityTimeout

func markedConnectorAttachmentURL(rawURL, connectorKey string) (markedURL, marker string, err error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || strings.TrimSpace(connectorKey) == "" {
		return "", "", fmt.Errorf("building connector attachment marker from %q: %w", rawURL, err)
	}
	marker = "zotio-write-" + connectorKey
	if parsed.Fragment == "" {
		parsed.Fragment = marker
	} else {
		parsed.Fragment += "&" + marker
	}
	return parsed.String(), marker, nil
}

func attachmentURLHasMarker(rawURL, marker string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	for _, part := range strings.Split(parsed.Fragment, "&") {
		if part == marker {
			return true
		}
	}
	return false
}

// confirmStoredConnectorKeys binds this write to the exact source URL marker
// and file bytes supplied to SaveAttachment. Title and type only bound the
// recent parent set; neither can make a concurrent identical write qualify.
func confirmStoredConnectorKeys(ctx context.Context, flags *rootFlags, item map[string]any, createdAfter time.Time, marker, wantMD5 string) (parentKey, attachmentKey string, parentMatches, fileMatches int, err error) {
	if createdAfter.IsZero() || strings.TrimSpace(marker) == "" || strings.TrimSpace(wantMD5) == "" {
		return "", "", 0, 0, fmt.Errorf("connector recovery requires a creation bound, URL marker, and file MD5")
	}
	title, _ := item["title"].(string)
	itemType, _ := item["itemType"].(string)
	if err := ctx.Err(); err != nil {
		return "", "", 0, 0, err
	}
	local, err := localClientForRoute(ctx, flags)
	if err != nil {
		return "", "", 0, 0, err
	}
	deadline := time.Now().Add(storedConnectorRecoveryTimeout)
	if routeDeadline, ok := ctx.Deadline(); ok && routeDeadline.Before(deadline) {
		deadline = routeDeadline
	}
	var lastChildErr error
	for {
		if err := ctx.Err(); err != nil {
			return "", "", parentMatches, fileMatches, err
		}
		parents, err := findRecentlyAddedItemKeys(local, title, itemType, createdAfter)
		if err != nil {
			return "", "", 0, 0, err
		}
		parentMatches = len(parents)
		fileMatches = 0
		parentKey, attachmentKey = "", ""
		lastChildErr = nil
		for _, candidate := range parents {
			if candidate == "" {
				lastChildErr = fmt.Errorf("a matching parent did not expose its permanent key")
				continue
			}
			rows, childErr := attachmentChildRows(local, candidate)
			if childErr != nil {
				lastChildErr = childErr
				continue
			}
			for _, row := range rows {
				if row.Data.ItemType != "attachment" || !zoteroItemKeyRE.MatchString(row.Key) ||
					!strings.EqualFold(strings.TrimSpace(row.Data.MD5), wantMD5) ||
					!attachmentURLHasMarker(row.Data.URL, marker) {
					continue
				}
				fileMatches++
				parentKey, attachmentKey = candidate, row.Key
			}
		}
		switch {
		case fileMatches == 1 && lastChildErr == nil:
			return parentKey, attachmentKey, parentMatches, fileMatches, nil
		case fileMatches > 1:
			return "", "", parentMatches, fileMatches, fmt.Errorf("%d recent title/type matches hold the marked manifest PDF; refusing to guess", fileMatches)
		case time.Now().After(deadline):
			if lastChildErr != nil {
				return "", "", parentMatches, fileMatches, lastChildErr
			}
			return "", "", parentMatches, fileMatches, fmt.Errorf("the marked manifest PDF did not appear under a recent matching parent within %s", storedConnectorRecoveryTimeout)
		}
		if err := sleepWithContext(ctx, connectorReparentPollInterval); err != nil {
			return "", "", parentMatches, fileMatches, err
		}
	}
}

// storedConnectorCreateResult resolves both permanent keys after the file
// lands. A connector write is already committed at this point, so an
// unresolved or ambiguous read-back is a committed conflict, never success.
func storedConnectorCreateResult(ctx context.Context, flags *rootFlags, item map[string]any, res itemCreateResult, entryTitle, marker, wantMD5 string) (string, map[string]any, error) {
	detail := map[string]any{
		"via": "connector", "committed": true, "title": entryTitle,
		"session": res.Session, "connector_key": res.ConnKey,
		"attachment_marker": marker,
	}
	if res.ConnectorError != "" {
		detail["connector_error"] = res.ConnectorError
	}
	parentKey, attachmentKey, parentMatches, fileMatches, err :=
		confirmStoredConnectorKeys(ctx, flags, item, res.CreatedAfter, marker, wantMD5)
	if err != nil {
		detail["recovery_target"] = "parent_attachment_keys"
		detail["parent_matches"] = parentMatches
		detail["file_matches"] = fileMatches
		detail["message"] = fmt.Sprintf("stored connector import %q committed in session %s with connector key %s and attachment marker %q, but its permanent keys could not be confirmed: %v. Do not retry; inspect Zotero for that attachment URL marker",
			sanitizeForTerminal(entryTitle), res.Session, res.ConnKey, marker, err)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "conflict", detail, ctxErr
		}
		return "conflict", detail, nil
	}
	detail["key"] = parentKey
	detail["parent_key"] = parentKey
	detail["attachment_key"] = attachmentKey
	return "applied", detail, nil
}

func storedConnectorCommittedDetail(res itemCreateResult, entryTitle, marker, message string) map[string]any {
	return map[string]any{
		"via": "connector", "committed": true, "title": entryTitle,
		"session": res.Session, "connector_key": res.ConnKey,
		"attachment_marker": marker, "recovery_target": "parent_attachment_keys",
		"message": message,
	}
}

// Build mutation ops without network or disk I/O.
func importApplyOps(cmd *cobra.Command, flags *rootFlags, writeClient importApplyPoster, storedClient *client.Client, m importManifest, attachMode string, fetchPDF bool, defaultCollection string) []mutation.Op {
	ops := make([]mutation.Op, 0, len(m.Entries))
	var session *importApplyConnectorSession
	if attachMode == "stored" {
		session = &importApplyConnectorSession{cmd: cmd, flags: flags, collectionKey: defaultCollection}
	}
	for i := range m.Entries {
		entry := m.Entries[i]
		switch entry.Action {
		case "create":
			if entry.Status != "resolved" || entry.Item == nil {
				continue
			}
			item := copyImportApplyItem(entry.Item)
			destination := importApplyEntryCollection(entry, defaultCollection)
			addImportCollection(item, destination)
			entryTitle := importApplyEntryTitle(entry, item)
			entryPath := entry.Path
			entryNumber := i + 1
			var resolverCreate itemCreateResult
			storedEntry := &importApplyStoredEntry{
				number: entryNumber, title: entryTitle, path: entryPath,
				sourceURL: importEntrySourceURL(entry, item), item: item,
			}
			// A connector save session has exactly one target (the run's
			// destination), so an entry whose item names some other
			// collection keeps its own session.
			itemCollection := connectorCollectionKeyFromItem(item)
			if session != nil && (itemCollection == "" || itemCollection == session.collectionKey || strings.TrimSpace(flags.connectorTarget) != "") {
				session.entries = append(session.entries, storedEntry)
			} else {
				storedEntry = nil
			}
			changes := []mutation.Change{{Field: "item", Add: entryTitle}}
			if destination != "" {
				changes = append(changes, mutation.Change{Field: "collection", Add: destination})
			}
			ops = append(ops, mutation.Op{
				ID:      fmt.Sprintf("import.apply:%03d:create", entryNumber),
				Kind:    "import_create",
				Changes: changes,
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
						attachmentReq, err := prepareStoredCreate(entryNumber, entryPath)
						if err != nil {
							return "failed", nil, err
						}
						if storedEntry != nil {
							if status, detail, err, shared := session.apply(storedEntry); shared {
								return status, detail, err
							}
						}
						collectionRequested := connectorCollectionKeyFromItem(item) != "" || strings.TrimSpace(flags.connectorTarget) != ""
						via, err := flags.resolveCreateVia(cmd.Context(), collectionRequested)
						if err != nil {
							return "failed", nil, err
						}
						var connectorSource io.ReadCloser
						if via == "connector" {
							connectorSource, err = openVerifiedAttachmentSource(attachmentReq)
							if err != nil {
								return "failed", nil, fmt.Errorf("opening connector attachment before creating its parent: %w", err)
							}
							defer func() { _ = connectorSource.Close() }()
						}
						res, err := routeCreateItemViaWithOptions(
							cmd.Context(), flags, via, writeClient, item, importEntrySourceURL(entry, item),
							collectionRequested, routeCreateOptions{preserveUnresolvedConnectorWrite: true},
						)
						if err != nil {
							// routeCreateItem deliberately returns a POPULATED
							// result alongside its error when the item was
							// already committed and only a later step failed
							// (create_route.go: SaveItems succeeded but the
							// collection filing did not). Dropping res here
							// would discard the only evidence of that parent
							// and report a clean failure over an orphan.
							if res.Session != "" || res.WebKey != "" {
								return "conflict", orphanedParentDetail(res, entryTitle, err), err
							}
							return "failed", nil, err
						}
						switch res.Via {
						case "connector":
							conn, err := flags.newConnector()
							if err != nil {
								return "conflict", orphanedConnectorParentDetail(res, entryTitle, err), err
							}
							return saveStoredConnectorAttachment(cmd.Context(), flags, conn, res, &importApplyStoredEntry{
								number: entryNumber, title: entryTitle, path: entryPath,
								sourceURL: importEntrySourceURL(entry, item), item: item,
							}, attachmentReq, connectorSource)
						case "web":
							req := attachmentReq
							bindStoredUploadParent(&req, res.WebKey)
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
						created, createErr := routeCreateItem(
							cmd.Context(),
							flags,
							nil,
							item,
							importEntrySourceURL(entry, item),
							connectorCollectionKeyFromItem(item) != "" || strings.TrimSpace(flags.connectorTarget) != "",
						)
						resolverCreate = created
						return singleItemCreateApplyResult(resolverCreate, createErr, entryTitle)
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

					data, _, err := writeClient.PostWithHeaders("/items", []map[string]any{item}, map[string]string{"Zotero-Write-Token": createBatchWriteToken("zotio.import.apply", strconv.Itoa(entryNumber), item)})
					if err != nil {
						// The token is a pure function of the manifest entry, so
						// applying the same manifest replays the same token: an
						// entry Zotero already committed is answered 412 instead
						// of duplicated, and the entry reports the committed
						// conflict below instead of inviting another replay.
						if isCommittedCreateConflict(err) {
							refusal := ambiguousCreateRefusal("import apply", fmt.Sprintf("manifest entry %d %q", entryNumber, entryTitle), err)
							return "conflict", map[string]any{"committed": true, "title": entryTitle, "message": refusal.Error()}, refusal
						}
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
							// The parent is already committed and nothing rolls it back, so the
							// failure must carry the evidence to reconcile by hand: "key" lets
							// the engine journal the created parent, "committed" keeps the
							// reason in the journal, and "message" is the only thing the
							// human renderer prints. Status stays "failed" (no new status
							// string): the envelope is not OK and the exit is non-zero.
							detail["key"] = createdKey
							detail["committed"] = true
							detail["title"] = entryTitle
							detail["attachment_error"] = err.Error()
							detail["message"] = fmt.Sprintf("item %q was created (%s) but the linked file was not attached: %v; find it by key, then either attach the file by hand or delete the item and re-run", entryTitle, createdKey, err)
							return "failed", detail, nil
						}
						detail["attachment_key"] = attachmentKey
					}
					return "applied", detail, nil
				},
			})
			if fetchPDF {
				ops = append(ops, mutation.Op{
					ID:   fmt.Sprintf("import.apply:%03d:resolver-pdf", entryNumber),
					Key:  entryTitle,
					Kind: "attachment_create",
					Changes: []mutation.Change{{
						Field: "attachment",
						Add: map[string]any{
							"source":    "resolver",
							"condition": "when an open-access PDF resolver is available",
						},
					}},
					Apply: func() (string, any, error) {
						outcome, err := attachResolverPDF(cmd.Context(), flags, &resolverCreate)
						return resolverPDFApplyResult(resolverCreate, outcome, err)
					},
				})
			}
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

// prepareStoredCreate checks a stored create's file before any parent exists,
// so an unreadable file never leaves an orphaned item behind.
func prepareStoredCreate(entryNumber int, entryPath string) (storedUploadRequest, error) {
	if entryPath == "" {
		return storedUploadRequest{}, fmt.Errorf("manifest entry %d attachment path is empty", entryNumber)
	}
	if err := validateImportStoredAttachment(entryPath); err != nil {
		return storedUploadRequest{}, err
	}
	return newStoredUploadRequest("", entryPath, "")
}

// importApplyStoredEntry is one stored create manifest entry and, once the
// shared connector session has run, that entry's own outcome.
type importApplyStoredEntry struct {
	number    int
	title     string
	path      string
	sourceURL string
	item      map[string]any

	status string
	detail any
	err    error
}

func (e *importApplyStoredEntry) record(status string, detail map[string]any, err error) {
	e.status, e.err = status, err
	// A nil map stored in an interface is not a nil reason; the engine would
	// then drop the error text it otherwise renders.
	if detail != nil {
		e.detail = detail
	}
}

// importApplyConnectorSession saves every connector-bound stored create of one
// invocation in a single desktop save session. Zotero opens its own progress UI
// per session and no connector endpoint closes one; the 2026-08-22 run of 78
// one-item sessions left Zotero unresponsive. The first applied entry runs the
// session and every entry then reports its own recorded outcome.
type importApplyConnectorSession struct {
	cmd     *cobra.Command
	flags   *rootFlags
	entries []*importApplyStoredEntry
	// collectionKey is the run's destination; it becomes the session target
	// unless --connector-target already names one.
	collectionKey string

	done     bool
	via      string
	routeErr error
}

// apply reports entry's outcome from the shared session. shared is false when
// the route resolved to the Web API, so the caller keeps its per-entry path.
func (s *importApplyConnectorSession) apply(entry *importApplyStoredEntry) (status string, detail any, err error, shared bool) {
	if !s.done {
		s.done = true
		s.run()
	}
	if s.routeErr != nil {
		return "failed", nil, s.routeErr, true
	}
	if s.via != "connector" {
		return "", nil, nil, false
	}
	return entry.status, entry.detail, entry.err, true
}

func (s *importApplyConnectorSession) run() {
	ctx := s.cmd.Context()
	flags := s.flags
	target := strings.TrimSpace(flags.connectorTarget)
	via, err := flags.resolveCreateVia(ctx, target != "" || s.collectionKey != "")
	if err != nil {
		s.routeErr = err
		return
	}
	s.via = via
	if via != "connector" {
		return
	}

	// Entries that cannot load their file are left out of the session, so
	// they fail cleanly instead of committing a parent without its file.
	batch := make([]*importApplyStoredEntry, 0, len(s.entries))
	requests := make([]storedUploadRequest, 0, len(s.entries))
	for _, entry := range s.entries {
		if itemType, _ := entry.item["itemType"].(string); strings.TrimSpace(itemType) == "" {
			entry.record("failed", nil, fmt.Errorf("manifest entry %d item missing itemType", entry.number))
			continue
		}
		req, err := prepareStoredCreate(entry.number, entry.path)
		if err != nil {
			entry.record("failed", nil, err)
			continue
		}
		// Opened again right before SaveAttachment: holding one descriptor per
		// entry for the whole session would exhaust the limit on large batches.
		source, err := openVerifiedAttachmentSource(req)
		if err != nil {
			entry.record("failed", nil, fmt.Errorf("opening connector attachment before creating its parent: %w", err))
			continue
		}
		_ = source.Close()
		batch = append(batch, entry)
		requests = append(requests, req)
	}
	if len(batch) == 0 {
		return
	}
	failAll := func(err error) {
		for _, entry := range batch {
			entry.record("failed", nil, err)
		}
	}
	conn, err := connectorForCreate(flags)
	if err != nil {
		failAll(err)
		return
	}
	if target == "" && s.collectionKey != "" {
		// Resolved before SaveItems so an unmappable destination fails the
		// batch cleanly instead of leaving committed items unfiled.
		target, err = resolveConnectorTarget(ctx, flags, conn, s.collectionKey)
		if err != nil {
			failAll(err)
			return
		}
	}
	sessionID, err := connector.NewID()
	if err != nil {
		failAll(err)
		return
	}
	// The connector-local "id" travels on a copy so the recorded change and
	// the key read-back see the manifest item unchanged.
	results := make([]itemCreateResult, len(batch))
	payload := make([]map[string]any, len(batch))
	for i, entry := range batch {
		connectorKey, err := connector.NewID()
		if err != nil {
			failAll(err)
			return
		}
		copied := copyImportApplyItem(entry.item)
		copied["id"] = connectorKey
		payload[i] = copied
		results[i] = itemCreateResult{Via: "connector", Session: sessionID, ConnKey: connectorKey}
	}
	createdAfter := time.Now().UTC().Add(-recentItemClockSkew)
	for i := range results {
		results[i].CreatedAfter = createdAfter
	}
	if saveErr := conn.SaveItems(ctx, sessionID, "", payload); saveErr != nil {
		// The connector can report an error after committing the items, so
		// no entry may report a clean failure that invites a re-create.
		cause := fmt.Errorf("connector save outcome is unresolved; refusing an automatic retry: %w", saveErr)
		for i, entry := range batch {
			results[i].ConnectorError = saveErr.Error()
			entry.record("conflict", orphanedParentDetail(results[i], entry.title, cause), cause)
		}
		return
	}
	if target != "" {
		if err := conn.UpdateSession(ctx, sessionID, target, nil, ""); err != nil {
			for i, entry := range batch {
				results[i].FilingFailed = true
				entry.record("conflict", orphanedParentDetail(results[i], entry.title, err), err)
			}
			return
		}
	}
	for i, entry := range batch {
		source, err := openVerifiedAttachmentSource(requests[i])
		if err != nil {
			entry.record("conflict", orphanedConnectorParentDetail(results[i], entry.title, err), err)
			continue
		}
		status, detail, err := saveStoredConnectorAttachment(ctx, flags, conn, results[i], entry, requests[i], source)
		_ = source.Close()
		entry.record(status, detail, err)
	}
}

// saveStoredConnectorAttachment attaches the manifest file to a parent the
// connector already committed in res.Session, then resolves both permanent
// keys against the file's MD5 and the marked source URL.
func saveStoredConnectorAttachment(ctx context.Context, flags *rootFlags, conn *connector.Client, res itemCreateResult, entry *importApplyStoredEntry, req storedUploadRequest, source io.ReadCloser) (string, map[string]any, error) {
	// Zotero's importFromNetworkStream hard-rejects an empty url ("'url' not
	// provided"), which the connector surfaces as an opaque HTTP 500 AFTER the
	// parent item was already created. A locally scanned PDF has no DOI or web
	// source, so fall back to the file's own URI.
	attachmentURL := entry.sourceURL
	if strings.TrimSpace(attachmentURL) == "" {
		attachmentURL = localFileURL(entry.path)
	}
	markedURL, marker, err := markedConnectorAttachmentURL(attachmentURL, res.ConnKey)
	if err != nil {
		return "conflict", orphanedConnectorParentDetail(res, entry.title, err), err
	}
	if err := conn.SaveAttachment(ctx, res.Session, res.ConnKey, "Full Text PDF", markedURL, "application/pdf", source, req.Size); err != nil {
		cause := orphanedParentError(entry.title, err)
		detail := storedConnectorCommittedDetail(res, entry.title, marker,
			fmt.Sprintf("%s Connector evidence: session %s, connector key %s, attachment marker %s.",
				cause.Error(), res.Session, res.ConnKey, marker))
		return "conflict", detail, err
	}
	return storedConnectorCreateResult(ctx, flags, entry.item, res, entry.title, marker, req.MD5)
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
	message := fmt.Sprintf("%s Connector evidence: session %s, connector key %s. Do not retry until the library is reconciled.",
		orphanedParentError(entryTitle, cause).Error(), res.Session, res.ConnKey)
	return map[string]any{
		"committed":     true,
		"via":           "connector",
		"session":       res.Session,
		"connector_key": res.ConnKey,
		"title":         entryTitle,
		"message":       message,
	}
}

// orphanedWebParentDetail reports the same for the Web API route, which does
// know the real Zotero item key — strictly better evidence than the connector
// case, and previously the only thing reported, with no sentence to render.
func orphanedWebParentDetail(parentKey, entryTitle string, cause error) map[string]any {
	return map[string]any{
		"via":        "web",
		"committed":  true,
		"key":        parentKey,
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
