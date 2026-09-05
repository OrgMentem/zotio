// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"zotio/internal/store"

	"github.com/spf13/cobra"
)

type localQueryStore struct {
	*store.Store
}

func (s localQueryStore) QueryRaw(query string, args ...any) ([]map[string]any, error) {
	rows, err := s.Query(query, args...)
	if err != nil {
		return nil, err
	}
	return rawSQLRows(rows)
}

// QueryRawContext runs a local SQL query using the caller's cancellation
// boundary. MCP resource handlers use it so an abandoned request cannot keep a
// SQLite query running until the busy timeout expires.
func (s localQueryStore) QueryRawContext(ctx context.Context, query string, args ...any) ([]map[string]any, error) {
	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return rawSQLRows(rows)
}

func rawSQLRows(rows *sql.Rows) ([]map[string]any, error) {
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(columns))
		for i, col := range columns {
			row[col] = normalizeSQLValue(values[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func normalizeSQLValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(x)
	case sql.NullString:
		if x.Valid {
			return x.String
		}
		return nil
	default:
		return x
	}
}

func newItemsDuplicatesCmd(flags *rootFlags) *cobra.Command {
	var flagBy string

	cmd := &cobra.Command{
		Use:         "duplicates",
		Short:       "Find likely duplicate items in the local store",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first to enable duplicate detection.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{Store: rawDB}

			var results []map[string]any
			switch flagBy {
			case "doi":
				results, err = queryDuplicateDOIs(db)
			case "title":
				results, err = queryDuplicateTitles(db)
			case "all":
				results, err = queryDuplicateDOIs(db)
				if err == nil {
					var titleRows []map[string]any
					titleRows, err = queryDuplicateTitles(db)
					results = append(results, titleRows...)
				}
			default:
				return fmt.Errorf("invalid --by value %q: must be doi, title, or all", flagBy)
			}
			if err != nil {
				return fmt.Errorf("querying duplicates: %w", err)
			}
			groups := normalizeDuplicateRows(results)
			// The near pass is advisory and separate on purpose: `groups` is
			// what four consumers already treat as "these records ARE the
			// same", one of which merges them (items_duplicates_resolve.go).
			// See queryNearDuplicateTitles.
			var nearGroups []nearDuplicateTitleGroup
			nearTotal := 0
			titleLookup := titleLookupNotRequested
			if flagBy == "title" || flagBy == "all" {
				var nearErr error
				nearGroups, nearTotal, nearErr = queryNearDuplicateTitles(cmd.Context(), db)
				switch {
				case errors.Is(nearErr, context.Canceled), errors.Is(nearErr, context.DeadlineExceeded):
					// Not advisory: the pass never finished. Degrading this to
					// a warning would print "nothing is close" for a scan the
					// user interrupted.
					return nearErr
				case nearErr != nil:
					// Advisory data, so the exact report still stands. Name the
					// failure rather than let it read as a clean library.
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: near-duplicate title pass unavailable: %v\n", nearErr)
					titleLookup = titleLookupFailed
				case len(nearGroups) == 0:
					titleLookup = titleLookupNoNear
				default:
					titleLookup = titleLookupNear
				}
			}
			if flags.asJSON {
				payload := map[string]any{
					"groups":   groups,
					"findings": duplicateItemFindings(groups),
					// meta.title_lookup, the same key and the same words
					// `items find` publishes: how the title question ended.
					// A caller branches on data instead of on an absent key
					// that would mean "clean", "not run" and "broken" at once.
					"meta": map[string]any{"title_lookup": titleLookup},
				}
				if len(nearGroups) > 0 {
					// Sibling key, never inside `groups`: these records are
					// candidates for review, not duplicates the command found.
					// Named for what it returns — groups, where `items find`
					// returns near_title_matches.
					payload["near_title_groups"] = nearGroups
					if nearTotal > len(nearGroups) {
						// Same number the prose block prints, under the same
						// name `items find` uses. Without it a caller reads
						// "not in these 25" as "not in the library".
						payload["meta"].(map[string]any)["near_title_total"] = nearTotal
					}
				}
				data, err := json.Marshal(payload)
				if err != nil {
					return err
				}
				return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
			}
			// A human reading a bare `[]` cannot tell a clean library from a
			// broken command, which is the same silence the `matched: none`
			// line removed from `items find`. Machine formats keep the array.
			if wantsHumanTable(cmd.OutOrStdout(), flags) && len(groups) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no exact duplicate groups (%s)\n", flagBy)
			} else {
				data, err := json.Marshal(groups)
				if err != nil {
					return err
				}
				// Exact groups keep the shape every existing caller parses;
				// the near rows are added beside them, never merged in.
				if err := printOutputWithFlags(cmd.OutOrStdout(), data, flags); err != nil {
					return err
				}
			}
			switch {
			case len(nearGroups) == 0:
				return nil
			case wantsHumanTable(cmd.OutOrStdout(), flags):
				return printNearDuplicateTitleGroups(cmd, nearGroups, nearTotal)
			case flags.quiet:
				// --quiet asked for less output and the exit code is the whole
				// answer there (helpers.go printOutputWithFlags).
				return nil
			default:
				// --plain and --csv have no column for a score, and a piped
				// run without --json gets the bare exact-group array, so
				// nothing on either stream would carry these rows.
				fmt.Fprintf(cmd.ErrOrStderr(), "note: %s found and not shown in this format; re-run with --json to see them.\n",
					pluralCount(len(nearGroups), "near-duplicate title group", "near-duplicate title groups"))
				return nil
			}
		},
	}
	cmd.Flags().StringVar(&flagBy, "by", "all", "Duplicate detector to run (doi, title, all); title and all also report titles that are close but not equal, scored and separately (near_title_groups in JSON, with meta.title_lookup naming the state), never as duplicate groups and never as something 'duplicates resolve' will merge")
	// Keep the bare duplicate report intact while adding the write-safe resolver subcommand.
	cmd.AddCommand(newItemsDuplicatesResolveCmd(flags))

	return cmd
}

