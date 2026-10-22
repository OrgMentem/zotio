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
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/client"
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

Scope uses the shared --scope grammar: library (default), collection:KEY,
tag:NAME, or collection:KEY AND tag:NAME. The data file and the manifest are
each published atomically (a temporary file renamed into place), so a failed
run keeps the previous snapshot. A JSONL run keeps its in-progress data in
<output>.partial until it finishes.`,
		Example: `  zotio export snapshot --output backup.jsonl
  zotio export snapshot --format bibtex --output library.bib
  zotio export snapshot collection:ABCD2345 --format ris --output coll.ris
  zotio export snapshot "collection:ABCD2345 AND tag:to-read" --output subset.jsonl
  zotio export snapshot --format csljson --output library.json --resume`,
		Args:        cobra.MaximumNArgs(1),
		Annotations: map[string]string{"mcp:read-only": "true", "mcp:writes-files": "true"},
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
			if err := validateLimitFlag("--limit", limit); err != nil {
				return err
			}
			if pageSize < 1 || pageSize > 100 {
				return usageErr(fmt.Errorf("invalid --page-size %d: must be between 1 and 100", pageSize))
			}
			if flags.dryRun {
				return previewExportSnapshot(cmd, flags, outputFile, path, params, scopeLabel, pageSize, limit, resume, format)
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
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum items to export (0 = no limit)")
	cmd.Flags().BoolVar(&resume, "resume", false, "Resume an interrupted snapshot from its checkpoint sidecar")
	cmd.AddCommand(newExportSnapshotVerifyCmd(flags))
	return cmd
}

// snapshotPartialPath is where a JSONL snapshot keeps its in-progress data. It
// is renamed over the output only when the run completes, so an interrupted run
// never truncates or replaces the previous snapshot, and --resume continues
// from this file.
func snapshotPartialPath(outputFile string) string {
	return outputFile + ".partial"
}

// verifySnapshotCollection confirms a collection-scoped snapshot names a
// collection that exists. The Zotero local API answers
// /collections/<missing>/items with the whole library, so the item listing
// alone cannot tell a typo from a real collection.
func verifySnapshotCollection(c *client.Client, flags *rootFlags, path string) error {
	rest, ok := strings.CutPrefix(path, "/collections/")
	if !ok {
		return nil
	}
	escaped, _, _ := strings.Cut(rest, "/")
	key, err := url.PathUnescape(escaped)
	if err != nil {
		return usageErr(fmt.Errorf("invalid collection key %q: %w", escaped, err))
	}
	if _, err := c.Get("/collections/"+escaped, nil); err != nil {
		if apiStatus(err) == 404 {
			return notFoundErr(fmt.Errorf("collection %s not found", key))
		}
		return classifyAPIError(err, flags)
	}
	return nil
}

// snapshotReplacedPaths refuses an output or manifest path that cannot be
// published over (symlink, directory, device) before any fetch, and returns
// the existing regular files this run replaces.
func snapshotReplacedPaths(paths ...string) ([]string, error) {
	replaced := []string{}
	for _, p := range paths {
		exists, err := checkAtomicOutputTarget(p)
		if err != nil {
			return nil, err
		}
		if exists {
			replaced = append(replaced, p)
		}
	}
	return replaced, nil
}

// publishStagedFile renames a completed staging file over target.
func publishStagedFile(staged, target string) error {
	if _, err := checkAtomicOutputTarget(target); err != nil {
		return err
	}
	if err := os.Rename(staged, target); err != nil {
		return fmt.Errorf("publishing %s: %w", target, err)
	}
	return nil
}

// snapshotManifestPartialPath is where a finished manifest waits for commit.
func snapshotManifestPartialPath(outputFile string) string {
	return outputFile + ".manifest.json.partial"
}

// checkSnapshotStagingFile refuses a staging path that exists as anything but a
// regular file. A symlink there would send the snapshot's bytes (or a
// truncation) to whatever it points at.
func checkSnapshotStagingFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspecting staging file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to use staging file %s: it is not a regular file (mode %s); remove it and rerun", path, info.Mode())
	}
	return nil
}

// openSnapshotStagingFile opens a staging file owner-only without following a
// symlink, and confirms the opened file is a regular file. The Lstat check
// alone leaves a window between check and open; O_NOFOLLOW closes it where the
// platform has it.
func openSnapshotStagingFile(path string, flag int) (*os.File, error) {
	if err := checkSnapshotStagingFile(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, flag|noFollowOpenFlag, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("refusing to use staging file %s: it is not a regular file (mode %s)", path, info.Mode())
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// stageSnapshotManifest writes the finished manifest to its .partial path
// atomically, so a manifest .partial that exists is always complete.
func stageSnapshotManifest(outputFile string, lf exportLockfile) error {
	if err := withAtomicOutputFile(snapshotManifestPartialPath(outputFile), exportOutputFileMode, func(w io.Writer) error {
		return writeExportLockfile(w, lf)
	}); err != nil {
		return fmt.Errorf("staging manifest: %w", err)
	}
	return nil
}

// snapshotBetweenRenames is a test seam: it runs after the data file is
// published and before the manifest is, the window a crash can land in.
var snapshotBetweenRenames func() error

// commitSnapshot publishes one fully staged generation: the data file, then the
// manifest. Both .partial files stay in place until their own rename, so an
// interruption between the renames leaves a manifest .partial that
// recoverPendingSnapshot finishes.
func commitSnapshot(outputFile string) error {
	dataPartial := snapshotPartialPath(outputFile)
	if _, err := os.Lstat(dataPartial); err == nil {
		if err := publishStagedFile(dataPartial, outputFile); err != nil {
			return err
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		// Data already published by an interrupted run; it must be there.
		if _, statErr := os.Lstat(outputFile); statErr != nil {
			return fmt.Errorf("staged snapshot data %s is missing and %s does not exist: %w", dataPartial, outputFile, statErr)
		}
	} else {
		return fmt.Errorf("inspecting %s: %w", dataPartial, err)
	}
	if snapshotBetweenRenames != nil {
		if err := snapshotBetweenRenames(); err != nil {
			return err
		}
	}
	return publishStagedFile(snapshotManifestPartialPath(outputFile), outputFile+".manifest.json")
}

// recoverPendingSnapshot handles a manifest .partial left by a run that died
// after staging a generation. With --resume it finishes publishing that
// generation without fetching anything. Without --resume it refuses: a fresh
// run could leave newly published data beside an older manifest.
func recoverPendingSnapshot(cmd *cobra.Command, flags *rootFlags, outputFile, checkpointFile, scopeLabel, format string, resume bool) (bool, error) {
	manifestPartial := snapshotManifestPartialPath(outputFile)
	if _, err := os.Lstat(manifestPartial); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return true, fmt.Errorf("inspecting %s: %w", manifestPartial, err)
	}
	if !resume {
		return true, preconditionErr(fmt.Errorf("an interrupted snapshot publication is pending (%s); run again with --resume to finish it, or remove %s and %s to discard it", manifestPartial, manifestPartial, snapshotPartialPath(outputFile)))
	}
	lf, err := readExportLockfile(manifestPartial)
	if err != nil {
		return true, fmt.Errorf("pending snapshot manifest %s is unreadable: %w; remove the .partial files and rerun without --resume", manifestPartial, err)
	}
	if lf.Scope != scopeLabel || lf.Format != format {
		return true, fmt.Errorf("pending snapshot publication is for scope %q format %q, not scope %q format %q; rerun with matching arguments or remove the .partial files", lf.Scope, lf.Format, scopeLabel, format)
	}
	if err := commitSnapshot(outputFile); err != nil {
		return true, err
	}
	_ = os.Remove(checkpointFile)
	_ = os.Remove(checkpointFile + ".items.jsonl")
	report, _ := json.Marshal(map[string]any{
		"scope":          scopeLabel,
		"output":         outputFile,
		"manifest":       outputFile + ".manifest.json",
		"count":          lf.Count,
		"content_sha256": lf.ContentSHA256,
		"recovered":      true,
	})
	return true, printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(report), flags)
}

func exportSnapshot(cmd *cobra.Command, flags *rootFlags, outputFile, path string, params map[string]string, scopeLabel string, pageSize, limit int, resume bool, format string) error {
	checkpointFile := outputFile + ".checkpoint.json"
	manifestPath := outputFile + ".manifest.json"
	replaced, err := snapshotReplacedPaths(outputFile, manifestPath)
	if err != nil {
		return err
	}
	for _, staging := range []string{snapshotPartialPath(outputFile), snapshotManifestPartialPath(outputFile)} {
		if err := checkSnapshotStagingFile(staging); err != nil {
			return err
		}
	}
	if handled, err := recoverPendingSnapshot(cmd, flags, outputFile, checkpointFile, scopeLabel, format, resume); handled || err != nil {
		return err
	}
	c, resumable, resumeCheckpoint, err := resolveSnapshotResume(flags, checkpointFile, path, params, pageSize, limit, resume, format)
	if err != nil {
		return err
	}
	if format != "jsonl" {
		return exportTranslatorSnapshot(cmd, flags, c, outputFile, path, params, scopeLabel, pageSize, limit, checkpointFile, resumable, format, replaced)
	}
	partialFile := snapshotPartialPath(outputFile)
	var committedOffset int64
	if resumable {
		committedOffset, err = checkpointJSONLOffset(partialFile, resumeCheckpoint)
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
	f, err := openSnapshotStagingFile(partialFile, openFlags)
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
	items, err := readJSONLItems(partialFile)
	if err != nil {
		return err
	}
	lf, err := buildExportLockfile(scopeLabel, "jsonl", items)
	if err != nil {
		return err
	}
	lf.Truncated = limit > 0 && lf.Count >= limit
	// Stage the manifest next to the staged data, then publish both as one
	// generation. Until commitSnapshot finishes, both stay as .partial files,
	// so a run that dies between the two renames can be completed by --resume
	// without refetching.
	if err := stageSnapshotManifest(outputFile, lf); err != nil {
		return err
	}
	if err := commitSnapshot(outputFile); err != nil {
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
		"replaced":       replaced,
	})
	return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(report), flags)
}

// resolveSnapshotResume builds the client, sets the translator request
// parameters, and decides whether --resume continues an incomplete
// checkpoint. It only reads, so a run and its --dry-run preview share it and
// reach the same answer.
func resolveSnapshotResume(flags *rootFlags, checkpointFile, path string, params map[string]string, pageSize, limit int, resume bool, format string) (*client.Client, bool, exportCheckpoint, error) {
	c, err := flags.newClient()
	if err != nil {
		return nil, false, exportCheckpoint{}, err
	}
	if err := verifySnapshotCollection(c, flags, path); err != nil {
		return nil, false, exportCheckpoint{}, err
	}
	if format != "jsonl" {
		params["format"] = "json"
		params["include"] = "data," + format
	}
	if !resume {
		return c, false, exportCheckpoint{}, nil
	}
	source := exportReadSource(c, flags.profileName)
	expectedScope := exportCheckpointScope(source, path, params, normalizedExportPageSize(pageSize), limit)
	// Append only when every request and library identity field matches.
	// Reject legacy or foreign incomplete checkpoints before opening the output.
	cp, exists, cpErr := readExportCheckpointStatus(checkpointFile)
	if cpErr != nil {
		return nil, false, exportCheckpoint{}, fmt.Errorf("export checkpoint %q is corrupt: %w; remove it or rerun without --resume (existing output left intact)", checkpointFile, cpErr)
	}
	if !exists || cp.Done {
		return c, false, exportCheckpoint{}, nil
	}
	if cp.Path != path || cp.Source != source || cp.Scope != expectedScope || checkpointFormat(cp) != format {
		return nil, false, exportCheckpoint{}, fmt.Errorf("checkpoint scope does not match this export; remove the checkpoint or rerun without --resume")
	}
	return c, true, cp, nil
}

// previewExportSnapshot is `export snapshot --dry-run`: it reports the files a
// run would write, replace, and delete, and whether --resume would continue,
// without taking the output lock, fetching, or touching any file.
func previewExportSnapshot(cmd *cobra.Command, flags *rootFlags, outputFile, path string, params map[string]string, scopeLabel string, pageSize, limit int, resume bool, format string) error {
	checkpointFile := outputFile + ".checkpoint.json"
	manifestPath := outputFile + ".manifest.json"
	_, resumable, _, err := resolveSnapshotResume(flags, checkpointFile, path, params, pageSize, limit, resume, format)
	if err != nil {
		return err
	}
	// The run removes its checkpoint sidecars: at the start when it does not
	// resume, and on success either way. A fresh run also discards a stale
	// staging file.
	sidecars := []string{checkpointFile}
	if !resumable {
		sidecars = append(sidecars, snapshotPartialPath(outputFile))
	}
	if format != "jsonl" {
		sidecars = append(sidecars, checkpointFile+".items.jsonl")
	}
	wouldReplace, err := existingSnapshotPaths(outputFile, manifestPath)
	if err != nil {
		return err
	}
	wouldDelete, err := existingSnapshotPaths(sidecars...)
	if err != nil {
		return err
	}
	pending, err := existingSnapshotPaths(snapshotManifestPartialPath(outputFile))
	if err != nil {
		return err
	}
	report, err := json.Marshal(map[string]any{
		"dry_run":       true,
		"scope":         scopeLabel,
		"format":        format,
		"output":        outputFile,
		"manifest":      manifestPath,
		"resume":        resumable,
		"would_replace": wouldReplace,
		"would_delete":  wouldDelete,
		// A manifest .partial means an earlier run died while publishing: --resume
		// finishes it, and a run without --resume is refused.
		"pending_publication": len(pending) > 0,
	})
	if err != nil {
		return err
	}
	return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(report), flags)
}

// existingSnapshotPaths returns the paths that exist, in order. Any stat error
// other than absence is returned so a preview never under-reports a target.
func existingSnapshotPaths(paths ...string) ([]string, error) {
	out := []string{}
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("inspecting %q: %w", p, err)
		}
	}
	return out, nil
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
// The scope uses the shared --scope grammar; the Web API can serve library,
// collection:KEY, tag:NAME, and collection:KEY AND tag:NAME as one structured
// item request. Other arms (item, query, saved-search) have no single
// structured item endpoint and are refused. Structured item endpoints only
// (never the formatted-bibliography mode).
func snapshotScopePath(scopeArg string) (path string, params map[string]string, label string, err error) {
	expr := strings.TrimSpace(scopeArg)
	if expr == "" {
		expr = "library"
	}
	spec, err := parseScopeSpec(expr)
	if err != nil {
		return "", nil, "", err
	}
	if err := validateScopeKeys(spec); err != nil {
		return "", nil, "", err
	}
	unsupported := func() error {
		return usageErr(fmt.Errorf("unsupported snapshot scope %q; use library, collection:KEY, tag:NAME, or collection:KEY AND tag:NAME", scopeArg))
	}
	switch spec.Type {
	case "library":
		return "/items", map[string]string{}, "library", nil
	case "collection":
		return "/collections/" + url.PathEscape(spec.Value) + "/items", map[string]string{}, spec.expr(), nil
	case "tag":
		return "/items", map[string]string{"tag": spec.Value}, spec.expr(), nil
	case scopeTypeIntersection:
		var collection, tag string
		for _, arm := range spec.Arms {
			switch arm.Type {
			case "collection":
				if collection != "" {
					return "", nil, "", unsupported()
				}
				collection = arm.Value
			case "tag":
				if tag != "" {
					return "", nil, "", unsupported()
				}
				tag = arm.Value
			default:
				return "", nil, "", unsupported()
			}
		}
		if collection == "" || tag == "" {
			return "", nil, "", unsupported()
		}
		return "/collections/" + url.PathEscape(collection) + "/items", map[string]string{"tag": tag}, spec.expr(), nil
	default:
		return "", nil, "", unsupported()
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
