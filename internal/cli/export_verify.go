// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Verify export snapshot lockfiles against the current Zotero library.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"zotio/internal/client"

	"github.com/spf13/cobra"
)

type exportVerifySummary struct {
	Added     int `json:"added"`
	Removed   int `json:"removed"`
	Changed   int `json:"changed"`
	Touched   int `json:"touched"`
	Unchanged int `json:"unchanged"`
}

type exportVerifyItem struct {
	Class                string `json:"class"`
	Key                  string `json:"key"`
	Title                string `json:"title,omitempty"`
	LockVersion          int    `json:"lock_version,omitempty"`
	CurrentVersion       int    `json:"current_version,omitempty"`
	LockContentSHA256    string `json:"lock_content_sha256,omitempty"`
	CurrentContentSHA256 string `json:"current_content_sha256,omitempty"`
}

type exportVerifyReport struct {
	Lockfile             string              `json:"lockfile"`
	GeneratedAt          string              `json:"generated_at"`
	Scope                string              `json:"scope,omitempty"`
	AddedDetection       string              `json:"added_detection"`
	AddedDetectionReason string              `json:"added_detection_reason,omitempty"`
	Summary              exportVerifySummary `json:"summary"`
	Items                []exportVerifyItem  `json:"items"`
}

func newExportSnapshotVerifyCmd(flags *rootFlags) *cobra.Command {
	var failOnDrift bool

	cmd := &cobra.Command{
		Use:   "verify <lockfile.lock.json>",
		Short: "Verify a snapshot lockfile against the current library",
		Long: `Verify a snapshot lockfile against the current Zotero library.

The verifier re-resolves the lockfile scope when possible, recomputes the same
content hash as export snapshot, and separates semantic drift (added, removed,
changed) from Zotero version churn (touched). Touched items have a newer version
but identical normalized content and never fail --fail-on-drift.`,
		Example: `  zotio export snapshot verify backup.jsonl.lock.json
  zotio export snapshot verify backup.jsonl.lock.json --fail-on-drift
  zotio export snapshot verify backup.jsonl.lock.json --json`,
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			lockPath := args[0]
			lf, err := readExportLockfile(lockPath)
			if err != nil {
				return err
			}

			c, err := flags.newClient()
			if err != nil {
				return err
			}

			currentItems, addedDetection, addedReason, err := resolveExportVerifyCurrentItems(cmd.Context(), c, flags, lf)
			if err != nil {
				return err
			}

			report, err := buildExportVerifyReport(lockPath, lf, currentItems, addedDetection, addedReason)
			if err != nil {
				return err
			}
			if err := renderExportVerifyReport(cmd.OutOrStdout(), report, flags); err != nil {
				return err
			}

			if failOnDrift && report.Summary.Added+report.Summary.Removed+report.Summary.Changed > 0 {
				return gateErr(fmt.Errorf("export snapshot drift detected: added=%d removed=%d changed=%d", report.Summary.Added, report.Summary.Removed, report.Summary.Changed))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&failOnDrift, "fail-on-drift", false, "Exit 11 when added, removed, or changed items are detected (touched never fails)")
	return cmd
}

func resolveExportVerifyCurrentItems(ctx context.Context, c *client.Client, flags *rootFlags, lf exportLockfile) ([]json.RawMessage, string, string, error) {
	if strings.TrimSpace(lf.Scope) != "" {
		path, params, _, err := snapshotScopePath(lf.Scope)
		if err == nil {
			items, err := fetchSnapshotItemsForVerify(ctx, c, path, params, flags)
			return items, "resolved", "", err
		}
	}

	items := make([]json.RawMessage, 0, len(lf.Items))
	for _, lockItem := range lf.Items {
		if err := ctx.Err(); err != nil {
			return nil, "skipped", "", err
		}
		if strings.TrimSpace(lockItem.Key) == "" {
			continue
		}
		raw, err := c.Get("/items/"+url.PathEscape(lockItem.Key), nil)
		if err != nil {
			if apiStatus(err) == 404 {
				continue
			}
			return nil, "skipped", "", fmt.Errorf("fetching item %s: %w", lockItem.Key, classifyAPIError(err, flags))
		}
		items = append(items, raw)
	}
	return items, "skipped", fmt.Sprintf("lockfile scope %q cannot be re-resolved; verified only the recorded keys", lf.Scope), nil
}

func fetchSnapshotItemsForVerify(ctx context.Context, c *client.Client, path string, params map[string]string, flags *rootFlags) ([]json.RawMessage, error) {
	const pageSize = 100
	items := make([]json.RawMessage, 0)
	for start := 0; ; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pageParams := make(map[string]string, len(params)+2)
		for key, value := range params {
			pageParams[key] = value
		}
		pageParams["start"] = strconv.Itoa(start)
		pageParams["limit"] = strconv.Itoa(pageSize)

		data, err := c.Get(path, pageParams)
		if err != nil {
			return nil, fmt.Errorf("fetching %s page at start %d: %w", path, start, classifyAPIError(err, flags))
		}
		var page []json.RawMessage
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, fmt.Errorf("decoding %s page at start %d: %w", path, start, err)
		}
		items = append(items, page...)
		if len(page) < pageSize {
			return items, nil
		}
		start += len(page)
	}
}

