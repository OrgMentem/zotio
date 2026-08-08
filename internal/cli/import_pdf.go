// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Recognize local PDFs through Zotero's desktop Connector API.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

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
}

// Import pdf is connector-only because PDF recognition exists only in Zotero desktop.
func newImportPDFCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pdf <path...>",
		Short: "Create items from PDFs using Zotero desktop recognition",
		Long: `Create Zotero items from local PDF files by asking the Zotero desktop Connector
API to save each PDF as a standalone attachment and run Zotero's metadata recognizer.

This command requires a local Zotero base URL, Zotero running, and Zotero's local API
preference enabled. Recognition failures are reported as unrecognized standalone PDF
attachments instead of hard errors.`,
		Args: cobra.MinimumNArgs(1),
		Annotations: map[string]string{
			"zotio:method": "POST",
			"zotio:path":   "/connector/saveStandaloneAttachment",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if via, err := flags.resolveCreateVia(cmd.Context(), false); err != nil || via != "connector" {
				return preconditionErr(fmt.Errorf("import pdf requires the desktop connector (local base URL + Zotero running)"))
			}
			conn, err := flags.newConnector()
			if err != nil {
				return err
			}
			ops := make([]mutation.Op, 0, len(args))
			for i, arg := range args {
				path, err := filepath.Abs(arg)
				if err != nil {
					return fmt.Errorf("resolving PDF path %q: %w", arg, err)
				}
				index := i + 1
				label := filepath.Base(path)
				ops = append(ops, importPDFOp(cmd, flags, conn, path, label, index))
			}
			env, runErr := runMutation(cmd.Context(), flags, "import.pdf", ops)
			if renderErr := renderMutation(cmd, flags, env, nil); renderErr != nil {
				return renderErr
			}
			if runErr == nil && env.Result != nil && env.Result.Summary.Applied > 0 {
				refreshItemsFromLocalAPI(cmd.Context(), flags)
			}
			return runErr
		},
	}
	return cmd
}

func importPDFOp(cmd *cobra.Command, flags *rootFlags, conn *connector.Client, path, label string, index int) mutation.Op {
	return mutation.Op{
		ID:   fmt.Sprintf("import.pdf.%03d", index),
		Key:  path,
		Kind: "import_pdf",
		Changes: []mutation.Change{{
			Field: "pdf",
			Add:   label,
		}},
		Apply: func() (string, any, error) {
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
			return "applied", result, nil
		},
	}
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
