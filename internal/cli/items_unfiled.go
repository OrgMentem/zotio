// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

func newItemsUnfiledCmd(flags *rootFlags) *cobra.Command {
	var flagLimit int
	var flagType string
	var flagSuggest bool
	var flagSuggestLimit int
	var flagSuggestMinScore float64

	cmd := &cobra.Command{
		Use:   "unfiled",
		Short: "List top-level items not assigned to any collection",
		Long: "List top-level items without collections from the local mirror. " +
			"With --suggest, rank existing collections using shared tags, creators, and venue against filed items. " +
			"Each collection scores the mean of its three best member matches (or all members when fewer than three). " +
			"Each shared tag, creator, or venue splits its vote across the collections its filed items occupy, so a " +
			"status tag such as \"/unread\" or a broad journal that spans many collections is weak evidence, and " +
			"suggestions scoring below --suggest-min-score are dropped. Suggestions are read-only and never move items.",
		Example: "  zotio items unfiled --suggest --json\n  zotio items unfiled --suggest --suggest-limit 5 --type book\n" +
			"  # File every item whose best suggestion is collection ABCD1234 (preview; add --yes to apply)\n" +
			"  zotio items unfiled --suggest --json | jq '[.results[] | select(.suggestions[0].collection == \"ABCD1234\")]' | zotio items move --keys-from - --to ABCD1234",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flagSuggestLimit < 0 {
				return usageErr(fmt.Errorf("--suggest-limit must be >= 0"))
			}
			if math.IsNaN(flagSuggestMinScore) || math.IsInf(flagSuggestMinScore, 0) || flagSuggestMinScore < 0 {
				return usageErr(fmt.Errorf("--suggest-min-score must be a finite value >= 0"))
			}
			for _, name := range []string{"suggest-limit", "suggest-min-score"} {
				if cmd.Flags().Changed(name) && !flagSuggest {
					return usageErr(fmt.Errorf("--%s requires --suggest", name))
				}
			}
			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening database: %w", err)
			}
			if rawDB == nil {
				if flagSuggest {
					return preconditionErr(fmt.Errorf("run 'zotio sync' first to enable collection suggestions"))
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			rows, err := queryUnfiledItems(db, flagType, flagLimit)
			if err != nil {
				return fmt.Errorf("querying unfiled items: %w", err)
			}
			if flagSuggest {
				if err := addUnfiledSuggestions(cmd.Context(), db, rows, flagSuggestLimit, flagSuggestMinScore); err != nil {
					return fmt.Errorf("suggesting collections: %w", err)
				}
			}
			data, err := json.Marshal(rows)
			if err != nil {
				return err
			}
			// Local-mirror read: same envelope pipeline as every other read
			// command, so `.results` stays a JSON array (this used to print a
			// bare top-level array).
			prov := localProvenance(rawDB, "items", "local_only")
			printProvenance(cmd, len(rows), prov)
			if wantsJSONEnvelope(cmd.OutOrStdout(), flags) {
				filtered := json.RawMessage(data)
				if flags.selectFields != "" {
					filtered = filterFields(filtered, flags.selectFields)
				} else if flags.compact {
					filtered = compactFields(filtered)
				}
				wrapped, wrapErr := wrapWithProvenance(filtered, prov)
				if wrapErr != nil {
					return wrapErr
				}
				return printOutput(cmd.OutOrStdout(), wrapped, true)
			}
			return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
		},
	}
	cmd.Flags().IntVar(&flagLimit, "limit", 0, "Maximum number of items to return (0 = no limit)")
	cmd.Flags().StringVar(&flagType, "type", "", "Filter by Zotero item type")
	cmd.Flags().BoolVar(&flagSuggest, "suggest", false, "Suggest collections from local tags, creators, and venue (read-only)")
	cmd.Flags().IntVar(&flagSuggestLimit, "suggest-limit", 3, "Maximum collection suggestions per item (requires --suggest)")
	cmd.Flags().Float64Var(&flagSuggestMinScore, "suggest-min-score", unfiledSuggestMinScore, "Drop collection suggestions scoring below this (requires --suggest)")

	return cmd
}

