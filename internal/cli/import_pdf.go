// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Recognize local PDFs through Zotero's desktop Connector API.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/connector"
	"zotio/internal/mutation"
)

// How far back through recently added top-level items to look when resolving the
// keys Zotero just created. Generous enough for a batch import, small enough
// that an unrelated same-title item elsewhere in the library cannot collide.
const importPDFKeyLookupLimit = 50

// Slack subtracted from the pre-import wall clock before comparing against
// Zotero's dateAdded. Both processes share this machine's clock, so this only
// absorbs the API's second-level granularity and any rounding at the boundary.
const importPDFClockSkew = 2 * time.Minute

type importPDFResult struct {
	File          string `json:"file"`
	Session       string `json:"session,omitempty"`
	Status        string `json:"status"`
	CanRecognize  bool   `json:"can_recognize"`
	Title         string `json:"title,omitempty"`
	ItemType      string `json:"item_type,omitempty"`
	ItemKey       string `json:"item_key,omitempty"`
	AttachmentKey string `json:"attachment_key,omitempty"`
	DOI           string `json:"doi,omitempty"`
	KeysNote      string `json:"keys_note,omitempty"`
	// CollectionKey/CollectionNote report the outcome of --collection filing:
	// the resolved key and a human note (filed, already a member, or why not).
	CollectionKey  string `json:"collection_key,omitempty"`
	CollectionNote string `json:"collection_note,omitempty"`
	// DuplicateOf/DuplicateNote report the outcome of --on-duplicate: the
	// existing library item this file's DOI already matched, and what zotio
	// did about it (skipped, attached, or created anyway).
	DuplicateOf   string `json:"duplicate_of,omitempty"`
	DuplicateNote string `json:"duplicate_note,omitempty"`
}

// zoteroCollectionKeyRE matches Zotero's real key format (8 uppercase
// alphanumerics). --collection accepts a key or a name; this is the only shape
// trusted as a key, since a human-chosen name could otherwise collide with it.
var zoteroCollectionKeyRE = regexp.MustCompile(`^[A-Z0-9]{8}$`)

// Import pdf is connector-only because PDF recognition exists only in Zotero desktop.
func newImportPDFCmd(flags *rootFlags) *cobra.Command {
	var collectionFlag string
	var onDuplicateFlag string
	cmd := &cobra.Command{
		Use:   "pdf <path...>",
		Short: "Create items from PDFs using Zotero desktop recognition",
		Long: `Create Zotero items from local PDF files by asking the Zotero desktop Connector
API to save each PDF as a standalone attachment and run Zotero's metadata recognizer.

This command requires a local Zotero base URL, Zotero running, and Zotero's local API
preference enabled. Recognition failures are reported as unrecognized standalone PDF
attachments instead of hard errors. The plan phase probes the connector before
previewing or applying anything, so a missing desktop fails loudly instead of a
--dry-run silently planning an import that cannot run.

Before importing, each PDF is classified against your synced library the same way
'import scan' does (DOI match). A DOI that already has a copy on file is handled per
--on-duplicate (skip, attach, or create) instead of always minting a second item; a
synced library is required for that check, and its absence only disables the check,
never the import.

--collection <key|name> files the created item into a collection after import,
creating a same-named collection when one doesn't already exist (like 'items
add-to-collection'). This is a separate step, not part of the connector call: the
saveStandaloneAttachment endpoint saves into whatever the desktop pane currently
targets and accepts no collection parameter.`,
		Args: cobra.MinimumNArgs(1),
		Annotations: map[string]string{
			"zotio:method": "POST",
			"zotio:path":   "/connector/saveStandaloneAttachment",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			dupMode := strings.TrimSpace(onDuplicateFlag)
			switch dupMode {
			case "skip", "attach", "create":
			default:
				return fmt.Errorf("--on-duplicate must be one of skip, attach, create")
			}
			if via, err := flags.resolveCreateVia(cmd.Context(), false); err != nil || via != "connector" {
				return preconditionErr(fmt.Errorf("import pdf requires the desktop connector (local base URL + Zotero running)"))
			}
			conn, err := flags.newConnector()
			if err != nil {
				return err
			}
			collection := strings.TrimSpace(collectionFlag)
			idx, dupWarning := loadImportPDFDuplicateIndex(cmd.Context())
			ops := make([]mutation.Op, 0, len(args))
			for i, arg := range args {
				path, err := filepath.Abs(arg)
				if err != nil {
					return fmt.Errorf("resolving PDF path %q: %w", arg, err)
				}
				index := i + 1
				label := filepath.Base(path)
				scan := classifyPDF(cmd.Context(), path, idx, nil)
				ops = append(ops, importPDFOpWithOptions(cmd, flags, conn, path, label, index, importPDFOptions{
					Collection:  collection,
					OnDuplicate: dupMode,
					Duplicate:   scan,
				}))
			}
			env, runErr := runMutation(cmd.Context(), flags, "import.pdf", ops)
			if dupWarning != "" {
				env.Warnings = append(env.Warnings, dupWarning)
			}
			if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
				return renderErr
			}
			if runErr == nil && env.Result != nil && env.Result.Summary.Applied > 0 {
				refreshItemsFromLocalAPI(cmd.Context(), flags)
			}
			return runErr
		},
	}
	cmd.Flags().StringVar(&collectionFlag, "collection", "", "Collection key or name to file the created item into (the name is created if it doesn't exist)")
	cmd.Flags().StringVar(&onDuplicateFlag, "on-duplicate", "skip", "How to handle a PDF whose DOI already has a copy in the library: skip, attach, or create")
	return cmd
}

