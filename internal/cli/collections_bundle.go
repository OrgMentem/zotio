// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH(glean write-safety): add a read-only local research-package exporter for collections.

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"zotero-pp-cli/internal/store"
)

type collectionBundleManifest struct {
	Collection string   `json:"collection"`
	Out        string   `json:"out"`
	Files      []string `json:"files"`
	ItemCount  int      `json:"item_count"`
}

type collectionBundleCitation struct {
	Key      string `json:"key"`
	Citation string `json:"citation"`
}

func newCollectionsBundleCmd(flags *rootFlags) *cobra.Command {
	var outDir string
	cmd := &cobra.Command{
		Use:   "bundle <collectionKey>",
		Short: "Write a local research package for a collection",
		Long: `Assemble a self-contained research package from the local store for a
collection: synthesis context, annotations, and compact bibliography. This command
never writes to Zotero; run sync first if local data is missing.`,
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(outDir) == "" {
				return usageErr(fmt.Errorf("--out is required"))
			}

			db, err := openStoreForRead(cmd.Context(), "zotero-pp-cli")
			if err != nil {
				return err
			}
			if db == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotero-pp-cli sync' first.")
				return nil
			}
			defer db.Close()

			itemCount, err := db.Count("items")
			if err != nil {
				return fmt.Errorf("checking local item store: %w", err)
			}
			if itemCount == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotero-pp-cli sync' first.")
				return nil
			}

			manifest, err := writeCollectionBundle(db, args[0], outDir)
			if err != nil {
				return err
			}
			if flags.asJSON {
				return printCommandJSON(cmd.OutOrStdout(), manifest, flags)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %d files for collection %s (%d item(s)) to %s\n", len(manifest.Files), manifest.Collection, manifest.ItemCount, manifest.Out)
			for _, name := range manifest.Files {
				fmt.Fprintf(cmd.OutOrStdout(), "- %s\n", name)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&outDir, "out", "", "Output directory for the research package")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

func writeCollectionBundle(db *store.Store, collKey, outDir string) (collectionBundleManifest, error) {
	items, err := db.QueryItems(store.ItemQuery{
		Collection: collKey,
		TopOnly:    true,
		Sort:       "title",
		Direction:  "asc",
	})
	if err != nil {
		return collectionBundleManifest{}, fmt.Errorf("querying collection items: %w", err)
	}

	keys := make([]string, 0, len(items))
	for _, raw := range items {
		keys = append(keys, vaultItemMeta(raw).Key)
	}
	annByKey, err := db.AnnotationsForItems(keys)
	if err != nil {
		return collectionBundleManifest{}, fmt.Errorf("reading collection annotations: %w", err)
	}
	ftByItem := fulltextByParentItem(db)

	bundle := summarizeCollectionBundle{
		Collection: collKey,
		ItemCount:  len(items),
		Prompt:     collectionSynthesisPrompt(len(items)),
	}
	for _, raw := range items {
		key := vaultItemMeta(raw).Key
		bundle.Items = append(bundle.Items, buildItemBundle(raw, annByKey[key], ftByItem[key], summarizeOpts{maxChars: 8000, maxAnnotations: 40}))
	}

	annotationsMarkdown := formatAnnotationExportMarkdown(collectionBundleAnnotationItems(items, annByKey))
	if strings.TrimSpace(annotationsMarkdown) == "" {
		annotationsMarkdown = "# Annotations\n\nNo annotations found locally for this collection.\n"
	}

	bibliography, err := json.Marshal(collectionBundleBibliography(bundle.Items))
	if err != nil {
		return collectionBundleManifest{}, err
	}
	bibliography = append(bibliography, '\n')

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return collectionBundleManifest{}, fmt.Errorf("creating output directory: %w", err)
	}

	files := []struct {
		name string
		data []byte
	}{
		{name: "synthesis.md", data: []byte(ensureTrailingNewline(renderCollectionMarkdown(bundle)))},
		{name: "annotations.md", data: []byte(annotationsMarkdown)},
		{name: "bibliography.json", data: bibliography},
	}
	manifestFiles := make([]string, 0, len(files))
	for _, file := range files {
		path := filepath.Join(outDir, file.name)
		if err := os.WriteFile(path, file.data, 0o600); err != nil {
			return collectionBundleManifest{}, fmt.Errorf("writing %s: %w", file.name, err)
		}
		manifestFiles = append(manifestFiles, file.name)
	}

	return collectionBundleManifest{Collection: collKey, Out: outDir, Files: manifestFiles, ItemCount: len(items)}, nil
}

// collectionBundleAnnotationItems adapts local store rows into the reusable
// annotations-export Markdown formatter. PATCH(glean write-safety): keep bundle
// annotation rendering byte-for-byte aligned with `annotations export`.
func collectionBundleAnnotationItems(items []json.RawMessage, annByKey map[string][]json.RawMessage) []annotationExportItem {
	exports := make([]annotationExportItem, 0, len(items))
	for _, raw := range items {
		meta := vaultItemMeta(raw)
		annotations := annotationSummariesSorted(annByKey[meta.Key])
		if len(annotations) == 0 {
			continue
		}
		exports = append(exports, annotationExportItem{
			Key:         meta.Key,
			Title:       meta.Title,
			Year:        meta.Year,
			Authors:     meta.Authors,
			DOI:         meta.DOI,
			Annotations: annotations,
		})
	}
	return exports
}

func collectionBundleBibliography(items []summarizeBundle) []collectionBundleCitation {
	bibliography := make([]collectionBundleCitation, 0, len(items))
	for _, item := range items {
		bibliography = append(bibliography, collectionBundleCitation{Key: item.Key, Citation: item.Citation})
	}
	return bibliography
}

func ensureTrailingNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}
