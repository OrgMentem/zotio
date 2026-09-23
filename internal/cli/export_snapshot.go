// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// `export snapshot` walks every page of a structured item set, streams the
// requested format to a resumable data file, and records canonical item
// content in a manifest for drift detection.

package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func newExportSnapshotCmd(flags *rootFlags) *cobra.Command {
	var outputFile string
	var pageSize int
	var limit int
	var resume bool
	var format string

	cmd := &cobra.Command{
		Use:   "snapshot [scope]",
		Short: "Reproducible, resumable paginated export with a content manifest",
		Long: `Export every page of a library, collection, or tag scope as JSONL, BibTeX,
RIS, or CSL-JSON. A sidecar manifest (<output>.manifest.json) records each
item's key, version, and canonical data hash for drift detection. An
interrupted run continues with --resume using <output>.checkpoint.json.
Translator formats need Zotero to return the requested include field.

Scope is one of: library (default), collection:KEY, or tag:NAME.`,
		Example: `  zotio export snapshot --output backup.jsonl
  zotio export snapshot --format bibtex --output library.bib
  zotio export snapshot collection:ABCD1234 --format ris --output coll.ris
  zotio export snapshot --format csljson --output library.json --resume`,
		Args:        cobra.MaximumNArgs(1),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			scopeArg := "library"
			if len(args) > 0 {
				scopeArg = args[0]
			}
			path, params, scopeLabel, err := snapshotScopePath(scopeArg)
			if err != nil {
				return err
			}
			switch format {
			case "jsonl", "bibtex", "ris", "csljson":
			default:
				return usageErr(fmt.Errorf("unknown snapshot format %q: use jsonl, bibtex, ris, or csljson", format))
			}
			if strings.TrimSpace(outputFile) == "" {
				return usageErr(fmt.Errorf("--output is required for export snapshot (it writes a data file and a .manifest.json sidecar)"))
			}

			lockPath, _, err := outputWriterLockPath(outputFile)
			if err != nil {
				return fmt.Errorf("resolving output path: %w", err)
			}
			// The lock file is deliberately left behind. Plain exports and
			// --deliver=file now share this key, and unlinking it could drop an
			// inode a concurrent acquirer has already flocked, letting a third
			// writer lock a fresh inode under the same name. See ADR-0005.
			return withPathWriterLock(cmd, lockPath, "export snapshot", func() error {
				return exportSnapshot(cmd, flags, outputFile, path, params, scopeLabel, pageSize, limit, resume, format)
			})
		},
	}
	cmd.Flags().StringVarP(&outputFile, "output", "o", "", "Output data file (required); the manifest is written to <output>.manifest.json")
	cmd.Flags().StringVar(&format, "format", "jsonl", "Snapshot format: jsonl, bibtex, ris, or csljson")
	cmd.Flags().IntVar(&pageSize, "page-size", 100, "Items per API page (1-100)")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum items to export (0 = all)")
	cmd.Flags().BoolVar(&resume, "resume", false, "Resume an interrupted snapshot from its checkpoint sidecar")
	cmd.AddCommand(newExportSnapshotVerifyCmd(flags))
	return cmd
}

func exportSnapshot(cmd *cobra.Command, flags *rootFlags, outputFile, path string, params map[string]string, scopeLabel string, pageSize, limit int, resume bool, format string) error {
	c, err := flags.newClient()
	if err != nil {
		return err
	}
	if format != "jsonl" {
		params["format"] = "json"
		params["include"] = "data," + format
	}

	checkpointFile := outputFile + ".checkpoint.json"
	source := exportReadSource(c, flags.profileName)
	expectedScope := exportCheckpointScope(source, path, params, normalizedExportPageSize(pageSize), limit)
	// Append only when every request and library identity field matches.
	// Reject legacy or foreign incomplete checkpoints before opening the output.
	resumable := false
	var resumeCheckpoint exportCheckpoint
	if resume {
		if cp, ok := readExportCheckpoint(checkpointFile); ok && !cp.Done {
			if cp.Path != path || cp.Source != source || cp.Scope != expectedScope || checkpointFormat(cp) != format {
				return fmt.Errorf("checkpoint scope does not match this export; remove the checkpoint or rerun without --resume")
			}
			resumable = true
			resumeCheckpoint = cp
		}
	}
	if format != "jsonl" {
		return exportTranslatorSnapshot(cmd, flags, c, outputFile, path, params, scopeLabel, pageSize, limit, checkpointFile, resumable, format)
	}
	var committedOffset int64
	if resumable {
		committedOffset, err = checkpointJSONLOffset(outputFile, resumeCheckpoint)
		if err != nil {
			return fmt.Errorf("resuming JSONL snapshot: %w; rerun without --resume to start over", err)
		}
	} else {
		_ = os.Remove(checkpointFile)
	}
	openFlags := os.O_WRONLY | os.O_APPEND
	if !resumable {
		openFlags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	f, err := openPrivateOutputFile(outputFile, openFlags)
	if err != nil {
		return fmt.Errorf("opening output: %w", err)
	}
	if resumable {
		if err := f.Truncate(committedOffset); err != nil {
			_ = f.Close()
			return fmt.Errorf("truncating uncommitted JSONL data: %w", err)
		}
	}
	w := bufio.NewWriter(f)

	onPage := func(page []json.RawMessage) error {
		for _, item := range page {
			var buf bytes.Buffer
			if err := json.Compact(&buf, item); err != nil {
				return err
			}
			if _, err := w.Write(buf.Bytes()); err != nil {
				return err
			}
			if err := w.WriteByte('\n'); err != nil {
				return err
			}
		}
		// flush each page to the OS before the engine
		// advances the checkpoint, so an abrupt interrupt cannot leave the
		// checkpoint ahead of the data file (a later --resume would skip the tail).
		return w.Flush()
	}

	fetched, fetchErr := resumablePaginatedFetch(cmd.Context(), c, path, params, pageSize, limit, checkpointFile, flags.profileName, onPage, format, func() (int64, error) {
		return f.Seek(0, 1)
	})
	flushErr := w.Flush()
	closeErr := f.Close()
	if fetchErr != nil {
		return fetchErr
	}
	if flushErr != nil {
		return flushErr
	}
	if closeErr != nil {
		return closeErr
	}

	// Build the manifest from the complete data file so --resume runs
	// produce a correct full-set fingerprint, not just the new pages.
	items, err := readJSONLItems(outputFile)
	if err != nil {
		return err
	}
	lf, err := buildExportLockfile(scopeLabel, "jsonl", items)
	if err != nil {
		return err
	}
	manifestPath := outputFile + ".manifest.json"
	manifestFile, err := openPrivateOutputFile(manifestPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("writing manifest: %w", err)
	}
	if err := writeExportLockfile(manifestFile, lf); err != nil {
		_ = manifestFile.Close()
		return err
	}
	if err := manifestFile.Close(); err != nil {
		return err
	}
	_ = os.Remove(checkpointFile)

	report, _ := json.Marshal(map[string]any{
		"scope":          scopeLabel,
		"output":         outputFile,
		"manifest":       manifestPath,
		"fetched":        fetched,
		"count":          lf.Count,
		"content_sha256": lf.ContentSHA256,
	})
	return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(report), flags)
}