func queryDuplicateDOIs(db localQueryStore, scopes ...map[string]struct{}) ([]map[string]any, error) {
	clause, args, err := duplicateQueryScopeClause(scopes)
	if err != nil {
		return nil, err
	}
	return db.QueryRaw(fmt.Sprintf(`
SELECT
	'doi' AS "group",
	value,
	COUNT(*) AS count,
	json_group_array(id) AS keys
FROM (
	SELECT id, LOWER(TRIM(json_extract(data, '$.data.DOI'))) AS value
	FROM resources
	WHERE resource_type = 'items'
		AND COALESCE(TRIM(json_extract(data, '$.data.DOI')), '') != ''%s
)
GROUP BY value
HAVING COUNT(*) > 1
ORDER BY count DESC, value`, clause), args...)
}

// queryDuplicateTitles groups citeable items sharing a normalized title.
// Exclude attachment/annotation/note rows
// so that attachments named "PDF" / "Snapshot" / "Full Text PDF" don't dominate
// the report as false bibliographic duplicates (and so `items duplicates resolve
// --title` never tries to merge them).
//
// This is EQUALITY, and it stays equality: LOWER(TRIM(...)) here is the single
// owner of exact title grouping. Its rows are merged automatically
// (items_duplicates_resolve.go) and counted as removed records in a published
// PRISMA flow (library_prisma.go), so widening what lands in a group changes
// what gets trashed and what gets published. A Go-side mirror of this fold
// existed and had drifted out of use; the fold that answers "is this title
// close?" is a separate, advisory pass — see queryNearDuplicateTitles.
func queryDuplicateTitles(db localQueryStore, scopes ...map[string]struct{}) ([]map[string]any, error) {
	clause, args, err := duplicateQueryScopeClause(scopes)
	if err != nil {
		return nil, err
	}
	return db.QueryRaw(fmt.Sprintf(`
SELECT
	'title' AS "group",
	MIN(title) AS value,
	COUNT(*) AS count,
	json_group_array(id) AS keys
FROM (
	SELECT
		id,
		TRIM(json_extract(data, '$.data.title')) AS title,
		LOWER(TRIM(json_extract(data, '$.data.title'))) AS normalized_title,
		COALESCE(json_extract(data, '$.data.itemType'), '') AS item_type
	FROM resources
	WHERE resource_type = 'items'
		AND COALESCE(TRIM(json_extract(data, '$.data.title')), '') != ''%s
		AND COALESCE(item_type, '') NOT IN ('attachment', 'annotation', 'note')
)
GROUP BY normalized_title, item_type
HAVING COUNT(*) > 1
ORDER BY count DESC, value`, clause), args...)
}

