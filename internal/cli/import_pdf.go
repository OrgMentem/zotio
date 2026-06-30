// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: recognize local PDFs through Zotero's desktop Connector API.

package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"zotero-pp-cli/internal/connector"
	"zotero-pp-cli/internal/mutation"
)

type importPDFResult struct {
	File         string `json:"file"`
	Session      string `json:"session,omitempty"`
	Status       string `json:"status"`
	CanRecognize bool   `json:"can_recognize"`
	Title        string `json:"title,omitempty"`
	ItemType     string `json:"item_type,omitempty"`
}

// PATCH: import pdf is connector-only because PDF recognition exists only in Zotero desktop.
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
			"pp:method": "POST",
			"pp:path":   "/connector/saveStandaloneAttachment",
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
			return "applied", result, nil
		},
	}
}

func localFileURL(path string) string {
	return "file://" + filepath.ToSlash(path)
}
