// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/spf13/cobra"
)

func newItemsVenuesCmd(flags *rootFlags) *cobra.Command {
	var flagTop int
	var flagType string

	cmd := &cobra.Command{
		Use:   "venues",
		Short: "Count synced items by publication venue",
		Example: `  zotio items venues
  zotio items venues --top 10
  zotio items venues --type journalArticle --json`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{rawDB}

			rows, err := queryItemVenues(db, flagType, flagTop)
			if err != nil {
				return fmt.Errorf("querying venues: %w", err)
			}
			data, err := json.Marshal(rows)
			if err != nil {
				return err
			}
			return printOutputWithFlags(cmd.OutOrStdout(), json.RawMessage(data), flags)
		},
	}
	cmd.Flags().IntVar(&flagTop, "top", 20, "Maximum number of venues to return")
	cmd.Flags().StringVar(&flagType, "type", "", "Filter by Zotero itemType")

	return cmd
}

func queryItemVenues(db localQueryStore, itemType string, top int) ([]map[string]any, error) {
	// Fetch per-item venue + raw date + type, then aggregate in Go.
	// Venue: first non-empty of publicationTitle/bookTitle/conferenceName/publisher.
	// Raw date: prefer normalized meta.parsedDate, fall back to freeform data.date.
	// Year extraction is delegated to yearFromDate (dateYearPattern `\b(1[5-9]\d{2}|20\d{2})\b`)
	// so that freeform values like "April 2023" yield "2023" and undatable values
	// like "n.d." are excluded rather than producing garbage SUBSTR("April 2023",1,4)="Apri".
	// Item type is aggregated deterministically as the modal (most frequent) type per
	// venue, tie-broken lexicographically. This keeps the documented single-row-per-venue
	// contract (GROUP BY venue) while eliminating SQLite's nondeterministic bare-column
	// behavior for the same venue appearing under multiple itemTypes. Alternative
	// GROUP BY (venue, item_type) would split venues and break consumers; lexical
	// MIN alone would ignore frequency. Modal reflects the dominant type actually
	// used at that venue.
	query := `
SELECT
	COALESCE(
		NULLIF(TRIM(json_extract(data,'$.data.publicationTitle')),''),
		NULLIF(TRIM(json_extract(data,'$.data.bookTitle')),''),
		NULLIF(TRIM(json_extract(data,'$.data.conferenceName')),''),
		NULLIF(TRIM(json_extract(data,'$.data.publisher')),'')
	) AS venue,
	COALESCE(NULLIF(json_extract(data,'$.meta.parsedDate'),''), json_extract(data,'$.data.date'),'') AS raw_date,
	json_extract(data,'$.data.itemType') AS item_type
FROM resources
WHERE resource_type='items'
	AND json_extract(data,'$.data.itemType') NOT IN ('attachment','note','annotation')`
	args := make([]any, 0, 2)
	if itemType != "" {
		query += `
	AND json_extract(data,'$.data.itemType') = ?`
		args = append(args, itemType)
	}
	// No GROUP BY here; aggregation happens in Go for correct year handling.
	rows, err := db.QueryRaw(query, args...)
	if err != nil {
		return nil, err
	}

	type venueAgg struct {
		count      int
		minYear    string
		maxYear    string
		typeCounts map[string]int
		hasYear    bool
	}
	aggByVenue := make(map[string]*venueAgg)
	for _, r := range rows {
		venue := sqlStringValue(r["venue"])
		if venue == "" {
			continue
		}
		rawDate := sqlStringValue(r["raw_date"])
		itemTp := sqlStringValue(r["item_type"])
		agg, ok := aggByVenue[venue]
		if !ok {
			agg = &venueAgg{typeCounts: make(map[string]int)}
			aggByVenue[venue] = agg
		}
		agg.count++
		if t := itemTp; t != "" {
			agg.typeCounts[t]++
		}
		yr := yearFromDate(rawDate)
		if yr == "" {
			continue
		}
		if !agg.hasYear {
			agg.minYear = yr
			agg.maxYear = yr
			agg.hasYear = true
		} else {
			if yr < agg.minYear {
				agg.minYear = yr
			}
			if yr > agg.maxYear {
				agg.maxYear = yr
			}
		}
	}

	out := make([]map[string]any, 0, len(aggByVenue))
	for venue, agg := range aggByVenue {
		// Deterministic item_type: modal, lexicographically smallest on tie.
		chosen := ""
		best := -1
		for tp, cnt := range agg.typeCounts {
			if cnt > best || (cnt == best && tp < chosen) {
				best = cnt
				chosen = tp
			}
		}
		var minYear any
		var maxYear any
		if agg.hasYear {
			minYear = agg.minYear
			maxYear = agg.maxYear
		} else {
			minYear = nil
			maxYear = nil
		}
		out = append(out, map[string]any{
			"venue":     venue,
			"item_type": chosen,
			"min_year":  minYear,
			"max_year":  maxYear,
			"count":     agg.count,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		ci := sqlIntValue(out[i]["count"])
		cj := sqlIntValue(out[j]["count"])
		if ci != cj {
			return ci > cj
		}
		vi := sqlStringValue(out[i]["venue"])
		vj := sqlStringValue(out[j]["venue"])
		return vi < vj
	})
	if top > 0 && len(out) > top {
		out = out[:top]
	}
	return out, nil
}