// duplicateQueryScopeClause keeps legacy duplicate calls unscoped while letting
// reports restrict the shared SQL to one resolved item cohort.
func duplicateQueryScopeClause(scopes []map[string]struct{}) (string, []any, error) {
	if len(scopes) == 0 || scopes[0] == nil {
		return "", nil, nil
	}
	keys := make([]string, 0, len(scopes[0]))
	for key := range scopes[0] {
		keys = append(keys, key)
	}
	return duplicateKeyScopeClause(keys)
}

func duplicateKeyScopeClause(keys []string) (string, []any, error) {
	if len(keys) == 0 {
		return "\n\t\tAND 1=0", nil, nil
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		return "", nil, fmt.Errorf("encoding scoped item keys: %w", err)
	}
	// json_each carries an arbitrarily large key set through one SQLite bind.
	return "\n\t\tAND id IN (SELECT value FROM json_each(?))", []any{string(encoded)}, nil
}

func normalizeDuplicateRows(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		normalized := make(map[string]any, len(row))
		for k, v := range row {
			normalized[k] = v
		}
		if rawKeys, ok := normalized["keys"].(string); ok {
			var keys []string
			if json.Unmarshal([]byte(rawKeys), &keys) == nil {
				normalized["keys"] = keys
			}
		}
		out = append(out, normalized)
	}
	return out
}

func duplicateItemFindings(groups []map[string]any) []Finding {
	findings := make([]Finding, 0)
	for _, group := range groups {
		keys, ok := group["keys"].([]string)
		if !ok {
			continue
		}
		evidence := map[string]any{
			"group": sqlStringValue(group["group"]),
			"value": sqlStringValue(group["value"]),
			"count": sqlIntValue(group["count"]),
			"keys":  keys,
		}
		for _, key := range keys {
			findings = append(findings, Finding{
				Kind:              "duplicate_item",
				Severity:          sevHigh,
				ItemKey:           key,
				Evidence:          evidence,
				Source:            FindingSource{Kind: "local"},
				Autofixable:       true,
				RecommendedAction: &RecommendedAction{Command: "zotio items duplicates resolve"},
			})
		}
	}
	return findings
}

// Near-duplicate title detection: the advisory half of `items duplicates --by
// title`.
//
// The exact pass (queryDuplicateTitles) requires two titles to be equal after
// LOWER(TRIM(...)). That is the right contract for the consumers that read a
// group as identity — items_duplicates_resolve.go MERGES one, library_prisma.go
// publishes its size as a removal count — and the wrong contract for this
// command's own purpose: one paper imported twice, once from a reference list
// with a trailing full stop and once from a publisher feed with a curly
// apostrophe, is two groups, so the pair is never reported and the library
// reads as clean.
//
// Nothing below ever adds a row to `groups`. It answers a different question in
// its own rows, carrying a score and a review instruction.
const (
	// duplicateGroupKindTitleNear labels a near row, so a consumer reading the
	// JSON tells it from an exact 'title' or 'doi' group by the row itself and
	// not only by the key it arrived under.
	duplicateGroupKindTitleNear = "title_near"

	// titleLookupNotRequested extends the titleLookup* family in items_find.go
	// with the one state that command cannot have. There, a title lookup
	// either ran or no --title was given; here `--by doi` selects a detector
	// that never asks the title question at all, and a caller reading a stored
	// report has to be able to tell that silence from a clean library. It is
	// actionable: the remedy is `--by title`. The other three states are the
	// ones `items find` already publishes (titleLookupNear, titleLookupNoNear,
	// titleLookupFailed), under the same meta key, so an agent that learned
	// the vocabulary on one command can read the other.
	titleLookupNotRequested = "not_requested"

	// nearDuplicateTitleGroupLimit caps the reported rows by RANK, not by
	// score. A user-facing distance threshold would ask the person who typed
	// the title to reason about edit distance; a rank cap plus the score on
	// every row asks them to read two titles and decide.
	nearDuplicateTitleGroupLimit = 25
)

