// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// Resolve an item zotio just created, when the write path could not report its key.

package cli

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"zotio/internal/client"
)

// How far back through recently added top-level items to look. Generous enough
// for a batch, small enough that an unrelated same-title item cannot collide.
const recentItemLookupLimit = 50

// Slack subtracted from the pre-write wall clock before comparing against
// Zotero's dateAdded. Both processes share this machine's clock, so this only
// absorbs the API's second-level granularity.
const recentItemClockSkew = 2 * time.Minute

// findRecentlyAddedItemKey resolves the key of an item created after floor that
// matches title and itemType, returning "" unless the match is unambiguous.
//
// Used where a write succeeded but its own response could not name the key:
// Zotero's connector reports only a title for a recognised PDF, and can return
// HTTP 500 having already created the item. Matching is anchored to a wall-clock
// floor captured before the write, because a title alone can land on an older
// item that merely shares it — the connector writes into whatever library the
// desktop pane targets, which need not be the library zotio reads.
//
// Returns the key and how many recently added items matched the title, so a
// caller can distinguish "not there" from "ambiguous" and refuse to guess.
func findRecentlyAddedItemKey(c *client.Client, title, itemType string, floor time.Time) (key string, matched int, err error) {
	if strings.TrimSpace(title) == "" || itemType == "" {
		return "", 0, nil
	}
	// The item was created seconds ago; a cached list would not contain it.
	c.NoCache = true
	data, err := c.Get("/items/top", map[string]string{
		"sort":      "dateAdded",
		"direction": "desc",
		"limit":     strconv.Itoa(recentItemLookupLimit),
	})
	if err != nil {
		return "", 0, err
	}
	var top []json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return "", 0, err
	}

	var found string
	for _, entry := range top {
		if !strings.EqualFold(strings.TrimSpace(jsonStringField(entry, "title")), strings.TrimSpace(title)) {
			continue
		}
		if jsonStringField(entry, "itemType") != itemType {
			continue
		}
		if !addedAfter(entry, floor) {
			continue
		}
		matched++
		found = jsonStringField(entry, "key")
	}
	// Reporting a guessed key is worse than reporting none.
	if matched != 1 {
		return "", matched, nil
	}
	return found, matched, nil
}