// importPDFOptions configures behavior beyond bare recognition that only the
// standalone `import pdf` command exercises. import apply's manifest-driven
// "recognize" step reuses importPDFOp with the zero value: no collection
// filing, no duplicate handling — the manifest's own review step already
// decided what to do with each file.
type importPDFOptions struct {
	// Collection is the --collection value (key or name); empty disables filing.
	Collection string
	// OnDuplicate is skip|attach|create; only consulted when Duplicate.Status
	// is "duplicate".
	OnDuplicate string
	// Duplicate is import scan's classification of this file against the
	// synced library, computed once per file before any connector call.
	Duplicate scanResult
}

// importPDFOp builds a bare recognition op with none of the standalone
// command's extras. import apply's "recognize" manifest entries call this.
func importPDFOp(cmd *cobra.Command, flags *rootFlags, conn *connector.Client, path, label string, index int) mutation.Op {
	return importPDFOpWithOptions(cmd, flags, conn, path, label, index, importPDFOptions{OnDuplicate: "create"})
}

func importPDFOpWithOptions(cmd *cobra.Command, flags *rootFlags, conn *connector.Client, path, label string, index int, opts importPDFOptions) mutation.Op {
	kind := "import_pdf"
	changes := []mutation.Change{{Field: "pdf", Add: label}}
	if opts.Duplicate.Status == "duplicate" {
		changes = append(changes, mutation.Change{Field: "duplicate", Add: map[string]any{"item_key": opts.Duplicate.ItemKey, "doi": opts.Duplicate.DOI}})
		switch opts.OnDuplicate {
		case "skip":
			kind = "import_pdf_duplicate_skip"
		case "attach":
			kind = "import_pdf_duplicate_attach"
		}
	}
	if opts.Collection != "" {
		changes = append(changes, mutation.Change{Field: "collection", Add: opts.Collection})
	}
	return mutation.Op{
		ID:      fmt.Sprintf("import.pdf.%03d", index),
		Key:     path,
		Kind:    kind,
		Changes: changes,
		Apply: func() (string, any, error) {
			if opts.Duplicate.Status == "duplicate" {
				switch opts.OnDuplicate {
				case "skip":
					result := importPDFResult{
						File:        path,
						Status:      "skipped_duplicate",
						DOI:         opts.Duplicate.DOI,
						DuplicateOf: opts.Duplicate.ItemKey,
						DuplicateNote: fmt.Sprintf(
							"DOI %s already has a PDF on item %s; not creating a duplicate (use --on-duplicate attach or create to override)",
							opts.Duplicate.DOI, opts.Duplicate.ItemKey),
					}
					if opts.Collection != "" {
						result.CollectionNote = "not filed: skipped duplicate, no new item was created"
					}
					return "skipped", result, nil
				case "attach":
					status, reason, err := applyImportPDFAttach(cmd.Context(), flags, path, opts.Duplicate)
					if opts.Collection != "" {
						if result, ok := reason.(importPDFResult); ok {
							result.CollectionNote = "not filed: --collection only applies to newly created items"
							reason = result
						}
					}
					return status, reason, err
				}
				// case "create" falls through to a normal import below.
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return "failed", nil, fmt.Errorf("reading PDF %q: %w", path, err)
			}
			sessionID, err := connector.NewID()
			if err != nil {
				return "failed", nil, err
			}
			activeConn := conn
			if activeConn == nil {
				var err error
				activeConn, err = flags.newConnector()
				if err != nil {
					return "failed", nil, err
				}
			}
			// Anchor key resolution to a wall-clock floor captured BEFORE the
			// item exists. Zotero sets dateAdded at import time, so anything
			// older than this cannot be what this op created.
			importedAfter := time.Now().UTC().Add(-importPDFClockSkew)
			canRecognize, err := activeConn.SaveStandaloneAttachment(cmd.Context(), sessionID, filepath.Base(path), localFileURL(path), "application/pdf", data)
			if err != nil {
				return "failed", nil, err
			}
			result := importPDFResult{File: path, Session: sessionID, Status: "standalone", CanRecognize: canRecognize}
			if canRecognize {
				item, recognized, err := activeConn.GetRecognizedItem(cmd.Context(), sessionID)
				if err != nil {
					return "failed", nil, err
				}
				if recognized {
					result.Status = "recognized"
					result.Title = item.Title
					result.ItemType = item.ItemType
				} else {
					result.Status = "unrecognized"
				}
			}
			resolveImportPDFKeys(flags, &result, filepath.Base(path), importedAfter)

			if opts.Collection == "" {
				return "applied", result, nil
			}
			if result.ItemKey == "" {
				result.CollectionNote = "not filed: item key could not be resolved (see keys_note)"
				return "applied", result, nil
			}
			collectionKey, status, note := fileImportPDFIntoCollection(flags, result.ItemKey, opts.Collection)
			result.CollectionKey = collectionKey
			result.CollectionNote = note
			if status != "applied" && status != "no_op" {
				return status, result, nil
			}
			return "applied", result, nil
		},
	}
}