// nearDuplicateTitleGroup is one advisory row: titles that fold together, or a
// pair of titles close enough to be worth a look.
//
// A distinct type rather than the map[string]any the exact passes return, and
// that is the point: duplicateResolveRows and queryPrismaDuplicateRows build
// []map[string]any, so a near row cannot be appended into either of them by
// mistake. The compiler enforces "never auto-merged" instead of a comment.
type nearDuplicateTitleGroup struct {
	Group string `json:"group"`
	// Score is what the reader acts on, so it is always published. 1.00 means
	// the titles are equal under normalizeExactTitle — the same title, typed
	// or exported differently — and only that case may reach 1.00;
	// titleTokenSimilarity caps every other pair at titleDistinctMaxScore.
	Score float64 `json:"score"`
	// Count is the number of item keys, matching the exact rows' `count`.
	Count  int      `json:"count"`
	Keys   []string `json:"keys"`
	Titles []string `json:"titles"`
	// ItemType is the type both sides share. Near matching never crosses item
	// types, exactly as the exact pass's GROUP BY does not.
	ItemType string `json:"item_type,omitempty"`
	// RequiresReview is always true, and is published anyway: a row handed to
	// an agent out of context has to carry its own instruction, because the
	// one thing that must not happen to it is an automatic merge.
	RequiresReview bool `json:"requires_review"`
}

// queryNearDuplicateTitles reports title cohorts that are close but not equal,
// strongest first, with the pre-cap total.
//
// Deliberately NOT called by library_health.go or library_prisma.go, and this
// is a decision rather than an omission. PRISMA duplicate-removal counts are
// published numbers in a manuscript: moving them as a side effect of a detector
// improvement would silently restate a screening figure a reader cannot
// re-derive. `library health` grades a library and gates CI for some users, so
// failing it on an advisory judgement call turns "two titles look similar" into
// a build break. Both stay on the equality contract; this report is where a
// person is already looking at candidate pairs and can confirm one.
//
// Complexity. The SQL groups on the same LOWER(TRIM(...)) key as the exact
// pass, so it returns one row per distinct stored title (n rows, one table
// scan) rather than one per item. Folding those into units is O(n) hashing,
// and it is where the cheap wins come from: a title that differs only in
// punctuation or case is answered by the hash, with no scoring at all.
// Scoring is NOT all pairs — nearTitleBlocks bounds the compared pairs to
// O(n * nearDuplicateTitleBlockKeys * nearDuplicateTitleBlockMaxCohorts),
// linear in library size with a fixed constant — and retention is bounded by
// nearTitleGroupCollector, so neither time nor memory follows the pair count.
//
// Measured on an M1 Max: 50,000 distinct titles complete in 0.61s, and a
// deliberately adversarial library where every blocking word sits exactly at
// the bucket cap scores 774,872 pairs in 1.9s while holding only the reported
// rows. The naive all-pairs pass over those 50,000 titles is 1.25e9
// comparisons.
func queryNearDuplicateTitles(ctx context.Context, db localQueryStore) ([]nearDuplicateTitleGroup, int, error) {
	rows, err := db.QueryRawContext(ctx, `
SELECT
	MIN(title) AS value,
	item_type,
	json_group_array(id) AS keys
FROM (
	SELECT
		id,
		TRIM(json_extract(data, '$.data.title')) AS title,
		LOWER(TRIM(json_extract(data, '$.data.title'))) AS normalized_title,
		COALESCE(json_extract(data, '$.data.itemType'), '') AS item_type
	FROM resources
	WHERE resource_type = 'items'
		AND COALESCE(TRIM(json_extract(data, '$.data.title')), '') != ''
		AND COALESCE(item_type, '') NOT IN ('attachment', 'annotation', 'note')
)
GROUP BY normalized_title, item_type
ORDER BY normalized_title, item_type`)
	if err != nil {
		return nil, 0, err
	}
	units := buildNearTitleUnits(normalizeDuplicateRows(rows))
	collector := &nearTitleGroupCollector{}
	nearTitleFoldGroups(units, collector)
	nearTitleScoredGroups(units, collector)
	return collector.ranked(), collector.total, nil
}

