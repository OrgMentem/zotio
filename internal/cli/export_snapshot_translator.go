// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"zotio/internal/client"

	"github.com/spf13/cobra"
)

// The companion file retains the structured API items until the manifest is
// published. Translator output alone cannot reconstruct their content hashes.
func exportTranslatorSnapshot(cmd *cobra.Command, flags *rootFlags, c *client.Client, outputFile, path string, params map[string]string, scopeLabel string, pageSize, limit int, checkpointFile string, resume bool, format string, replaced []string) error {
	itemsFile := checkpointFile + ".items.jsonl"
	if err := checkSnapshotStagingFile(itemsFile); err != nil {
		return err
	}
	var items []json.RawMessage
	if resume {
		cp, _ := readExportCheckpoint(checkpointFile)
		var err error
		items, err = checkpointSnapshotItems(itemsFile, cp.Fetched)
		if err != nil {
			return fmt.Errorf("resuming snapshot: %w", err)
		}
	} else {
		_ = os.Remove(checkpointFile)
	}

	metadataFlags := os.O_CREATE | os.O_WRONLY
	if resume {
		metadataFlags |= os.O_APPEND
	} else {
		metadataFlags |= os.O_TRUNC
	}
	metadata, err := openSnapshotStagingFile(itemsFile, metadataFlags)
	if err != nil {
		return fmt.Errorf("opening snapshot checkpoint items: %w", err)
	}
	defer metadata.Close()
	metadataWriter := bufio.NewWriter(metadata)

	// entries counts records written to the data file, not items fetched:
	// non-bibliographic items are fetched and manifested but never written, so
	// the CSL-JSON separator must follow what the file actually holds.
	entries, skipped := 0, 0
	var fetched int
	// The data file is rebuilt from the committed items on every run (an
	// interrupted page may have written bytes before the engine advanced the
	// checkpoint), so it is staged in a temporary file and renamed into place
	// only when the fetch completes. A failed run keeps the previous snapshot;
	// --resume continues from the checkpoint items file, not from the output.
	err = withAtomicOutputFile(snapshotPartialPath(outputFile), exportOutputFileMode, func(output io.Writer) error {
		if format == "csljson" {
			if _, err := io.WriteString(output, "["); err != nil {
				return err
			}
		}
		if len(items) > 0 {
			page, n, s, err := snapshotPageBytes(items, format)
			if err != nil {
				return err
			}
			if err := appendSnapshotPage(output, page, format, false); err != nil {
				return err
			}
			entries, skipped = n, s
		}
		onPage := func(page []json.RawMessage) error {
			// Validate the whole page before writing any of it.
			data, n, s, err := snapshotPageBytes(page, format)
			if err != nil {
				return err
			}
			var raw bytes.Buffer
			for _, item := range page {
				if err := json.Compact(&raw, item); err != nil {
					return err
				}
				raw.WriteByte('\n')
			}
			if _, err := metadataWriter.Write(raw.Bytes()); err != nil {
				return err
			}
			if err := metadataWriter.Flush(); err != nil {
				return err
			}
			if n > 0 {
				if err := appendSnapshotPage(output, data, format, entries > 0); err != nil {
					return err
				}
			}
			entries += n
			skipped += s
			items = append(items, page...)
			return nil
		}
		var fetchErr error
		fetched, fetchErr = resumablePaginatedFetch(cmd.Context(), c, path, params, pageSize, limit, checkpointFile, flags.profileName, onPage, format, nil)
		if fetchErr != nil {
			return fetchErr
		}
		if format == "csljson" {
			if _, err := io.WriteString(output, "]"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := metadata.Close(); err != nil {
		return err
	}
	lf, err := buildExportLockfile(scopeLabel, format, items)
	if err != nil {
		return err
	}
	lf.Truncated = limit > 0 && lf.Count >= limit
	manifestPath := outputFile + ".manifest.json"
	// Stage the manifest beside the staged data and publish both as one
	// generation (see commitSnapshot), so --resume can finish an interrupted
	// publication without refetching.
	if err := stageSnapshotManifest(outputFile, lf); err != nil {
		return err
	}
	if err := commitSnapshot(outputFile); err != nil {
		return err
	}
	_ = os.Remove(checkpointFile)
	_ = os.Remove(itemsFile)
	report, _ := json.Marshal(map[string]any{
		"scope": scopeLabel, "output": outputFile, "manifest": manifestPath,
		"fetched": fetched, "count": lf.Count, "content_sha256": lf.ContentSHA256,
		"entries": entries, "skipped_non_bibliographic": skipped,
		"replaced": replaced,
	})
	return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(report), flags)
}

// nonBibliographicItemTypes carry no citation of their own. On both the local
// and the Web API their bibtex and ris include fields are blank, and csljson
// renders them as a "document" pseudo-record, so a translator snapshot skips
// them in every format. The manifest still records them: verify covers the
// whole scope, not only what the data file contains.
var nonBibliographicItemTypes = map[string]bool{"attachment": true, "note": true, "annotation": true}

// snapshotPageBytes renders one page in a translator format and returns the
// bytes, how many records they hold, and how many items were skipped as
// non-bibliographic.
func snapshotPageBytes(page []json.RawMessage, format string) ([]byte, int, int, error) {
	var buf bytes.Buffer
	entries, skipped := 0, 0
	for _, item := range page {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(item, &object); err != nil {
			return nil, 0, 0, apiErr(fmt.Errorf("decoding snapshot item: %w", err))
		}
		key := exportItemKey(item)
		var data struct {
			ItemType string `json:"itemType"`
		}
		if len(object["data"]) == 0 || bytes.Equal(object["data"], []byte("null")) || json.Unmarshal(object["data"], &data) != nil {
			return nil, 0, 0, apiErr(fmt.Errorf("item %q lacks data for %s snapshot", key, format))
		}
		if nonBibliographicItemTypes[data.ItemType] {
			skipped++
			continue
		}
		value := object[format]
		if len(value) == 0 || bytes.Equal(value, []byte("null")) {
			return nil, 0, 0, apiErr(fmt.Errorf("item %q lacks %s include field", key, format))
		}
		if format == "csljson" {
			records, ok := cslJSONRecords(value)
			if !ok {
				return nil, 0, 0, apiErr(fmt.Errorf("item %q has invalid %s include field", key, format))
			}
			for _, record := range records {
				if entries > 0 {
					buf.WriteByte(',')
				}
				if err := json.Compact(&buf, record); err != nil {
					return nil, 0, 0, apiErr(fmt.Errorf("item %q has invalid %s include field", key, format))
				}
				entries++
			}
			continue
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil || strings.TrimSpace(text) == "" {
			return nil, 0, 0, apiErr(fmt.Errorf("item %q has invalid %s include field", key, format))
		}
		buf.WriteString(text)
		entries++
	}
	return buf.Bytes(), entries, skipped, nil
}

// cslJSONRecords accepts both shapes Zotero serves for include=csljson: the
// Web API embeds one CSL object, while the local API embeds a JSON string
// holding an array of them. Anything else, or an empty result, is invalid.
func cslJSONRecords(value json.RawMessage) ([]json.RawMessage, bool) {
	var text string
	if json.Unmarshal(value, &text) == nil {
		value = json.RawMessage(text)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(value, &object) == nil && object != nil {
		return []json.RawMessage{value}, true
	}
	var records []json.RawMessage
	if json.Unmarshal(value, &records) != nil || len(records) == 0 {
		return nil, false
	}
	for _, record := range records {
		if json.Unmarshal(record, &object) != nil || object == nil {
			return nil, false
		}
	}
	return records, true
}

func appendSnapshotPage(output io.Writer, data []byte, format string, preceded bool) error {
	if format == "csljson" && preceded {
		if _, err := io.WriteString(output, ","); err != nil {
			return err
		}
	}
	_, err := output.Write(data)
	return err
}

// The checkpoint count, not the companion file length, is authoritative:
// a crash can leave part of an uncommitted page in the companion file.
func checkpointSnapshotItems(path string, count int) ([]json.RawMessage, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("reading snapshot checkpoint items: %w", err)
	}
	defer f.Close()
	items := make([]json.RawMessage, 0, count)
	reader := bufio.NewReader(f)
	var offset int64
	for len(items) < count {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("checkpoint has %d items but data has only %d: %w", count, len(items), err)
		}
		if !json.Valid(line) {
			return nil, fmt.Errorf("checkpoint item %d is invalid JSON", len(items))
		}
		items = append(items, bytes.TrimSpace(line))
		offset += int64(len(line))
	}
	if err := f.Truncate(offset); err != nil {
		return nil, err
	}
	return items, nil
}
