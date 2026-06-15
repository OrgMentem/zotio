// Copyright 2026 enieuwy. Licensed under Apache-2.0. See LICENSE.
// PATCH: Add hand-written Zotero reading queue workflow missing from the generated CLI.

package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

type readingListItem struct {
	Key       string `json:"key"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	Year      string `json:"year"`
	DateAdded string `json:"date_added"`
	ItemType  string `json:"item_type"`
}

type readingListResult struct {
	QueueTag string            `json:"queue_tag"`
	Count    int               `json:"count"`
	Oldest   string            `json:"oldest"`
	Items    []readingListItem `json:"items"`
}

func newReadingListCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int
	defaultTag := readingQueueDefaultTag()
	flagTag := defaultTag

	cmd := &cobra.Command{
		Use:         "reading-list",
		Short:       "Show the Zotero reading queue",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			queueTag := strings.TrimSpace(flagTag)
			if queueTag == "" {
				queueTag = defaultTag
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			items, err := fetchZoteroItems(c, "/items", map[string]string{
				"tag":       queueTag,
				"sort":      "dateAdded",
				"direction": "asc",
			}, flagLimit)
			if err != nil {
				return classifyAPIError(err, flags)
			}

			queue := make([]readingListItem, 0, len(items))
			for _, item := range items {
				queue = append(queue, readingListItem{
					Key:       zoteroString(item, "key"),
					Title:     zoteroString(item, "title"),
					Author:    zoteroFirstAuthor(item),
					Year:      zoteroItemYear(item),
					DateAdded: zoteroString(item, "dateAdded"),
					ItemType:  zoteroString(item, "itemType"),
				})
			}
			sort.Slice(queue, func(i, j int) bool {
				return queue[i].DateAdded < queue[j].DateAdded
			})
			if flagLimit > 0 && len(queue) > flagLimit {
				queue = queue[:flagLimit]
			}
			result := readingListResult{
				QueueTag: queueTag,
				Count:    len(queue),
				Items:    queue,
			}
			if len(queue) > 0 {
				result.Oldest = queue[0].DateAdded
			}
			if flags.asJSON || !isTerminal(cmd.OutOrStdout()) {
				return printCommandJSON(cmd.OutOrStdout(), result, flags)
			}
			return printReadingList(cmd, result)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 20, "Maximum number of items to show")
	cmd.Flags().StringVar(&flagTag, "tag", defaultTag, "Override the reading queue tag")

	return cmd
}

func readingQueueDefaultTag() string {
	if tag := strings.TrimSpace(os.Getenv("ZOTERO_QUEUE_TAG")); tag != "" {
		return tag
	}
	return "to-read"
}

func printReadingList(cmd *cobra.Command, result readingListResult) error {
	oldest := result.Oldest
	if oldest == "" {
		oldest = "n/a"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Reading queue: %d items (oldest: %s)\n", result.Count, oldest)
	if len(result.Items) == 0 {
		return nil
	}
	tw := newTabWriter(cmd.OutOrStdout())
	fmt.Fprintln(tw, "Key\tTitle\tAuthor\tYear\tDate Added\tType")
	for _, item := range result.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			item.Key,
			truncate(item.Title, 80),
			item.Author,
			item.Year,
			item.DateAdded,
			item.ItemType,
		)
	}
	return tw.Flush()
}