// nearTitleGroupCollector keeps the top nearDuplicateTitleGroupLimit rows by
// rank and counts every row it was offered, so the report can say "25 of 800"
// without holding 800.
//
// That is a measured hazard rather than a theoretical one: a library where
// every blocking word sits exactly at the bucket cap produced 774,872 scored
// pairs over 50,000 titles, and retaining all of them to publish 25 costs
// hundreds of megabytes for nothing.
type nearTitleGroupCollector struct {
	groups []nearDuplicateTitleGroup
	total  int
}

// offer keeps the row only if it is still in the running. Compaction is
// order-independent — the sort is a strict total order, so the kept set is
// always the best rows of everything offered so far, whichever order the pair
// blocks arrived in.
func (c *nearTitleGroupCollector) offer(group nearDuplicateTitleGroup) {
	c.total++
	c.groups = append(c.groups, group)
	if len(c.groups) >= 4*nearDuplicateTitleGroupLimit {
		c.truncate()
	}
}

func (c *nearTitleGroupCollector) ranked() []nearDuplicateTitleGroup {
	c.truncate()
	return c.groups
}

func (c *nearTitleGroupCollector) truncate() {
	sort.Slice(c.groups, func(i, j int) bool {
		if c.groups[i].Score != c.groups[j].Score {
			return c.groups[i].Score > c.groups[j].Score
		}
		if c.groups[i].Titles[0] != c.groups[j].Titles[0] {
			return c.groups[i].Titles[0] < c.groups[j].Titles[0]
		}
		// Keys break the last tie so two runs over one library print the same
		// list: the pair set is built by iterating a map.
		return c.groups[i].Keys[0] < c.groups[j].Keys[0]
	})
	if len(c.groups) > nearDuplicateTitleGroupLimit {
		c.groups = c.groups[:nearDuplicateTitleGroupLimit]
	}
}

// nearTitleUnit is one title as normalizeExactTitle sees it: every stored
// spelling that folds to the same key, and every item under those spellings.
//
// Folding first is what makes the cheap wins free. Two spellings in one unit
// already ARE the near answer, at O(n) hashing and no scoring at all, and the
// scored pass then compares units rather than spellings, so it never spends a
// comparison on a difference the fold already explained.
type nearTitleUnit struct {
	itemType string
	titles   []string
	keys     []string
	tokens   titleTokens
}

func buildNearTitleUnits(rows []map[string]any) []nearTitleUnit {
	index := make(map[string]int, len(rows))
	units := make([]nearTitleUnit, 0, len(rows))
	for _, row := range rows {
		title := sqlStringValue(row["value"])
		tokens := titleMatchTokens(title)
		if tokens.exact == "" {
			// A title of nothing but punctuation folds away; there is no word
			// in it to be close to.
			continue
		}
		keys, ok := row["keys"].([]string)
		if !ok || len(keys) == 0 {
			continue
		}
		itemType := sqlStringValue(row["item_type"])
		id := itemType + "\x00" + tokens.exact
		pos, seen := index[id]
		if !seen {
			pos = len(units)
			index[id] = pos
			units = append(units, nearTitleUnit{itemType: itemType, tokens: tokens})
		}
		units[pos].titles = append(units[pos].titles, title)
		units[pos].keys = append(units[pos].keys, keys...)
	}
	// json_group_array follows scan order, which is not promised, so both
	// lists are sorted before anything reports them.
	for i := range units {
		sort.Strings(units[i].titles)
		sort.Strings(units[i].keys)
	}
	return units
}

