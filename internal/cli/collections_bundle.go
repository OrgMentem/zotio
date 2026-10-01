// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/store"
)

// collectionBundleCommitName is the package's completion record. Publication
// removes it before replacing any content file and writes it last, so it is
// present only while every listed file belongs to one generation.
const collectionBundleCommitName = "zotio-bundle.json"

// collectionBundleManifest is the command result. Replaced names the content
// files that already existed and were (or, under --dry-run, would be)
// replaced; it is never null.
type collectionBundleManifest struct {
	Collection string   `json:"collection"`
	Out        string   `json:"out"`
	Files      []string `json:"files"`
	ItemCount  int      `json:"item_count"`
	Commit     string   `json:"commit"`
	Replaced   []string `json:"replaced"`
	DryRun     bool     `json:"dry_run,omitempty"`
}

// collectionBundleCommit is the on-disk completion record. A reader trusts the
// package only when this file exists and each listed file matches its size and
// digest; anything else is an interrupted publication.
type collectionBundleCommit struct {
	Collection string                       `json:"collection"`
	ItemCount  int                          `json:"item_count"`
	Files      []collectionBundleCommitFile `json:"files"`
}

type collectionBundleCommitFile struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// collectionBundleFile is one content file of the package; produce writes its
// complete bytes.
type collectionBundleFile struct {
	name    string
	produce func(io.Writer) error
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
never writes to Zotero; run sync first if local data is missing.

Re-running into the same --out replaces the package; the result names every
existing file it replaced. ` + collectionBundleCommitName + ` is written last and lists each
file's size and SHA-256: the package is complete only while that file is
present and matches. With --dry-run nothing is written; the result lists the
files that would be written and replaced.`,
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:read-only": "true", "mcp:writes-files": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(outDir) == "" {
				return usageErr(fmt.Errorf("--out is required"))
			}

			// --dry-run creates nothing, not even the <out>.lock sibling.
			if flags.dryRun {
				return runCollectionsBundle(cmd, flags, args[0], outDir)
			}
			lockPath, _, err := outputWriterLockPath(outDir)
			if err != nil {
				return fmt.Errorf("resolving output path: %w", err)
			}
			return withPathWriterLock(cmd, lockPath, "collections bundle", func() error {
				return runCollectionsBundle(cmd, flags, args[0], outDir)
			})
		},
	}
	cmd.Flags().StringVar(&outDir, "out", "", "Output directory for the research package")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

func runCollectionsBundle(cmd *cobra.Command, flags *rootFlags, collectionKey, outDir string) error {
	db, err := openStoreForRead(cmd.Context(), "zotio")
	if err != nil {
		return err
	}
	if db == nil {
		return preconditionErr(fmt.Errorf("no local data. Run 'zotio sync' first."))
	}
	defer db.Close()

	itemCount, err := db.Count("items")
	if err != nil {
		return fmt.Errorf("checking local item store: %w", err)
	}
	if itemCount == 0 {
		return preconditionErr(fmt.Errorf("local store is empty. Run 'zotio sync' first."))
	}

	manifest, err := writeCollectionBundle(cmd.Context(), db, collectionKey, outDir, flags.dryRun)
	if err != nil {
		return err
	}
	if flags.asJSON {
		return printCommandJSON(cmd.OutOrStdout(), manifest, flags)
	}
	out := cmd.OutOrStdout()
	verb, replacedNote := "Wrote", "replaced the existing file"
	if manifest.DryRun {
		verb, replacedNote = "Dry run: would write", "would replace the existing file"
	}
	fmt.Fprintf(out, "%s %d files for collection %s (%d item(s)) to %s\n", verb, len(manifest.Files), manifest.Collection, manifest.ItemCount, manifest.Out)
	replaced := make(map[string]bool, len(manifest.Replaced))
	for _, name := range manifest.Replaced {
		replaced[name] = true
	}
	for _, name := range manifest.Files {
		if replaced[name] {
			fmt.Fprintf(out, "- %s (%s)\n", name, replacedNote)
			continue
		}
		fmt.Fprintf(out, "- %s\n", name)
	}
	fmt.Fprintf(out, "- %s (completion record, written last)\n", manifest.Commit)
	if manifest.DryRun {
		fmt.Fprintln(out, "Nothing was written.")
	}
	return nil
}

func writeCollectionBundle(ctx context.Context, db *store.Store, collKey, outDir string, dryRun bool) (collectionBundleManifest, error) {
	items, err := db.QueryItemsContext(ctx, store.ItemQuery{
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
	ftByItem, err := fulltextByParentItemWithErr(ctx, db)
	if err != nil {
		return collectionBundleManifest{}, fmt.Errorf("reading collection fulltext: %w", err)
	}

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

	files := []collectionBundleFile{
		collectionBundleBytes("synthesis.md", []byte(ensureTrailingNewline(renderCollectionMarkdown(bundle)))),
		collectionBundleBytes("annotations.md", []byte(annotationsMarkdown)),
		collectionBundleBytes("bibliography.json", bibliography),
	}
	result := collectionBundleManifest{
		Collection: collKey,
		Out:        outDir,
		Files:      make([]string, 0, len(files)),
		ItemCount:  len(items),
		Commit:     collectionBundleCommitName,
		DryRun:     dryRun,
	}
	for _, file := range files {
		result.Files = append(result.Files, file.name)
	}

	if dryRun {
		// Validate every target exactly as publication would, so the preview
		// refuses what the real run refuses, but create nothing.
		if result.Replaced, err = collectionBundleExistingFiles(outDir, files); err != nil {
			return collectionBundleManifest{}, err
		}
		return result, nil
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return collectionBundleManifest{}, fmt.Errorf("creating output directory: %w", err)
	}
	if result.Replaced, err = publishCollectionBundle(outDir, collectionBundleCommit{Collection: collKey, ItemCount: len(items)}, files); err != nil {
		return collectionBundleManifest{}, err
	}
	return result, nil
}

func collectionBundleBytes(name string, data []byte) collectionBundleFile {
	return collectionBundleFile{name: name, produce: func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}}
}

// collectionBundleExistingFiles refuses any target publication may not
// replace (symlinks, directories, special files), including the completion
// record, and names the content files already present.
func collectionBundleExistingFiles(outDir string, files []collectionBundleFile) ([]string, error) {
	replaced := make([]string, 0, len(files))
	for _, file := range files {
		exists, err := checkAtomicOutputTarget(filepath.Join(outDir, file.name))
		if err != nil {
			return nil, err
		}
		if exists {
			replaced = append(replaced, file.name)
		}
	}
	if _, err := checkAtomicOutputTarget(filepath.Join(outDir, collectionBundleCommitName)); err != nil {
		return nil, err
	}
	return replaced, nil
}

// publishCollectionBundle replaces the package in outDir as one generation and
// returns the content files it replaced.
//
// Every content file is staged complete before any target changes, so a
// failure while producing leaves the previous package, completion record
// included, exactly as it was. Only then is the old completion record removed,
// the staged files renamed into place, and a new record written last. A crash
// between those steps leaves no record, which is how a reader tells an
// interrupted publication from a complete package. Like withAtomicOutputFile
// this buys process-failure consistency, not power-loss durability; the
// record's digests let a reader verify the files after either.
func publishCollectionBundle(outDir string, commit collectionBundleCommit, files []collectionBundleFile) ([]string, error) {
	replaced, err := collectionBundleExistingFiles(outDir, files)
	if err != nil {
		return nil, err
	}

	discard := func(paths []string) {
		for _, path := range paths {
			_ = os.Remove(path)
		}
	}
	staged := make([]string, 0, len(files))
	commit.Files = make([]collectionBundleCommitFile, 0, len(files))
	for _, file := range files {
		digest := &collectionBundleDigest{h: sha256.New()}
		mode := publishedOutputMode(filepath.Join(outDir, file.name), 0o600)
		tmpPath, err := stageAtomicOutput(outDir, mode, func(w io.Writer) error {
			return file.produce(io.MultiWriter(w, digest))
		})
		if err != nil {
			discard(staged)
			return nil, fmt.Errorf("writing %s: %w (the existing package in %s is unchanged)", file.name, err, outDir)
		}
		staged = append(staged, tmpPath)
		commit.Files = append(commit.Files, collectionBundleCommitFile{Name: file.name, Bytes: digest.n, SHA256: hex.EncodeToString(digest.h.Sum(nil))})
	}

	commitPath := filepath.Join(outDir, collectionBundleCommitName)
	if err := os.Remove(commitPath); err != nil && !os.IsNotExist(err) {
		discard(staged)
		return nil, fmt.Errorf("retiring %s: %w (the existing package in %s is unchanged)", collectionBundleCommitName, err, outDir)
	}
	for i, file := range files {
		if err := os.Rename(staged[i], filepath.Join(outDir, file.name)); err != nil {
			discard(staged[i:])
			done := "none"
			if i > 0 {
				names := make([]string, 0, i)
				for _, prior := range files[:i] {
					names = append(names, prior.name)
				}
				done = strings.Join(names, ", ")
			}
			return nil, fmt.Errorf("publishing %s: %w; the package in %s is incomplete (already replaced: %s) and has no %s; rerun to publish a complete package", file.name, err, outDir, done, collectionBundleCommitName)
		}
	}
	if err := withAtomicOutputFile(commitPath, 0o600, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(commit)
	}); err != nil {
		return nil, fmt.Errorf("writing %s: %w; every content file in %s was replaced but the package is unconfirmed; rerun to publish a complete package", collectionBundleCommitName, err, outDir)
	}
	return replaced, nil
}

// collectionBundleDigest records the size and SHA-256 of the bytes a staged
// file receives, for the completion record.
type collectionBundleDigest struct {
	h hash.Hash
	n int64
}

func (d *collectionBundleDigest) Write(p []byte) (int, error) {
	d.n += int64(len(p))
	return d.h.Write(p)
}

// collectionBundleAnnotationItems adapts local store rows into the reusable
// annotations-export Markdown formatter, keeping bundle annotation rendering
// byte-for-byte aligned with `annotations export`.
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