// canonicalOutputPath provides one lock identity for equivalent output paths.
// It resolves symlink components even when their final target does not exist.
func canonicalOutputPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return resolveOutputSymlinks(filepath.Clean(abs), 0)
}

func resolveOutputSymlinks(path string, depth int) (string, error) {
	if depth >= 40 {
		return "", fmt.Errorf("resolving output path symlinks: too many links (possible cycle)")
	}
	volume := filepath.VolumeName(path)
	remaining := strings.TrimPrefix(path, volume)
	remaining = strings.TrimPrefix(remaining, string(filepath.Separator))
	current := volume + string(filepath.Separator)
	parts := strings.Split(remaining, string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return filepath.Join(append([]string{current}, parts[i+1:]...)...), nil
			}
			return "", fmt.Errorf("resolving output path %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(current)
		if err != nil {
			return "", fmt.Errorf("reading output symlink %q: %w", current, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(current), target)
		}
		target = filepath.Join(append([]string{target}, parts[i+1:]...)...)
		return resolveOutputSymlinks(filepath.Clean(target), depth+1)
	}
	return filepath.Clean(current), nil
}

// snapshotScopePath maps a snapshot scope to a Web API path + query params.
// Structured item endpoints only (never the formatted-bibliography mode).
func snapshotScopePath(scopeArg string) (path string, params map[string]string, label string, err error) {
	s := strings.TrimSpace(scopeArg)
	switch {
	case s == "" || s == "library":
		return "/items", map[string]string{}, "library", nil
	case strings.HasPrefix(s, "collection:"):
		key := strings.TrimSpace(strings.TrimPrefix(s, "collection:"))
		if key == "" {
			return "", nil, "", usageErr(fmt.Errorf("collection scope needs a key, e.g. collection:ABCD1234"))
		}
		return "/collections/" + url.PathEscape(key) + "/items", map[string]string{}, s, nil
	case strings.HasPrefix(s, "tag:"):
		tag := strings.TrimSpace(strings.TrimPrefix(s, "tag:"))
		if tag == "" {
			return "", nil, "", usageErr(fmt.Errorf("tag scope needs a name, e.g. tag:to-read"))
		}
		return "/items", map[string]string{"tag": tag}, s, nil
	default:
		return "", nil, "", usageErr(fmt.Errorf("unsupported snapshot scope %q; use library, collection:KEY, or tag:NAME", scopeArg))
	}
}

// readJSONLItems reads a JSONL data file back into raw item objects, used to
// build the lockfile over the complete (possibly resumed) export.
func readJSONLItems(path string) ([]json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading export data: %w", err)
	}
	defer f.Close()
	items := make([]json.RawMessage, 0)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		items = append(items, append(json.RawMessage(nil), line...))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// A new checkpoint records the last committed byte. Older checkpoints carry
// only an item count, so locate that many complete JSONL lines before resume.
func checkpointJSONLOffset(path string, cp exportCheckpoint) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("opening checkpoint data: %w", err)
	}
	defer f.Close()
	size, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if cp.DataBytes != nil {
		if *cp.DataBytes < 0 || *cp.DataBytes > size.Size() {
			return 0, fmt.Errorf("checkpoint byte offset %d exceeds the data file (%d bytes)", *cp.DataBytes, size.Size())
		}
		return *cp.DataBytes, nil
	}
	reader := bufio.NewReader(f)
	var offset int64
	for i := range cp.Fetched {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return 0, fmt.Errorf("legacy checkpoint records %d items, but item %d has no complete line: %w", cp.Fetched, i+1, err)
		}
		if !json.Valid(bytes.TrimSpace(line)) {
			return 0, fmt.Errorf("legacy checkpoint item %d is not valid JSON", i+1)
		}
		offset += int64(len(line))
	}
	return offset, nil
}