// nearTitleFoldGroups reports the units holding more than one stored spelling:
// "Attention Is All You Need" against "Attention is all you need." These score
// 1.00 because they are equal under normalizeExactTitle, the boundary
// title_similarity.go documents between the same title and a near one — the
// pair differs in how it was typed, not in which work it names.
func nearTitleFoldGroups(units []nearTitleUnit, collector *nearTitleGroupCollector) {
	for _, unit := range units {
		if len(unit.titles) < 2 {
			continue
		}
		collector.offer(newNearDuplicateTitleGroup(1, unit.itemType, unit.titles, unit.keys))
	}
}

// nearTitleScoredGroups scores the blocked candidate pairs and keeps the ones
// above the junk floor.
//
// One row per PAIR of units, never a transitive cluster. Chaining "similar" is
// how a review list becomes a wrong merge — A close to B and B close to C says
// nothing about A and C — and the person confirming needs to see the two titles
// that produced the number.
func nearTitleScoredGroups(units []nearTitleUnit, collector *nearTitleGroupCollector) {
	compared := make(map[[2]int]struct{})
	for _, block := range nearTitleBlocks(units) {
		for i := range block {
			for j := i + 1; j < len(block); j++ {
				left, right := block[i], block[j]
				if left > right {
					left, right = right, left
				}
				pair := [2]int{left, right}
				if _, seen := compared[pair]; seen {
					continue
				}
				compared[pair] = struct{}{}
				if units[left].itemType != units[right].itemType {
					continue
				}
				// Rounded where the number is produced, as items find does, so
				// the score that is published is the score the floor tested.
				raw := titleTokenSimilarity(units[left].tokens, units[right].tokens)
				score := math.Round(raw*nearTitleScoreScale) / nearTitleScoreScale
				if score < nearTitleMinScore {
					continue
				}
				titles := make([]string, 0, len(units[left].titles)+len(units[right].titles))
				titles = append(titles, units[left].titles...)
				titles = append(titles, units[right].titles...)
				keys := make([]string, 0, len(units[left].keys)+len(units[right].keys))
				keys = append(keys, units[left].keys...)
				keys = append(keys, units[right].keys...)
				collector.offer(newNearDuplicateTitleGroup(score, units[left].itemType, titles, keys))
			}
		}
	}
}