// loadImportPDFDuplicateIndex builds the same by-DOI library index 'import
// scan' uses, so import pdf can reuse its duplicate/attach_candidate/new
// classification instead of creating items that 'library health' immediately
// flags as duplicates. Best-effort: an unsynced or unreadable local store
// disables the check (the underlying recognition-only import never depends on
// it) and returns a warning so the gap is visible instead of silent.
func loadImportPDFDuplicateIndex(ctx context.Context) (libraryDOIIndex, string) {
	empty := libraryDOIIndex{byDOI: map[string]libItem{}}
	db, err := openStoreForRead(ctx, "zotio")
	if err != nil {
		return empty, fmt.Sprintf("duplicate detection disabled: opening local store: %v", err)
	}
	if db == nil {
		return empty, "duplicate detection disabled: local store is not synced; run 'zotio sync' to enable --on-duplicate checks"
	}
	defer db.Close()
	idx, err := buildLibraryDOIIndex(db)
	if err != nil {
		return empty, fmt.Sprintf("duplicate detection disabled: indexing library DOIs: %v", err)
	}
	return idx, ""
}

// applyImportPDFAttach attaches path as a stored child of the library item
// scan already matched, via the same Web API upload protocol as 'attachments
// add' and 'import apply --attach-mode stored' (newStoredUploadRequest +
// applyStoredUpload) — reused rather than reimplemented — instead of minting a
// second top-level item for a PDF zotio already knows about. This routes
// through the Web API (it needs a real item to attach to, which the connector
// cannot target), unlike the rest of import pdf.
func applyImportPDFAttach(ctx context.Context, flags *rootFlags, path string, scan scanResult) (string, any, error) {
	result := importPDFResult{File: path, Status: "attached_duplicate", DOI: scan.DOI, DuplicateOf: scan.ItemKey, ItemKey: scan.ItemKey}
	c, err := flags.newWriteClient()
	if err != nil {
		result.DuplicateNote = err.Error()
		return "failed", result, err
	}
	req, err := newStoredUploadRequest(scan.ItemKey, path, "")
	if err != nil {
		result.DuplicateNote = err.Error()
		return "failed", result, err
	}
	status, reason, err := applyStoredUpload(ctx, c, req, flags)
	if m, ok := reason.(map[string]any); ok {
		if key, ok := m["item_key"].(string); ok {
			result.AttachmentKey = key
		}
	}
	switch status {
	case "applied":
		result.DuplicateNote = fmt.Sprintf("attached to existing item %s instead of creating a duplicate", scan.ItemKey)
	case "no_op":
		result.DuplicateNote = fmt.Sprintf("identical stored attachment already present on %s", scan.ItemKey)
	default:
		if err != nil {
			result.DuplicateNote = err.Error()
		} else {
			result.DuplicateNote = fmt.Sprintf("%v", reason)
		}
	}
	return status, result, err
}

