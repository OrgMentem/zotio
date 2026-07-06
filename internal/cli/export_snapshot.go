// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// PATCH(glean roadmap-phase6 d27f99d4): `export snapshot` — a truly paginated,
// resumable, reproducible export. Unlike the generated single-page `export`, it
// walks every page (start/limit) of a structured item set, streams JSONL to a
// data file (append-resumable via a checkpoint sidecar), and writes a lockfile
// recording each item's key+version plus a content hash so the snapshot is
// reproducible and drift is detectable. Uses structured item JSON, never the
// formatted-bibliography mode (which ignores limit/pagination).

package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newExportSnapshotCmd(flags *rootFlags) *cobra.Command {
	var outputFile string
	var pageSize int
	var limit int
	var resume bool

	cmd := &cobra.Command{
		Use:   "snapshot [scope]",
		Short: "Reproducible, resumable paginated export with a content lockfile",
		Long: `Export a structured item set (JSONL) across all pages into a data file,
plus a sidecar lockfile (<output>.lock.json) recording each item's key+version
and a content hash for reproducibility/drift detection. Resumable: an interrupted
run can continue with --resume (a <output>.checkpoint.json sidecar tracks progress).

Scope is one of: library (default), collection:KEY, or tag:NAME.`,
		Example: `  zotio export snapshot --output backup.jsonl
  zotio export snapshot collection:ABCD1234 --output coll.jsonl
  zotio export snapshot --output backup.jsonl --resume`,
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
			if strings.TrimSpace(outputFile) == "" {
				return usageErr(fmt.Errorf("--output is required for export snapshot (it writes a data file and a .lock.json sidecar)"))
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}

			checkpointFile := outputFile + ".checkpoint.json"
			// PATCH(glean review P1): only append when a checkpoint for THIS scope is
			// genuinely resumable (matching path, not done). --resume on a finished,
			// missing, or mismatched checkpoint must truncate, else the fetch restarts
			// at offset 0 while appending and silently duplicates the snapshot.
			resumable := false
			if resume {
				if cp, ok := readExportCheckpoint(checkpointFile); ok && cp.Path == path && !cp.Done {
					resumable = true
				}
			}
			openFlags := os.O_CREATE | os.O_WRONLY
			if resumable {
				openFlags |= os.O_APPEND
			} else {
				openFlags |= os.O_TRUNC
				_ = os.Remove(checkpointFile)
			}
			f, err := os.OpenFile(outputFile, openFlags, 0o644)
			if err != nil {
				return fmt.Errorf("opening output: %w", err)
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
				// PATCH(glean review P1): flush each page to the OS before the engine
				// advances the checkpoint, so an abrupt interrupt cannot leave the
				// checkpoint ahead of the data file (a later --resume would skip the tail).
				return w.Flush()
			}

			fetched, fetchErr := resumablePaginatedFetch(cmd.Context(), c, path, params, pageSize, limit, checkpointFile, onPage)
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

			// Build the lockfile from the complete data file so --resume runs
			// produce a correct full-set fingerprint, not just the new pages.
			items, err := readJSONLItems(outputFile)
			if err != nil {
				return err
			}
			lf := buildExportLockfile(scopeLabel, "jsonl", items)
			lockPath := outputFile + ".lock.json"
			lockFile, err := os.Create(lockPath)
			if err != nil {
				return fmt.Errorf("writing lockfile: %w", err)
			}
			if err := writeExportLockfile(lockFile, lf); err != nil {
				_ = lockFile.Close()
				return err
			}
			if err := lockFile.Close(); err != nil {
				return err
			}
			_ = os.Remove(checkpointFile)

			report, _ := json.Marshal(map[string]any{
				"scope":          scopeLabel,
				"output":         outputFile,
				"lockfile":       lockPath,
				"fetched":        fetched,
				"count":          lf.Count,
				"content_sha256": lf.ContentSHA256,
			})
			return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(report), flags)
		},
	}
	cmd.Flags().StringVarP(&outputFile, "output", "o", "", "Output JSONL data file (required); the lockfile is written to <output>.lock.json")
	cmd.Flags().IntVar(&pageSize, "page-size", 100, "Items per API page (1-100)")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum items to export (0 = all)")
	cmd.Flags().BoolVar(&resume, "resume", false, "Resume an interrupted snapshot from its checkpoint sidecar")
	return cmd
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