func queryUnfiledItems(db localQueryStore, itemType string, limit int) ([]map[string]any, error) {
	query := `
SELECT
	i.id AS key,
	json_extract(i.data, '$.data.title') AS title,
	json_extract(i.data, '$.data.itemType') AS item_type,
	json_extract(i.data, '$.data.dateAdded') AS date_added
FROM resources i
WHERE i.resource_type = 'items'
	AND json_extract(i.data, '$.data.itemType') NOT IN ('attachment', 'note', 'annotation')
	AND (
		json_extract(i.data, '$.data.collections') IS NULL
		OR json_array_length(json_extract(i.data, '$.data.collections')) = 0
	)`
	args := make([]any, 0, 2)
	if itemType != "" {
		query += `
	AND json_extract(i.data, '$.data.itemType') = ?`
		args = append(args, itemType)
	}
	query += `
ORDER BY date_added DESC`
	if limit > 0 {
		query += `
LIMIT ?`
		args = append(args, limit)
	}
	return db.QueryRaw(query, args...)
}

type unfiledSuggestion struct {
	Collection string   `json:"collection"`
	Name       string   `json:"name"`
	Score      float64  `json:"score"`
	Reasons    []string `json:"reasons"`
}

type unfiledMemberMatch struct {
	Record itemSimilarRecord
	Score  float64
	Tags   []string
	People []string
	Venue  float64
}

// keepBestUnfiledMembers retains at most three matches in score/key order.
func keepBestUnfiledMembers(members []unfiledMemberMatch, match unfiledMemberMatch) []unfiledMemberMatch {
	at := 0
	for at < len(members) &&
		(members[at].Score > match.Score ||
			(members[at].Score == match.Score && members[at].Record.Key < match.Record.Key)) {
		at++
	}
	if at == 3 {
		return members
	}
	if len(members) < 3 {
		members = append(members, unfiledMemberMatch{})
	}
	copy(members[at+1:], members[at:len(members)-1])
	members[at] = match
	return members
}

// unfiledSuggestMinScore is the default evidence floor. Every signal's vote is
// split across the k collections it occupies: one exact tag match scores
// 0.56/k and one shared sole creator or venue 0.22/k. 0.05 therefore keeps a
// tag confined to at most eleven collections and a creator or venue confined
// to four, and drops a lone status tag such as "/unread" across eighteen
// (0.03), which was otherwise the only evidence behind most of a real inbox's
// suggestions.
const unfiledSuggestMinScore = 0.05