// fileImportPDFIntoCollection resolves --collection to a key (creating a
// same-named collection when absent, like 'items add-to-collection') and adds
// itemKey to it, reusing the same membership writer as 'items move'
// (applyItemCollectionMove) instead of a second PATCH path.
func fileImportPDFIntoCollection(flags *rootFlags, itemKey, collectionValue string) (collectionKey, status, note string) {
	c, err := flags.newWriteClient()
	if err != nil {
		return "", "failed", err.Error()
	}
	collectionKey, err = resolveImportPDFCollectionKey(c, collectionValue)
	if err != nil {
		return "", "failed", err.Error()
	}
	itemPath := replacePathParam("/items/{itemKey}", "itemKey", itemKey)
	status, reason, err := applyItemCollectionMove(c, itemPath, "", collectionKey)
	switch {
	case err != nil:
		note = err.Error()
	case status == "no_op":
		note = "already in collection " + collectionKey
		if m, ok := reason.(map[string]any); ok {
			if msg, ok := m["message"].(string); ok {
				note = msg
			}
		}
	case status == "applied":
		note = "filed into collection " + collectionKey
	}
	return collectionKey, status, note
}

// resolveImportPDFCollectionKey resolves --collection to a collection key. A
// value in Zotero's real key shape is trusted as a key outright; anything else
// is resolved (and created when absent) the same way 'items add-to-collection'
// resolves --collection-name, so the two commands behave consistently.
func resolveImportPDFCollectionKey(c *client.Client, value string) (string, error) {
	if zoteroCollectionKeyRE.MatchString(value) {
		return value, nil
	}
	collections, err := collectionsByName(c)
	if err != nil {
		return "", fmt.Errorf("looking up collection %q: %w", value, err)
	}
	if key := collections[value]; key != "" {
		return key, nil
	}
	return createCollectionByName(c, value)
}