// nearTitleBlocks buckets units by their rarest SHARED informative words, so
// scoring compares titles that already have something distinctive in common.
//
// Two words are useless to block on, at opposite ends. A word carried by more
// than nearDuplicateTitleBlockMaxCohorts units — "study", "analysis" — buckets
// half the library, and is skipped; that is the pass's one accepted blind spot,
// since two similar titles built entirely from words that common are never
// compared. A word carried by exactly one unit cannot pair anything at all, so
// spending a slot on it silently costs recall: "Attention Is All You Need"
// against the same title plus a three-word subtitle of unique words used all
// three slots on the subtitle, and the pair was never scored.
//
// A title takes up to nearDuplicateTitleBlockKeys slots rather than one,
// because a typo lands inside a word and would otherwise change which single
// bucket its title chose.
//
// The bound follows from those constants: every bucket holds at most
// nearDuplicateTitleBlockMaxCohorts units, and each unit joins at most
// nearDuplicateTitleBlockKeys buckets, so the compared pairs are
// O(n * keys * maxCohorts) — linear in the number of titles, where the naive
// pass is quadratic.
func nearTitleBlocks(units []nearTitleUnit) map[string][]int {
	const (
		// A title contributes at most this many blocking words.
		nearDuplicateTitleBlockKeys = 3
		// A word carried by more units than this is too common to block on.
		nearDuplicateTitleBlockMaxCohorts = 32
	)
	words := make([][]string, len(units))
	frequency := make(map[string]int)
	for i, unit := range units {
		// Informative words when the title has them, all words otherwise:
		// "The Art of War" is nothing but function words, and blocking it on
		// nothing would drop it from the pass entirely.
		candidates := unit.tokens.informative
		if len(candidates) == 0 {
			candidates = unit.tokens.all
		}
		distinct := make([]string, 0, len(candidates))
		seen := make(map[string]struct{}, len(candidates))
		for _, word := range candidates {
			if _, repeat := seen[word]; repeat {
				// A repeated word is one blocking word, and one document
				// frequency count: counting it twice would make it look
				// rarer than it is.
				continue
			}
			seen[word] = struct{}{}
			distinct = append(distinct, word)
			frequency[word]++
		}
		words[i] = distinct
	}
	blocks := make(map[string][]int)
	for i, distinct := range words {
		sort.Slice(distinct, func(a, b int) bool {
			if frequency[distinct[a]] != frequency[distinct[b]] {
				return frequency[distinct[a]] < frequency[distinct[b]]
			}
			return distinct[a] < distinct[b]
		})
		used := 0
		for _, word := range distinct {
			if frequency[word] < 2 || frequency[word] > nearDuplicateTitleBlockMaxCohorts {
				continue
			}
			blocks[word] = append(blocks[word], i)
			used++
			if used == nearDuplicateTitleBlockKeys {
				break
			}
		}
	}
	return blocks
}

func newNearDuplicateTitleGroup(score float64, itemType string, titles, keys []string) nearDuplicateTitleGroup {
	sort.Strings(titles)
	sort.Strings(keys)
	return nearDuplicateTitleGroup{
		Group:          duplicateGroupKindTitleNear,
		Score:          score,
		Count:          len(keys),
		Keys:           keys,
		Titles:         titles,
		ItemType:       itemType,
		RequiresReview: true,
	}
}

// printNearDuplicateTitleGroups renders the advisory block for a human. It
// leads with the keys and titles the reader acts on, keeps the score last
// because it is advisory, and ends with the command to run — including the
// fact that the resolver will not touch these rows, which is the question a
// reader who just saw a duplicate report will ask next.
//
// groups is never empty: the caller owns that branch.
func printNearDuplicateTitleGroups(cmd *cobra.Command, groups []nearDuplicateTitleGroup, total int) error {
	out := cmd.OutOrStdout()
	// Not "different titles": a 1.00 row IS the same title, differing only in
	// case, punctuation or Unicode form, and calling that different repeats
	// the contradiction this feature was built to remove from `items find`,
	// where a perfect score printed under a heading denying it.
	fmt.Fprint(out, "\npossible duplicate titles (not equal in the store — confirm before merging):\n")
	tw := newTabWriter(out)
	fmt.Fprintln(tw, "KEYS\tTITLES\tSCORE")
	foldEqual := false
	for _, group := range groups {
		if group.Score >= 1 {
			foldEqual = true
		}
		fmt.Fprintf(tw, "%s\t%s\t%.2f\n", strings.Join(group.Keys, ", "), strings.Join(group.Titles, " | "), group.Score)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if total > len(groups) {
		fmt.Fprintf(out, "(%d of %d shown)\n", len(groups), total)
	}
	if foldEqual {
		// The strongest thing this command can say, so say it plainly rather
		// than leaving the reader to infer it from a number.
		fmt.Fprint(out, "\n1.00 means the titles are equal apart from case, punctuation or Unicode form: exactly the pair the exact detector cannot see.\n")
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"\nThese are not duplicate groups: 'items duplicates resolve --title' matches equal titles only and will not merge them. Compare a pair first: zotio items get %s\n",
		groups[0].Keys[0])
	return nil
}