func addUnfiledSuggestions(ctx context.Context, db localQueryStore, rows []map[string]any, limit int, minScore float64) error {
	if len(rows) == 0 {
		return nil
	}
	// The similarity query loads each regular item once, excluding the trash mirror.
	records, err := queryItemSimilarCandidates(ctx, db)
	if err != nil {
		return err
	}
	collections, err := db.QueryRaw(`
SELECT r.id AS key, COALESCE(json_extract(r.data, '$.data.name'), json_extract(r.data, '$.name'), '') AS name
FROM resources r
WHERE r.resource_type = 'collections'
	AND COALESCE(json_extract(r.data, '$.data.deleted'), json_extract(r.data, '$.deleted'), 0) = 0
	AND NOT EXISTS (
		SELECT 1 FROM pending_writes p
		WHERE p.resource_type = 'collections' AND p.id = r.id AND p.deleted = 1
	)
ORDER BY r.id`)
	if err != nil {
		return fmt.Errorf("querying collections: %w", err)
	}
	type collectionInfo struct{ key, name string }
	names := make(map[string]collectionInfo, len(collections))
	for _, collection := range collections {
		key := sqlStringValue(collection["key"])
		names[normalizeItemSimilarToken(key)] = collectionInfo{key, sqlStringValue(collection["name"])}
	}
	byKey := make(map[string]itemSimilarRecord, len(records))
	filed := make([]itemSimilarRecord, 0, len(records))
	memberCounts := make(map[string]int, len(names))
	// Each signal token maps to the collections its filed items occupy; a
	// match's vote is split across them (see unfiledSplitJaccard).
	tagSpread, creatorSpread, venueSpread := unfiledSpread{}, unfiledSpread{}, unfiledSpread{}
	for _, rec := range records {
		byKey[rec.Key] = rec
		if len(rec.Collections) != 0 {
			filed = append(filed, rec)
			for key := range rec.Collections {
				memberCounts[key]++
				if _, exists := names[key]; !exists {
					continue
				}
				for tag := range rec.Tags {
					tagSpread.add(tag, key)
				}
				for creator := range rec.Creators {
					creatorSpread.add(creator, key)
				}
				if rec.Venue != "" {
					venueSpread.add(rec.Venue, key)
				}
			}
		}
	}
	for _, row := range rows {
		suggestions := make([]unfiledSuggestion, 0)
		source, found := byKey[sqlStringValue(row["key"])]
		if found && limit > 0 {
			votes := make(map[string][]unfiledMemberMatch)
			for _, rec := range filed {
				tags, sharedTags := unfiledSplitJaccard(source.Tags, rec.Tags, tagSpread)
				people, sharedPeople := unfiledSplitJaccard(source.Creators, rec.Creators, creatorSpread)
				venue := itemSimilarVenueScore(source, rec) / float64(max(1, len(venueSpread[rec.Venue])))
				score := (tags*itemSimilarTagWeight + people*itemSimilarCreatorWeight +
					venue*itemSimilarVenueWeight) / (itemSimilarTagWeight + itemSimilarCreatorWeight + itemSimilarVenueWeight)
				if score <= 0 {
					continue
				}
				match := unfiledMemberMatch{rec, score, sharedTags, sharedPeople, venue}
				for key := range rec.Collections {
					if _, exists := names[key]; exists {
						votes[key] = keepBestUnfiledMembers(votes[key], match)
					}
				}
			}
			for key, members := range votes {
				// The fixed-size list is already in score/key order.
				// Mean of the top three members, including zero-score members when
				// available: one accidental match in a large collection cannot win
				// by itself, while a small collection uses only its actual members.
				totalMembers := memberCounts[key]
				count := min(3, totalMembers)
				sum := 0.0
				reasons := make([]string, 0, 4)
				for i, member := range members[:min(count, len(members))] {
					sum += member.Score
					reasons = append(reasons, fmt.Sprintf("matched item %s (%s)", member.Record.Key, member.Record.Title))
					if i == 0 {
						if len(member.Tags) > 0 {
							reasons = append(reasons, itemSimilarSharedReason("tag", "tags", member.Tags[:min(3, len(member.Tags))], source.Tags, member.Record.Tags))
						}
						if len(member.People) > 0 {
							reasons = append(reasons, itemSimilarSharedReason("creator", "creators", member.People[:min(3, len(member.People))], source.Creators, member.Record.Creators))
						}
						if member.Venue > 0 {
							reasons = append(reasons, itemSimilarVenueReason(member.Venue, source, member.Record))
						}
					}
				}
				// The floor applies to the unrounded mean; rounding is display only.
				mean := sum / float64(count)
				score := math.Round(mean*100) / 100
				if mean > 0 && mean >= minScore {
					suggestions = append(suggestions, unfiledSuggestion{names[key].key, names[key].name, score, reasons})
				}
			}
			sort.Slice(suggestions, func(i, j int) bool {
				if suggestions[i].Score == suggestions[j].Score {
					return strings.Compare(suggestions[i].Collection, suggestions[j].Collection) < 0
				}
				return suggestions[i].Score > suggestions[j].Score
			})
			if len(suggestions) > limit {
				suggestions = suggestions[:limit]
			}
		}
		row["suggestions"] = suggestions
		if len(suggestions) > 0 {
			row["recommended_action"] = fmt.Sprintf("zotio items move %s --to %s", sqlStringValue(row["key"]), suggestions[0].Collection)
		}
	}
	return nil
}

type unfiledSpread map[string]map[string]bool

func (s unfiledSpread) add(token, collection string) {
	if s[token] == nil {
		s[token] = make(map[string]bool)
	}
	s[token][collection] = true
}

// unfiledSplitJaccard is Jaccard with each shared token's vote split across
// the collections its filed items occupy. Filing asks which collection the
// evidence points at, so a status tag spread over many collections (a reader's
// "/unread" on 100 items in 18 collections, or a broad journal, measured
// 2026-09-23) is near-zero evidence, while a token confined to one collection
// counts in full. Plain Jaccard scored two items sharing only such a tag as
// identical and filed most of a real library's inbox into whichever small
// collection it touched.
func unfiledSplitJaccard(source, candidate map[string]string, spread unfiledSpread) (float64, []string) {
	if len(source) == 0 || len(candidate) == 0 {
		return 0, nil
	}
	shared := make([]string, 0)
	evidence := 0.0
	for key, display := range source {
		if _, ok := candidate[key]; !ok {
			continue
		}
		shared = append(shared, display)
		evidence += 1 / float64(max(1, len(spread[key])))
	}
	if len(shared) == 0 {
		return 0, nil
	}
	sort.Strings(shared)
	return evidence / float64(len(source)+len(candidate)-len(shared)), shared
}