// resolveImportPDFKeys fills in the keys Zotero just created. The connector
// cannot supply them: /connector/getRecognizedItem answers with title and
// itemType only (Zotero server_connector.js GetRecognizedItem), so the keys are
// resolved from the library instead. Without this, filing an import costs the
// caller a title search plus a links.up walk to find the parent.
//
// importedAfter is a wall-clock floor captured before the item existed. It is
// load-bearing, not a nicety: a title match alone can land on an OLDER item that
// merely shares the recognized title. That happens for real, because Zotero's
// connector saves into whatever library the desktop pane currently targets
// (server_connector.js SaveStandaloneAttachment calls getSaveTarget(false,false)
// and accepts no library parameter), which need not be the library zotio is
// configured to read. When the new item is not in the library we query, the
// floor makes us report nothing instead of a same-titled stranger's key.
//
// Resolution is best-effort and never fails the import: the write already
// succeeded, so an unresolvable key is reported in keys_note rather than
// turning an applied op into a failure.
func resolveImportPDFKeys(flags *rootFlags, result *importPDFResult, filename string, importedAfter time.Time) {
	wantTitle, wantType := result.Title, result.ItemType
	if result.Status != "recognized" {
		// No parent item was created, so the standalone attachment is itself
		// top-level and still carries the original filename as its title.
		wantTitle, wantType = filename, "attachment"
	}
	if wantTitle == "" || wantType == "" {
		result.KeysNote = "keys unresolved: recognizer returned no title or item type to match on"
		return
	}

	c, err := flags.newClient()
	if err != nil {
		result.KeysNote = "keys unresolved: " + err.Error()
		return
	}
	// The item was created seconds ago by the desktop; a cached list would not
	// contain it.
	c.NoCache = true
	data, err := c.Get("/items/top", map[string]string{
		"sort":      "dateAdded",
		"direction": "desc",
		"limit":     strconv.Itoa(importPDFKeyLookupLimit),
	})
	if err != nil {
		result.KeysNote = "keys unresolved: " + err.Error()
		return
	}
	var top []json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		result.KeysNote = "keys unresolved: parsing /items/top response: " + err.Error()
		return
	}

	var matched json.RawMessage
	found, staleTitleMatches := 0, 0
	for _, entry := range top {
		if !strings.EqualFold(strings.TrimSpace(jsonStringField(entry, "title")), strings.TrimSpace(wantTitle)) {
			continue
		}
		if jsonStringField(entry, "itemType") != wantType {
			continue
		}
		if !addedAfter(entry, importedAfter) {
			staleTitleMatches++
			continue
		}
		found++
		matched = entry
	}
	// Reporting a guessed key is worse than reporting none: the caller would
	// file the wrong item. Only an unambiguous, new-enough match counts.
	if found != 1 {
		result.KeysNote = fmt.Sprintf(
			"keys unresolved: %d of the %d most recently added items match %q and postdate this import (%d older title match(es) ignored)",
			found, len(top), wantTitle, staleTitleMatches)
		return
	}
	key := jsonStringField(matched, "key")
	if key == "" {
		result.KeysNote = "keys unresolved: matched item carries no key"
		return
	}
	result.DOI = jsonStringField(matched, "DOI")
	if wantType == "attachment" {
		result.AttachmentKey = key
		return
	}
	result.ItemKey = key

	children, err := c.Get(replacePathParam("/items/{itemKey}/children", "itemKey", key), map[string]string{"itemType": "attachment"})
	if err != nil {
		result.KeysNote = "attachment key unresolved: " + err.Error()
		return
	}
	attachmentKey, err := findPDFAttachmentKey(children)
	if err != nil {
		result.KeysNote = "attachment key unresolved: " + err.Error()
		return
	}
	if attachmentKey == "" {
		result.KeysNote = "attachment key unresolved: item has no PDF child"
		return
	}
	result.AttachmentKey = attachmentKey
}

// addedAfter reports whether a Zotero object's dateAdded is at or after floor.
// An unparseable or missing dateAdded fails closed: without a usable timestamp
// we cannot prove the object came from this import, so it is not a candidate.
func addedAfter(entry json.RawMessage, floor time.Time) bool {
	raw := strings.TrimSpace(jsonStringField(entry, "dateAdded"))
	if raw == "" {
		return false
	}
	added, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false
	}
	return !added.Before(floor)
}

func localFileURL(path string) string {
	return "file://" + filepath.ToSlash(path)
}