func buildExportVerifyReport(lockPath string, lf exportLockfile, currentRaw []json.RawMessage, addedDetection, addedReason string) (exportVerifyReport, error) {
	currentLock, err := buildExportLockfile(lf.Scope, lf.Format, currentRaw)
	if err != nil {
		return exportVerifyReport{}, err
	}
	currentByKey := make(map[string]exportLockItem, len(currentLock.Items))
	for _, item := range currentLock.Items {
		currentByKey[item.Key] = item
	}
	lockByKey := make(map[string]exportLockItem, len(lf.Items))
	for _, item := range lf.Items {
		lockByKey[item.Key] = item
	}

	report := exportVerifyReport{
		Lockfile:             lockPath,
		GeneratedAt:          time.Now().UTC().Format(time.RFC3339),
		Scope:                lf.Scope,
		AddedDetection:       addedDetection,
		AddedDetectionReason: addedReason,
		Items:                make([]exportVerifyItem, 0, len(lf.Items)+len(currentLock.Items)),
	}

	for _, lockItem := range lf.Items {
		currentItem, ok := currentByKey[lockItem.Key]
		if !ok {
			report.Summary.Removed++
			report.Items = append(report.Items, exportVerifyItem{
				Class:             "removed",
				Key:               lockItem.Key,
				Title:             lockItem.Title,
				LockVersion:       lockItem.Version,
				LockContentSHA256: lockItem.ContentSHA256,
			})
			continue
		}

		item := exportVerifyItem{
			Key:                  lockItem.Key,
			Title:                currentTitle(currentItem, lockItem),
			LockVersion:          lockItem.Version,
			CurrentVersion:       currentItem.Version,
			LockContentSHA256:    lockItem.ContentSHA256,
			CurrentContentSHA256: currentItem.ContentSHA256,
		}
		switch {
		case lockItem.ContentSHA256 != currentItem.ContentSHA256:
			item.Class = "changed"
			report.Summary.Changed++
		case lockItem.Version != currentItem.Version:
			item.Class = "touched"
			report.Summary.Touched++
		default:
			item.Class = "unchanged"
			report.Summary.Unchanged++
		}
		report.Items = append(report.Items, item)
	}

	if addedDetection == "resolved" {
		for _, currentItem := range currentLock.Items {
			if _, ok := lockByKey[currentItem.Key]; ok {
				continue
			}
			report.Summary.Added++
			report.Items = append(report.Items, exportVerifyItem{
				Class:                "added",
				Key:                  currentItem.Key,
				Title:                currentItem.Title,
				CurrentVersion:       currentItem.Version,
				CurrentContentSHA256: currentItem.ContentSHA256,
			})
		}
	}

	sortExportVerifyItems(report.Items)
	return report, nil
}

func currentTitle(currentItem, lockItem exportLockItem) string {
	if currentItem.Title != "" {
		return currentItem.Title
	}
	return lockItem.Title
}

func sortExportVerifyItems(items []exportVerifyItem) {
	classRank := map[string]int{
		"changed":   0,
		"removed":   1,
		"added":     2,
		"touched":   3,
		"unchanged": 4,
	}
	sort.Slice(items, func(i, j int) bool {
		left, right := classRank[items[i].Class], classRank[items[j].Class]
		if left != right {
			return left < right
		}
		return items[i].Key < items[j].Key
	})
}

func renderExportVerifyReport(w io.Writer, report exportVerifyReport, flags *rootFlags) error {
	if flags.asJSON || flags.csv || flags.compact || flags.selectFields != "" {
		return printJSONFiltered(w, report, flags)
	}
	if flags.quiet {
		return nil
	}
	return renderExportVerifyHuman(w, report)
}

func renderExportVerifyHuman(w io.Writer, report exportVerifyReport) error {
	if _, err := fmt.Fprintf(w, "Lockfile: %s\n", report.Lockfile); err != nil {
		return err
	}
	if report.Scope != "" {
		if _, err := fmt.Fprintf(w, "Scope: %s\n", report.Scope); err != nil {
			return err
		}
	}
	if report.AddedDetection == "skipped" && report.AddedDetectionReason != "" {
		if _, err := fmt.Fprintf(w, "Added detection: skipped (%s)\n", report.AddedDetectionReason); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "Summary: added=%d removed=%d changed=%d touched=%d unchanged=%d\n", report.Summary.Added, report.Summary.Removed, report.Summary.Changed, report.Summary.Touched, report.Summary.Unchanged); err != nil {
		return err
	}

	groups := []struct {
		Class string
		Label string
		Count int
	}{
		{Class: "changed", Label: "Changed", Count: report.Summary.Changed},
		{Class: "removed", Label: "Removed", Count: report.Summary.Removed},
		{Class: "added", Label: "Added", Count: report.Summary.Added},
		{Class: "touched", Label: "Touched", Count: report.Summary.Touched},
		{Class: "unchanged", Label: "Unchanged", Count: report.Summary.Unchanged},
	}
	for _, group := range groups {
		if _, err := fmt.Fprintf(w, "\n%s (%d)\n", group.Label, group.Count); err != nil {
			return err
		}
		for _, item := range report.Items {
			if item.Class != group.Class {
				continue
			}
			title := item.Title
			if title == "" {
				title = "(untitled)"
			}
			if _, err := fmt.Fprintf(w, "  - %s\t%s\n", item.Key, title); err != nil {
				return err
			}
		}
	}
	return nil
}
