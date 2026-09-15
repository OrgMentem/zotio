// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"zotio/internal/cliutil"

	"github.com/spf13/cobra"
)

// Import resolve materializes the editable scan manifest without mutating Zotero.
func newImportResolveCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int

	cmd := &cobra.Command{
		Use:         "resolve <dir-or-manifest>",
		Short:       "Resolve PDFs into an editable import manifest",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			m, resolveErr := resolveImportManifest(cmd, flags, args[0], flagLimit)
			// A manifest is a durable input to import apply. If cancellation
			// leaves any source unresolved, emit no artifact rather than a
			// manifest whose missing entries look intentional.
			if ctxErr := cmd.Context().Err(); ctxErr != nil {
				return errors.Join(resolveErr, ctxErr)
			}
			if m.SchemaVersion == 0 {
				return resolveErr
			}
			writeErr := writeImportManifest(cmd.OutOrStdout(), m)
			return errors.Join(resolveErr, writeErr)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 200, "Scan at most N PDFs when resolving a directory")
	return cmd
}

// Dispatch between fresh directory scans and editable manifest refreshes.
func resolveImportManifest(cmd *cobra.Command, flags *rootFlags, arg string, limit int) (importManifest, error) {
	if info, err := os.Stat(arg); err == nil && info.IsDir() {
		return buildImportManifestFromDir(cmd, flags, arg, limit)
	}

	m, err := readImportManifest(arg, cmd.InOrStdin())
	if err != nil {
		return importManifest{}, err
	}
	return refreshImportManifestCreates(cmd, flags, m)
}

// Reuse import scan classification while keeping absolute attachment paths in the manifest.
func buildImportManifestFromDir(cmd *cobra.Command, flags *rootFlags, dir string, limit int) (importManifest, error) {
	db, err := openStoreForRead(cmd.Context(), "zotio")
	if err != nil {
		return importManifest{}, fmt.Errorf("opening local store: %w", err)
	}
	idx := libraryDOIIndex{byDOI: map[string]libItem{}}
	if db != nil {
		defer db.Close()
		idx, err = buildLibraryDOIIndex(cmd.Context(), db)
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
	results, fanoutErrs := cliutil.FanoutRun(cmd.Context(), paths,
		func(path string) string { return path },
		func(ctx context.Context, path string) (importManifestEntryResolution, error) {
			return resolveImportManifestEntry(ctx, path, idx, httpClient)
		})
	for _, result := range results {
		m.Entries = append(m.Entries, result.Value.Entry)
		if result.Value.Err != nil {
			fanoutErrs = append(fanoutErrs, cliutil.FanoutError{
				Source: result.Source,
				Err:    result.Value.Err,
			})
		}
	}
	return m, FanoutReportErrors(fanoutErrs)
}

// importManifestEntryResolution keeps an unresolved entry beside its provider
// error. The manifest remains reviewable, while FanoutReportErrors also makes
// the partial failure visible to command callers.
type importManifestEntryResolution struct {
	Entry importManifestEntry
	Err   error
}

func resolveImportManifestEntry(ctx context.Context, path string, idx libraryDOIIndex, httpClient *http.Client) (importManifestEntryResolution, error) {
	res := classifyPDF(ctx, path, idx, httpClient)
	abs, err := filepath.Abs(path)
	if err != nil {
		return importManifestEntryResolution{}, fmt.Errorf("resolving absolute path for %q: %w", path, err)
	}
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

	var resolveErr error
	if entry.Action == "create" {
		// A "create" action means classifyPDF found a DOI: "new" is only
		// reached after the empty-DOI case returns "unidentified".
		if item, fetchErr := fetchDOIItemWithCache(ctx, httpClient, res.DOI, nil); fetchErr == nil {
			entry.Item = item
		} else {
			entry.Status = "unresolved"
			entry.Note = fetchErr.Error()
			resolveErr = fetchErr
		}
	}
	if res.Status == "unidentified" {
		entry.Status = "unresolved"
		// Without this, an entry carried no identifier, no item and no
		// note, which reads as "the registry has no such record" rather
		// than "nothing was extracted from this file". Name the failure and
		// the next step: apply routes "recognize" entries to Zotero's own
		// PDF recognizer. See dev/field-report-2026-08-22-papio-round2.md.
		entry.Note = "no DOI or arXiv ID found in the filename or the PDF's content; " +
			"import apply hands this file to Zotero's PDF recognizer"
	}
	return importManifestEntryResolution{Entry: entry, Err: resolveErr}, nil
}

type importManifestRefreshSource struct {
	Index      int
	Path       string
	Identifier string
}

type importManifestRefreshResolution struct {
	Index int
	Item  map[string]any
	Err   error
}

// Let users re-run metadata resolution for unresolved DOI create entries.
func refreshImportManifestCreates(cmd *cobra.Command, flags *rootFlags, m importManifest) (importManifest, error) {
	sources := make([]importManifestRefreshSource, 0, len(m.Entries))
	for i, entry := range m.Entries {
		if entry.Action != "create" || entry.Status != "unresolved" || entry.Identifier == "" {
			continue
		}
		sources = append(sources, importManifestRefreshSource{
			Index:      i,
			Path:       entry.Path,
			Identifier: entry.Identifier,
		})
	}

	httpClient := &http.Client{Timeout: flags.timeout}
	results, fanoutErrs := cliutil.FanoutRun(cmd.Context(), sources,
		func(source importManifestRefreshSource) string { return source.Path },
		func(ctx context.Context, source importManifestRefreshSource) (importManifestRefreshResolution, error) {
			item, err := fetchDOIItemWithCache(ctx, httpClient, source.Identifier, nil)
			return importManifestRefreshResolution{Index: source.Index, Item: item, Err: err}, nil
		})

	// Mutate the manifest only after every resolve completes. This keeps the
	// write step sequential and preserves the original entry order.
	for _, result := range results {
		entry := &m.Entries[result.Value.Index]
		if result.Value.Err != nil {
			entry.Note = result.Value.Err.Error()
			fanoutErrs = append(fanoutErrs, cliutil.FanoutError{
				Source: result.Source,
				Err:    result.Value.Err,
			})
			continue
		}
		entry.Item = result.Value.Item
		entry.Status = "resolved"
		entry.Note = ""
	}
	return m, FanoutReportErrors(fanoutErrs)
}
