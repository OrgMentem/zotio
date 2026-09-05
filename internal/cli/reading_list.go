// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
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
			// route the reading queue through the shared
			// --data-source local parity path (ADR 0002 /
			// internal/store.QueryItems) instead of a live-only fetch, so the
			// queue works offline — including the ZOTIO_DEMO sandbox — via
			// `--data-source local`, and still falls back to local in auto mode
			// when the API is unreachable.
			params := map[string]string{
				"tag":       queueTag,
				"sort":      "dateAdded",
				"direction": "asc",
			}
			if flagLimit > 0 {
				params["limit"] = fmt.Sprintf("%d", flagLimit)
			}
			data, _, err := resolveRead(cmd.Context(), c, flags, "items", true, "/items", params, nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			items, err := decodeZoteroItems(data)
			if err != nil {
				return fmt.Errorf("parsing reading queue: %w", err)
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
			if !wantsHumanTable(cmd.OutOrStdout(), flags) {
				data, err := json.Marshal(result)
				if err != nil {
					return err
				}
				// The payload wraps a single `items` array, so --plain/--csv can
				// render it as records; printCommandJSON force-cleared them.
				return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
			}
			return printReadingList(cmd, result)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 20, "Maximum number of items to show")
	cmd.Flags().StringVar(&flagTag, "tag", defaultTag, "Override the reading queue tag")
	// keep the bare queue view while adding state transition subcommands.
	cmd.AddCommand(newReadingListAddCmd(flags), newReadingListStartCmd(flags), newReadingListDoneCmd(flags))

	return cmd
}

func readingQueueDefaultTag() string {
	if tag := strings.TrimSpace(os.Getenv("ZOTERO_QUEUE_TAG")); tag != "" {
		return tag
	}
	return "to-read"
}

// printReadingList renders the human table. Every cell is library text — the
// key, title, first author, year, dateAdded and itemType all come from the
// stored item, and so does the "oldest" date in the header line — so each one
// goes through advisoryCell (items_find.go).
//
// The title column used to be sanitized and truncated at 80. The 80 was
// inherited from the CLI this command was imported from and never justified,
// and sanitizeForTerminal alone left the hole this fold closes: it keeps tabs
// by design, so a tab in a title still opened a column in newTabWriter and
// shifted the author, year and date under the wrong headers. Fixing that
// means folding tabs here, and the fold and the cell budget live together in
// advisoryCell, so the column now matches printTable and every other ranked
// item list at 48 rather than carrying a third width of its own. A reading
// queue is a pick-what-to-read-next view, which is the same job `items list`
// does at 48, so nothing here needs the longer column.
func printReadingList(cmd *cobra.Command, result readingListResult) error {
	oldest := result.Oldest
	if oldest == "" {
		oldest = "n/a"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Reading queue: %d items (oldest: %s)\n", result.Count, advisoryCell(oldest))
	if len(result.Items) == 0 {
		return nil
	}
	tw := newTabWriter(cmd.OutOrStdout())
	fmt.Fprintln(tw, "Key\tTitle\tAuthor\tYear\tDate Added\tType")
	for _, item := range result.Items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			advisoryCell(item.Key),
			advisoryCell(item.Title),
			advisoryCell(item.Author),
			advisoryCell(item.Year),
			advisoryCell(item.DateAdded),
			advisoryCell(item.ItemType),
		)
	}
	return tw.Flush()
}
