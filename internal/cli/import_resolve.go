// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// PATCH(glean roadmap-phase4 7e799ea9): import resolve materializes the editable scan manifest without mutating Zotero.
func newImportResolveCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int

	cmd := &cobra.Command{
		Use:         "resolve <dir-or-manifest>",
		Short:       "Resolve PDFs into an editable import manifest",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := resolveImportManifest(cmd, flags, args[0], flagLimit)
			if err != nil {
				return err
			}
			return writeImportManifest(cmd.OutOrStdout(), m)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 200, "Scan at most N PDFs when resolving a directory")
	return cmd
}

// PATCH(glean roadmap-phase4 7e799ea9): dispatch between fresh directory scans and editable manifest refreshes.
func resolveImportManifest(cmd *cobra.Command, flags *rootFlags, arg string, limit int) (importManifest, error) {
	if info, err := os.Stat(arg); err == nil && info.IsDir() {
		return buildImportManifestFromDir(cmd, flags, arg, limit)
	}

	m, err := readImportManifest(arg)
	if err != nil {
		return importManifest{}, err
	}
	return refreshImportManifestCreates(cmd, flags, m), nil
}

// PATCH(glean roadmap-phase4 7e799ea9): reuse import scan classification while keeping absolute attachment paths in the manifest.
func buildImportManifestFromDir(cmd *cobra.Command, flags *rootFlags, dir string, limit int) (importManifest, error) {
	db, err := openStoreForRead(cmd.Context(), "zotio")
	if err != nil {
		return importManifest{}, fmt.Errorf("opening local store: %w", err)
	}
	idx := libraryDOIIndex{byDOI: map[string]libItem{}}
	if db != nil {
		defer db.Close()
		idx, err = buildLibraryDOIIndex(db)
		if err != nil {
			return importManifest{}, fmt.Errorf("indexing library DOIs: %w", err)
		}
	}

	paths, err := listPDFs(dir, limit)
	if err != nil {
		return importManifest{}, err
	}

	httpClient := &http.Client{Timeout: flags.timeout}
	m := importManifest{
		SchemaVersion: importManifestSchemaVersion,
		Dir:           dir,
		Entries:       make([]importManifestEntry, 0, len(paths)),
	}
	for _, path := range paths {
		res := classifyPDF(cmd.Context(), path, idx, httpClient)
		abs, _ := filepath.Abs(path)
		entry := importManifestEntry{
			Path:           abs,
			Classification: res.Status,
			Action:         manifestActionForStatus(res.Status),
			MatchedKey:     res.ItemKey,
			Title:          res.Title,
			Status:         "resolved",
		}
		if res.DOI != "" {
			entry.IdentifierType = "doi"
			entry.Identifier = res.DOI
		}
		if entry.Action == "create" {
			if res.DOI == "" {
				entry.Status = "unresolved"
				entry.Note = "no identifier"
			} else if item, fetchErr := fetchCrossRefItem(cmd, flags.timeout, res.DOI); fetchErr == nil {
				entry.Item = item
			} else {
				entry.Status = "unresolved"
				entry.Note = fetchErr.Error()
			}
		}
		if res.Status == "unidentified" {
			entry.Status = "unresolved"
		}
		m.Entries = append(m.Entries, entry)
	}
	return m, nil
}

// PATCH(glean roadmap-phase4 7e799ea9): let users re-run metadata resolution for unresolved DOI create entries.
func refreshImportManifestCreates(cmd *cobra.Command, flags *rootFlags, m importManifest) importManifest {
	for i := range m.Entries {
		entry := &m.Entries[i]
		if entry.Action != "create" || entry.Status != "unresolved" || entry.Identifier == "" {
			continue
		}
		item, err := fetchCrossRefItem(cmd, flags.timeout, entry.Identifier)
		if err != nil {
			entry.Note = err.Error()
			continue
		}
		entry.Item = item
		entry.Status = "resolved"
		entry.Note = ""
	}
	return m
}
