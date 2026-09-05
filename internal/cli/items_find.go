// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"zotio/internal/store"
)

func newItemsFindCmd(flags *rootFlags) *cobra.Command {
	var query findItemsQuery
	cmd := &cobra.Command{
		Use:   "find",
		Short: "Find locally synced items by identifier, URL, or exact title",
		Example: `  zotio items find --doi 10.1145/3290605.3300709
  zotio items find --isbn 978-0-262-03384-8
  zotio items find --url https://example.org/paper
  zotio items find --openalex W2741809807
  zotio items find --title "Attention Is All You Need"
  zotio items find --citekey smith2023 --json`,
		// Every selector is a flag, so a positional argument is always a
		// mistake — a shell that ate the quotes around a title, most often.
		// Without this, `find --title "Random Forests" extra junk` returned
		// Random Forests and never mentioned the junk. `items similar` already
		// declares its arity (items_similar.go), so the convention exists.
		Args:        cobra.NoArgs,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			normalizedQuery, err := query.normalized()
			if err != nil {
				return err
			}
			if normalizedQuery.empty() {
				return fmt.Errorf("at least one of --doi, --arxiv, --isbn, --pmid, --citekey, --url, --openalex, or --title is required")
			}
			rawDB, err := openStoreForRead(cmd.Context(), "zotio")
			if err != nil {
				return fmt.Errorf("opening local database: %w", err)
			}
			if rawDB == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Run 'zotio sync' first to enable item lookup.")
				return nil
			}
			defer rawDB.Close()
			db := localQueryStore{Store: rawDB}

			rows, err := queryFindItemsExact(cmd.Context(), db, normalizedQuery)
			if err != nil {
				return fmt.Errorf("querying local items: %w", err)
			}
			data, err := json.Marshal(extractItemDataRows(rows))
			if err != nil {
				return err
			}
			// An exact lookup that finds nothing cannot say whether the item
			// is absent or the input was mistyped, and that silence is the
			// whole failure. Near matches answer it, for the two selectors a
			// person types by hand: the title and the citekey. Both are
			// gathered only when the lookup found nothing at all, and neither
			// ever enters .results: `import resolve` and every caller that
			// treats a hit as identity must keep reading an exact-equality
			// answer.
			var near []nearTitleMatch
			nearTotal := 0
			// Four outcomes used to collapse into one absent envelope key, and
			// one of them was a failure: a caller could not tell "nothing is
			// close" from "the near lookup broke". Name the state so an agent
			// branches on data instead of on absence.
			titleLookup := ""
			if normalizedQuery.Title != "" {
				if len(rows) > 0 {
					// The selectors are OR-ed, so a --doi hit alongside a
					// --title miss still returns rows. Test the title itself
					// rather than call any hit an exact title hit: the near
					// lookup deliberately does not run once anything matched,
					// so in that case this command never answered the title
					// question and the key stays absent, which is what absence
					// has always meant here.
					if findRowsMatchTitleExactly(rows, normalizedQuery.Title) {
						titleLookup = titleLookupExactHit
					}
				} else {
					var nearErr error
					near, nearTotal, nearErr = queryNearTitleMatches(cmd.Context(), rawDB, normalizedQuery.Title)
					state, fatal := nearLookupState(cmd, "title", len(near), nearErr)
					if fatal != nil {
						return fatal
					}
					titleLookup = state
				}
			}
			var nearKeys []nearCiteKeyMatch
			nearKeysTotal := 0
			citekeyLookup := ""
			if normalizedQuery.Citekey != "" {
				if len(rows) > 0 {
					// Same rule as the title, for the same reason: --doi X
					// --citekey Y can return rows while the citekey missed,
					// and calling that an exact hit would report a match that
					// never happened.
					if findRowsMatchCitekeyExactly(rows, normalizedQuery.Citekey) {
						citekeyLookup = citekeyLookupExactHit
					}
				} else {
					var nearErr error
					nearKeys, nearKeysTotal, nearErr = queryNearCiteKeyMatches(cmd.Context(), db, normalizedQuery.Citekey)
					state, fatal := nearLookupState(cmd, "citekey", len(nearKeys), nearErr)
					if fatal != nil {
						return fatal
					}
					citekeyLookup = state
				}
			}
			// Item lookup has no live API equivalent, so it always reads the
			// local mirror. Use the shared envelope to keep `.results` stable.
			prov := localProvenance(rawDB, "items", "local_only")
			// Inside meta, not beside it: these annotate the read rather than
			// adding data, and `extra` is reserved for top-level sibling
			// payloads.
			metaExtra := make(map[string]any, 4)
			if titleLookup != "" {
				metaExtra["title_lookup"] = titleLookup
				if nearTotal > len(near) {
					// The prose block says "(5 of 8 shown)". Without the same
					// number here an agent would read "not in the five" as
					// "not in the library", which is the asymmetry between the
					// two halves of this output that the year field also had.
					metaExtra["near_title_total"] = nearTotal
				}
			}
			if citekeyLookup != "" {
				metaExtra["citekey_lookup"] = citekeyLookup
				if nearKeysTotal > len(nearKeys) {
					metaExtra["near_citekey_total"] = nearKeysTotal
				}
			}
			if len(metaExtra) > 0 {
				prov.MetaExtra = metaExtra
			}
			printProvenance(cmd, countResultItems(data), prov)
			if wantsJSONEnvelope(cmd.OutOrStdout(), flags) {
				filtered := data
				if flags.selectFields != "" {
					filtered = filterFields(filtered, flags.selectFields)
				} else if flags.compact {
					filtered = compactFields(filtered)
				}
				// Sibling keys, never inside .results: the envelope invariant
				// is that .results holds items matched by identity.
				//
				// --select and --compact deliberately do NOT reach them. That
				// runs against printOutputWithFlags, where an explicit field
				// list is the caller's authoritative request, but these blocks
				// are not the caller's data: they are rank-capped, their shape
				// is fixed, and a caller that selected `key` still needs the
				// title to decide whether the row is their paper.
				var extra map[string]any
				if len(near) > 0 || len(nearKeys) > 0 {
					extra = make(map[string]any, 2)
					if len(near) > 0 {
						extra["near_title_matches"] = near
					}
					if len(nearKeys) > 0 {
						extra["near_citekey_matches"] = nearKeys
					}
				}
				wrapped, wrapErr := wrapWithProvenanceExtra(filtered, prov, extra)
				if wrapErr != nil {
					return wrapErr
				}
				return printOutput(cmd.OutOrStdout(), wrapped, true)
			}
			if wantsHumanTable(cmd.OutOrStdout(), flags) {
				var items []map[string]any
				if json.Unmarshal(data, &items) == nil && len(items) > 0 {
					if err := printAutoTable(cmd.OutOrStdout(), items); err != nil {
						return err
					}
					if len(items) >= 25 {
						fmt.Fprintf(cmd.ErrOrStderr(), "\nShowing %d results. To narrow: add --json --select or more lookup flags.\n", len(items))
					}
					return nil
				}
			}
			// The whole human answer to a hand-typed selector that matched
			// nothing. `[]` was printed whenever nothing was close, so the
			// human half of this fix only worked when a near match existed.
			if wantsNearMatchProse(flags) && (findLookupMissed(titleLookup) || findLookupMissed(citekeyLookup)) {
				return printFindLookupMiss(cmd, normalizedQuery,
					nearTitleBlock{matches: near, total: nearTotal, lookup: titleLookup},
					nearCiteKeyBlock{matches: nearKeys, total: nearKeysTotal, lookup: citekeyLookup})
			}
			// Reaching here with rows in hand means nothing carried them:
			// --plain and --csv keep wantsJSONEnvelope false, so those callers
			// saw nothing on either stream. --quiet is exempt because there
			// the exit code is the whole answer (helpers.go printOutputWithFlags).
			//
			// The counts are the rank-capped lengths, not the totals: this
			// promises what --json will actually hand back, and the envelope
			// carries the capped lists. The full count belongs to the prose
			// block, where the reader can act on it.
			if !flags.quiet {
				if len(near) > 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "note: %s found and not shown in this format; re-run with --json to see them.\n",
						pluralCount(len(near), "near title", "near titles"))
				}
				if len(nearKeys) > 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "note: %s found and not shown in this format; re-run with --json to see them.\n",
						pluralCount(len(nearKeys), "near citekey", "near citekeys"))
				}
			}
			return printOutputWithFlags(cmd.OutOrStdout(), data, flags)
		},
	}
	cmd.Flags().StringVar(&query.DOI, "doi", "", "Find items with this DOI")
	cmd.Flags().StringVar(&query.ArXiv, "arxiv", "", "Find items with this arXiv ID or URL")
	cmd.Flags().StringVar(&query.ISBN, "isbn", "", "Find items with this ISBN")
	cmd.Flags().StringVar(&query.PMID, "pmid", "", "Find items with this PMID in Extra")
	cmd.Flags().StringVar(&query.Citekey, "citekey", "", "Find items with this Better BibTeX citation key; when the lookup as a whole matches nothing, the closest citation keys are reported separately (near_citekey_matches in JSON) and never as results")
	cmd.Flags().StringVar(&query.URL, "url", "", "Find items with this normalized URL")
	cmd.Flags().StringVar(&query.OpenAlex, "openalex", "", "Find items with this OpenAlex work ID or URL")
	cmd.Flags().StringVar(&query.Title, "title", "", "Find items with this exact title, ignoring case, whitespace, quote and dash styling, and a trailing full stop; when the lookup as a whole matches nothing, the closest titles are reported separately (near_title_matches in JSON) and never as results")

	return cmd
}

// titleLookup* name the outcome of a --title lookup, reported as
// meta.title_lookup. Without them all four outcomes were one absent key, and
// one of the four is a failure: a caller could not distinguish "no title in
// the library is close" from "the near-title lookup itself broke", so an agent
// had to treat a broken feature as a confident negative.
const (
	titleLookupExactHit = "exact_hit"
	titleLookupNear     = "near_matches"
	titleLookupNoNear   = "no_near_matches"
	titleLookupFailed   = "near_lookup_failed"
)

// citekeyLookup* name the outcome of a --citekey lookup, reported as
// meta.citekey_lookup. The vocabulary is deliberately the SAME four words as
// the title lookup: both selectors are typed by hand, both answer the same
// four questions, and an agent that learned to branch on one must not have to
// learn a second spelling for the other.
const (
	citekeyLookupExactHit = titleLookupExactHit
	citekeyLookupNear     = titleLookupNear
	citekeyLookupNoNear   = titleLookupNoNear
	citekeyLookupFailed   = titleLookupFailed
)

// findLookupMissed reports whether a lookup state means "the thing you asked
// for is not here". An empty state means the question was never answered (no
// such selector, or another selector matched first), which is not a miss.
func findLookupMissed(lookup string) bool {
	return lookup != "" && lookup != titleLookupExactHit
}

// nearLookupState classifies a near lookup that ran because the exact lookup
// matched nothing. Both selectors share it so the four states cannot drift
// into two vocabularies, and label names the lookup in the warning.
//
// The second return value is fatal: a cancelled or timed-out context is NOT
// advisory data. Degrading it to a warning and exiting 0 would report "not
// found" for a lookup that never finished.
func nearLookupState(cmd *cobra.Command, label string, rows int, err error) (string, error) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "", err
	case err != nil:
		// Advisory data. Name the failure instead of silently reverting to
		// the empty answer this exists to explain.
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: near-%s lookup unavailable: %v\n", label, err)
		return titleLookupFailed, nil
	case rows == 0:
		return titleLookupNoNear, nil
	default:
		return titleLookupNear, nil
	}
}

// findRowsMatchTitleExactly reports whether any returned row matched on the
// title itself, reusing the one exact matcher rather than a second folding.
// The selectors are OR-ed, so "rows came back" and "the title matched" are
// different facts, and meta.title_lookup must report the second.
func findRowsMatchTitleExactly(rows []map[string]any, title string) bool {
	titleOnly := findItemsQuery{Title: title}
	for _, row := range rows {
		if findRowMatchesExact(row, titleOnly) {
			return true
		}
	}
	return false
}

// findRowsMatchCitekeyExactly reports whether any returned row matched on the
// citekey itself, for the same reason as the title: the selectors are OR-ed,
// so "rows came back" and "the citekey matched" are different facts, and
// meta.citekey_lookup must report the second.
func findRowsMatchCitekeyExactly(rows []map[string]any, citekey string) bool {
	citekeyOnly := findItemsQuery{Citekey: citekey}
	for _, row := range rows {
		if findRowMatchesExact(row, citekeyOnly) {
			return true
		}
	}
	return false
}

// queryNearCiteKeyMatches gathers approximate citekey matches for a citekey
// that matched nothing exactly. Unlike a title, a citekey has no index to
// generate candidates from — it is a short opaque string inside `extra` or
// the citationKey field, and no prefix of it is meaningful, so there is
// nothing for SQLite to narrow with. The whole citekey inventory is therefore
// read and ranked in Go. That is affordable exactly here: the read happens
// only when the lookup found nothing, one row per item, three columns wide,
// and it is the same query `items citekey-conflicts` and `library health`
// already run (citekeyAuditQuery), so the two layers cannot disagree about
// what a citekey is or where it lives.
//
// The second return value is how many DISTINCT keys cleared the admission
// bound before the rank cap, so the reader can be told the list was
// truncated. Distinct keys, because that is what the capped list holds:
// rankNearCiteKeys reports a key held by several items once, so counting
// rows here would print "(3 of 5 shown)" for a library with three keys in it.
func queryNearCiteKeyMatches(ctx context.Context, db localQueryStore, citekey string) ([]nearCiteKeyMatch, int, error) {
	rows, err := db.QueryRawContext(ctx, citekeyAuditQuery)
	if err != nil {
		return nil, 0, err
	}
	items := buildCitekeyItems(rows)
	candidates := make([]citeKeyCandidate, 0, len(items))
	for _, item := range items {
		if item.CiteKey == "" {
			continue
		}
		candidates = append(candidates, citeKeyCandidate{CiteKey: item.CiteKey, ItemKey: item.Key, Title: item.Title})
	}
	ranked := rankNearCiteKeys(citekey, candidates, len(candidates))
	total := len(ranked)
	if total > nearCiteKeyMatchLimit {
		ranked = ranked[:nearCiteKeyMatchLimit]
	}
	return ranked, total, nil
}

// queryNearTitleMatches gathers approximate title matches for a title that
// matched nothing exactly. Candidate generation is the store's FTS index
// (bm25 order); ranking is titleTokenSimilarity. Splitting it that way keeps
// the scan in SQLite — a Go-side edit distance over every item would read the
// whole library to answer a lookup that found nothing.
//
// The second return value is how many DISTINCT items cleared the junk floor
// before the rank cap, so the reader can be told the list was truncated.
// Distinct items for the same reason the citekey path counts distinct keys:
// rankNearTitleMatches reports one item once, so counting scored rows here
// would print "(6 of 5 shown)" for a library holding five near titles.
// Ranking the full set and slicing here costs nothing: rankNearTitleMatches
// sorts every scored candidate before it slices either way.
func queryNearTitleMatches(ctx context.Context, db *store.Store, title string) ([]nearTitleMatch, int, error) {
	rows, err := db.TitleCandidates(ctx, title, titleCandidateLimit)
	if err != nil {
		return nil, 0, err
	}
	candidates := make([]titleCandidate, 0, len(rows))
	details := make(map[string]nearTitleMatch, len(rows))
	for _, row := range rows {
		var payload struct {
			Key  string `json:"key"`
			Data struct {
				Key      string `json:"key"`
				Title    string `json:"title"`
				Date     string `json:"date"`
				ItemType string `json:"itemType"`
				// Zotero writes the trash marker as 1, and other paths in
				// this package have seen `true`, so decode it loosely rather
				// than let one row shape drop the candidate entirely.
				Deleted any `json:"deleted"`
			} `json:"data"`
		}
		if json.Unmarshal(row, &payload) != nil {
			continue
		}
		key := payload.Data.Key
		if key == "" {
			key = payload.Key
		}
		if key == "" || strings.TrimSpace(payload.Data.Title) == "" {
			continue
		}
		candidates = append(candidates, titleCandidate{Key: key, Title: payload.Data.Title})
		details[key] = nearTitleMatch{
			Year:     findItemYear(payload.Data.Date),
			ItemType: payload.Data.ItemType,
			// TitleCandidates does not filter the trash, and the exact path
			// prints a DELETED column, so an unmarked trashed near match hid
			// information the other half of this command already shows.
			Trashed: duplicateResolveTruthy(payload.Data.Deleted),
		}
	}
	ranked := rankNearTitleMatches(title, candidates, len(candidates))
	total := len(ranked)
	if total > nearTitleMatchLimit {
		ranked = ranked[:nearTitleMatchLimit]
	}
	for i := range ranked {
		detail := details[ranked[i].Key]
		ranked[i].Year = detail.Year
		ranked[i].ItemType = detail.ItemType
		ranked[i].Trashed = detail.Trashed
	}
	return ranked, total, nil
}

var findItemYearPattern = regexp.MustCompile(`\b((?:1[5-9]|20)[0-9]{2})\b`)

// findItemYear pulls a four-digit year out of a Zotero date field. The field is
// free text and holds everything from "2017" to "March 3, 2017" to "n.d.", so a
// year is reported only when one is unambiguously present.
func findItemYear(date string) string {
	return findItemYearPattern.FindString(date)
}

// wantsNearMatchProse reports whether a near-match block may be written to
// stdout as prose. --plain and --csv are machine formats with no column for a
// score, and --quiet asked for less output; interleaving a prose block into any
// of them corrupts what the caller is parsing.
//
// Those callers do NOT get the rows another way. --plain and --csv keep
// wantsJSONEnvelope false (helpers.go), so no envelope is ever built and the
// rows reach neither stream; the command prints a stderr note naming the count
// and --json instead. --quiet reaches the envelope only when stdout is not a
// terminal.
//
// flags is never nil here: RunE dereferences it (flags.selectFields,
// flags.asJSON) long before this is consulted, so a nil-guard would have been
// a branch no run can reach.
func wantsNearMatchProse(flags *rootFlags) bool {
	return !flags.plain && !flags.csv && !flags.quiet
}

// nearMatchMissingField is the placeholder for a column a row cannot fill. A
// blank cell reads as a rendering bug, and the envelope reports the same gap as
// an empty string, so the two halves describe the same row.
const nearMatchMissingField = "----"

// advisoryCellWidth is printTable's cell budget (root.go). The advisory blocks
// print beside those tables, so a title has one display length in this
// command, not one per renderer.
const advisoryCellWidth = 48

// advisoryCell prepares one library-supplied value for a tab-separated
// advisory row: the treatment flags.printTable already gives every table cell
// (sanitizeForTerminal, then truncate to advisoryCellWidth), plus the tab.
//
// A title, a citation key and an item type are publisher- or user-supplied
// text, so any of them can carry a control byte. Unsanitized, a newline ended
// the row early and printed the rest of the title as a line of its own — text
// the library chose, rendered where zotio's own output goes — and an ANSI
// escape recoloured the terminal from a data field.
//
// The tab is the one addition. printTable renders through renderColumns,
// which aligns by display width and treats a tab as an ordinary rune;
// these blocks feed text/tabwriter, where a tab inside a cell opens a column
// and shifts every later value under the wrong header, so the delimiter has
// to go. sanitizeForTerminal deliberately keeps tabs (helpers.go), hence the
// replacement here rather than there.
//
// Truncation is display-only and never reaches JSON: near_title_matches and
// near_citekey_matches carry the stored value verbatim, because a consumer
// diffing a title against its own record needs the bytes, and an escape
// sequence in a JSON string is inert.
func advisoryCell(s string) string {
	return truncate(sanitizeForTerminal(strings.ReplaceAll(s, "\t", " ")), advisoryCellWidth)
}

// nearTitleBlock and nearCiteKeyBlock each group the three values a miss
// answer needs: the ranked rows, how many there were before the rank cap, and
// the lookup state. They always travel together, and one selector missing
// says nothing about the other.
type nearTitleBlock struct {
	matches []nearTitleMatch
	total   int
	lookup  string
}

type nearCiteKeyBlock struct {
	matches []nearCiteKeyMatch
	total   int
	lookup  string
}

// printFindLookupMiss is the human answer to a lookup that matched nothing.
// "matched: none" belongs here rather than in either renderer: it is the
// answer to the command, printed once, while each selector then explains
// itself. A genuinely absent title or citekey has no rows to render, and the
// bare `[]` that fell out of the generic writer told the reader the command
// was broken instead of answering their question.
//
// Both selectors can miss in the same run (--title X --citekey Y), and each
// block is a separate answer: a close citekey does not make the title
// present, so neither may stand in for the other.
func printFindLookupMiss(cmd *cobra.Command, query findItemsQuery, title nearTitleBlock, citekey nearCiteKeyBlock) error {
	fmt.Fprint(cmd.OutOrStdout(), "matched: none\n\n")
	titleMissed := findLookupMissed(title.lookup)
	if titleMissed {
		if err := printTitleLookupMiss(cmd, query.Title, title.matches, title.total, title.lookup); err != nil {
			return err
		}
	}
	if !findLookupMissed(citekey.lookup) {
		return nil
	}
	if titleMissed {
		fmt.Fprintln(cmd.OutOrStdout())
	}
	return printCiteKeyLookupMiss(cmd, query.Citekey, citekey.matches, citekey.total, citekey.lookup)
}

// printTitleLookupMiss explains a --title lookup that matched nothing.
func printTitleLookupMiss(cmd *cobra.Command, title string, matches []nearTitleMatch, total int, lookup string) error {
	if len(matches) == 0 {
		return printNoNearTitleMatches(cmd, title, lookup)
	}
	return printNearTitleMatches(cmd, title, matches, total)
}

// printCiteKeyLookupMiss explains a --citekey lookup that matched nothing.
func printCiteKeyLookupMiss(cmd *cobra.Command, citekey string, matches []nearCiteKeyMatch, total int, lookup string) error {
	if len(matches) == 0 {
		return printNoNearCiteKeyMatches(cmd, citekey, lookup)
	}
	return printNearCiteKeyMatches(cmd, citekey, matches, total)
}

// printNearCiteKeyMatches renders the near-citekey block, the same shape as
// the near-title block and for the same reason: the reader asked for one key
// and is being shown different keys, so a list that looks like a result set
// invites treating the top row as the answer.
//
// Columns lead with what the reader acts on. The suggested citekey comes
// first because it is what they will paste into the manuscript, then the item
// key for the next command, then the title that tells them whether the row is
// their source, and the advisory score last.
//
// matches is never empty: printCiteKeyLookupMiss owns that branch.
func printNearCiteKeyMatches(cmd *cobra.Command, citekey string, matches []nearCiteKeyMatch, total int) error {
	out := cmd.OutOrStdout()
	fmt.Fprint(out, "near citekeys (different keys — confirm before using):\n")
	tw := newTabWriter(out)
	fmt.Fprintln(tw, "CITEKEY\tKEY\tTITLE\tSCORE")
	for _, match := range matches {
		title := advisoryCell(match.Title)
		if title == "" {
			title = nearMatchMissingField
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.2f\n", advisoryCell(match.CiteKey), advisoryCell(match.ItemKey), title, match.Score)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if total > len(matches) {
		fmt.Fprintf(out, "(%d of %d shown)\n", len(matches), total)
	}
	// The item key is library data too, and this line is the command the
	// reader pastes: sanitized, never truncated, because half a key is a
	// command that fails. The queried citekey is the reader's own input and
	// %q escapes it already.
	fmt.Fprintf(cmd.ErrOrStderr(), "\nNo item has the citation key %q. If one above is the paper you meant, then run: zotio items get %s\n",
		citekey, sanitizeForTerminal(matches[0].ItemKey))
	return nil
}

// printNoNearCiteKeyMatches answers a citekey lookup that found nothing and
// had nothing close. A failed near lookup is kept distinct: reporting
// "nothing is close" when the search never ran would be a confident negative
// the command did not earn.
func printNoNearCiteKeyMatches(cmd *cobra.Command, citekey, lookup string) error {
	if lookup == citekeyLookupFailed {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"No item has the citation key %q. The near-citekey lookup failed (reported above), so whether a close key exists is unknown.\n", citekey)
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"No item has the citation key %q, and no citation key in your library is close to it. This reads the local mirror only, so run 'zotio sync' if you expected a hit.\n", citekey)
	return nil
}

// printNearTitleMatches renders the near-match block. It states that these are
// NOT matches, because the reader arrived here by asking for one title and is
// being shown different titles; a list that looks like a result set invites
// treating the top row as the answer.
//
// Columns lead with what the reader acts on — the key they will pass to the
// next command — and end with the score, which is advisory. That is also the
// house shape for a ranked local list (printItemSimilarReport in
// items_similar.go: tab-separated, uppercase header, score beside the row it
// explains). Item type is always shown because two rows can otherwise be
// identical in every printed column: a preprint and its journal version share
// a title and a year.
//
// total is the count before the rank cap. Printing five of eight with nothing
// to say so tells the reader their paper is absent when it is on row six.
//
// matches is never empty: printTitleLookupMiss owns that branch, because an
// empty list is a different answer rather than a table with no rows.
func printNearTitleMatches(cmd *cobra.Command, title string, matches []nearTitleMatch, total int) error {
	out := cmd.OutOrStdout()
	fmt.Fprint(out, "near titles (different titles — confirm before using):\n")
	tw := newTabWriter(out)
	fmt.Fprintln(tw, "KEY\tYEAR\tTYPE\tTITLE\tSCORE")
	for _, match := range matches {
		year := advisoryCell(match.Year)
		if year == "" {
			year = nearMatchMissingField
		}
		itemType := advisoryCell(match.ItemType)
		if itemType == "" {
			itemType = nearMatchMissingField
		}
		// Truncated before the marker is appended, so a long title cannot
		// push "(trashed)" out of the cell that has to carry it.
		shownTitle := advisoryCell(match.Title)
		if match.Trashed {
			// The exact path prints a DELETED column, so an unmarked trashed
			// row here would hide what the other half of this command shows.
			shownTitle += " (trashed)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.2f\n", advisoryCell(match.Key), year, itemType, shownTitle, match.Score)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if total > len(matches) {
		fmt.Fprintf(out, "(%d of %d shown)\n", len(matches), total)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "\nNo item has the title %q. If one above is the paper you meant, then run: zotio items get %s\n",
		title, sanitizeForTerminal(matches[0].Key))
	return nil
}

// printNoNearTitleMatches answers a title lookup that found nothing and had
// nothing close. The rows are the interesting case, so this branch used to fall
// through to a bare `[]` — which reads as a broken command rather than as the
// answer "that paper is not here". Name the state and the remedy.
//
// A failed near lookup is kept distinct: reporting "nothing is close" when the
// search never ran would be a confident negative the command did not earn.
func printNoNearTitleMatches(cmd *cobra.Command, title, lookup string) error {
	if lookup == titleLookupFailed {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"No item has the title %q. The near-title lookup failed (reported above), so whether a close title exists is unknown.\n", title)
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"No item has the title %q, and no title in your library is close to it. This reads the local mirror only, so run 'zotio sync' if you expected a hit.\n", title)
	return nil
}

type findItemsQuery struct {
	DOI      string
	ArXiv    string
	ISBN     string
	PMID     string
	Citekey  string
	URL      string
	OpenAlex string
	Title    string
}

func (q findItemsQuery) normalized() (findItemsQuery, error) {
	q.DOI = normalizeDOI(q.DOI)
	rawArXiv := strings.TrimSpace(q.ArXiv)
	q.ArXiv = normalizeFindArxivID(rawArXiv)
	if rawArXiv != "" && q.ArXiv == "" {
		return findItemsQuery{}, fmt.Errorf("--arxiv must be an ID such as 2401.00001 or an arxiv.org abs/pdf URL")
	}
	q.ISBN = strings.TrimSpace(q.ISBN)
	q.PMID = strings.TrimSpace(q.PMID)
	q.Citekey = strings.TrimSpace(q.Citekey)
	q.URL = normalizeFindURL(q.URL)
	rawOpenAlex := strings.TrimSpace(q.OpenAlex)
	q.OpenAlex = normalizeOpenAlexWorkID(rawOpenAlex)
	if rawOpenAlex != "" && q.OpenAlex == "" {
		return findItemsQuery{}, fmt.Errorf("--openalex must be a work ID such as W2741809807 or an openalex.org work URL")
	}
	q.Title = strings.TrimSpace(q.Title)
	return q, nil
}

func (q findItemsQuery) empty() bool {
	return q.DOI == "" && q.ArXiv == "" && q.ISBN == "" && q.PMID == "" &&
		q.Citekey == "" && q.URL == "" && q.OpenAlex == "" && q.Title == ""
}

func findItemsCandidateSQL(q findItemsQuery) (string, []any) {
	clauses := make([]string, 0, 8)
	args := make([]any, 0, 12)
	if q.DOI != "" {
		// DOI normalization happens in Go. Keep URL-form and bare DOI rows.
		clauses = append(clauses, `COALESCE(json_extract(data, '$.data.DOI'), '') <> ''`)
	}
	if q.ArXiv != "" {
		clauses = append(clauses, `(
			COALESCE(json_extract(data, '$.data.archiveID'), '') LIKE '%' || ? || '%' ESCAPE '\'
			OR COALESCE(json_extract(data, '$.data.url'), '') LIKE '%' || ? || '%' ESCAPE '\'
			OR COALESCE(json_extract(data, '$.data.extra'), '') LIKE '%' || ? || '%' ESCAPE '\'
		)`)
		escaped := escapeSQLiteLikeLiteral(q.ArXiv)
		args = append(args, escaped, escaped, escaped)
	}
	if q.ISBN != "" {
		clauses = append(clauses, `COALESCE(json_extract(data, '$.data.ISBN'), '') <> ''`)
	}
	if q.PMID != "" {
		clauses = append(clauses, `COALESCE(json_extract(data, '$.data.extra'), '') LIKE '%' || ? || '%' ESCAPE '\'`)
		args = append(args, escapeSQLiteLikeLiteral(q.PMID))
	}
	if q.Citekey != "" {
		clauses = append(clauses, `(
			COALESCE(json_extract(data, '$.data.extra'), '') LIKE '%' || ? || '%' ESCAPE '\'
			OR COALESCE(json_extract(data, '$.data.citationKey'), '') <> ''
		)`)
		args = append(args, escapeSQLiteLikeLiteral(q.Citekey))
	}
	if q.URL != "" {
		// URL normalization happens in Go. Keep every URL-bearing candidate
		// because equivalent URLs can differ in host case, fragment, or slash.
		clauses = append(clauses, `TRIM(COALESCE(json_extract(data, '$.data.url'), '')) <> ''`)
	}
	if q.OpenAlex != "" {
		clauses = append(clauses, `(
			COALESCE(json_extract(data, '$.data.url'), '') LIKE '%' || ? || '%' ESCAPE '\'
			OR COALESCE(json_extract(data, '$.data.archiveID'), '') LIKE '%' || ? || '%' ESCAPE '\'
			OR COALESCE(json_extract(data, '$.data.repository'), '') LIKE '%' || ? || '%' ESCAPE '\'
			OR COALESCE(json_extract(data, '$.data.archiveLocation'), '') LIKE '%' || ? || '%' ESCAPE '\'
			OR COALESCE(json_extract(data, '$.data.extra'), '') LIKE '%' || ? || '%' ESCAPE '\'
		)`)
		escaped := escapeSQLiteLikeLiteral(q.OpenAlex)
		args = append(args, escaped, escaped, escaped, escaped, escaped)
	}
	if q.Title != "" {
		// SQLite TRIM/LOWER is narrower than Go's Unicode-aware exact check.
		clauses = append(clauses, `COALESCE(json_extract(data, '$.data.title'), '') <> ''`)
	}
	return `
SELECT id, data
FROM resources
WHERE resource_type = 'items'
	AND (parent_key IS NULL OR parent_key = '')
	AND (` + strings.Join(clauses, "\n\t\tOR ") + `)
ORDER BY id`, args
}

// queryFindItemsExact streams broad normalization candidates under the command
// context and retains only exact matches. URL and title equivalence cannot be
// expressed safely with SQLite's narrower case and whitespace rules.
func queryFindItemsExact(ctx context.Context, db localQueryStore, query findItemsQuery) ([]map[string]any, error) {
	sqlQuery, sqlArgs := findItemsCandidateSQL(query)
	cursor, err := db.QueryContext(ctx, sqlQuery, sqlArgs...)
	if err != nil {
		return nil, err
	}
	defer cursor.Close()

	matches := make([]map[string]any, 0)
	for cursor.Next() {
		var id, data string
		if err := cursor.Scan(&id, &data); err != nil {
			return nil, err
		}
		row := map[string]any{"id": id, "data": data}
		if findRowMatchesExact(row, query) {
			matches = append(matches, row)
		}
	}
	return matches, cursor.Err()
}

var findArxivIDInputPattern = regexp.MustCompile(`(?i)^([a-z-]+/[0-9]{7}|[0-9]{4}\.[0-9]{4,5})(?:v[0-9]+)?(?:\.pdf)?$`)

func normalizeFindArxivID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(value), "arxiv:") {
		value = strings.TrimSpace(value[len("arxiv:"):])
	} else if parsed, err := neturl.Parse(value); err == nil && parsed.Scheme != "" {
		scheme := strings.ToLower(parsed.Scheme)
		host := strings.ToLower(parsed.Hostname())
		if (scheme != "http" && scheme != "https") ||
			(host != "arxiv.org" && host != "www.arxiv.org" && host != "export.arxiv.org") {
			return ""
		}
		path := strings.Trim(parsed.Path, "/")
		lowerPath := strings.ToLower(path)
		switch {
		case strings.HasPrefix(lowerPath, "abs/"):
			value = path[len("abs/"):]
		case strings.HasPrefix(lowerPath, "pdf/"):
			value = path[len("pdf/"):]
		default:
			return ""
		}
	}
	matches := findArxivIDInputPattern.FindStringSubmatch(value)
	if len(matches) != 2 {
		return ""
	}
	return normalizeArxivID(matches[1])
}

func normalizeFindURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := neturl.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return value
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	parsed.RawFragment = ""
	if parsed.Path == "/" && parsed.RawPath == "" {
		parsed.Path = ""
	} else if parsed.RawPath != "" {
		// RawPath distinguishes a literal terminal slash from an encoded %2F.
		if strings.HasSuffix(parsed.RawPath, "/") {
			parsed.Path = strings.TrimSuffix(parsed.Path, "/")
			parsed.RawPath = strings.TrimSuffix(parsed.RawPath, "/")
		}
	} else {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	}
	return parsed.String()
}

func normalizeOpenAlexWorkID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if parsed, err := neturl.Parse(value); err == nil && strings.EqualFold(parsed.Hostname(), "openalex.org") {
		value = strings.Trim(parsed.Path, "/")
	}
	lower := strings.ToLower(value)
	for _, prefix := range []string{"openalex id:", "openalex:"} {
		if strings.HasPrefix(lower, prefix) {
			value = strings.TrimSpace(value[len(prefix):])
			break
		}
	}
	value = strings.ToUpper(strings.Trim(value, " /"))
	if len(value) < 2 || value[0] != 'W' {
		return ""
	}
	for _, digit := range value[1:] {
		if digit < '0' || digit > '9' {
			return ""
		}
	}
	return value
}

func escapeSQLiteLikeLiteral(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	value = strings.ReplaceAll(value, `_`, `\_`)
	return value
}

func extractItemDataRows(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		raw, ok := row["data"].(string)
		if !ok {
			out = append(out, row)
			continue
		}
		var item map[string]any
		if json.Unmarshal([]byte(raw), &item) != nil {
			out = append(out, row)
			continue
		}
		out = append(out, item)
	}
	return out
}

func findRowMatchesExact(row map[string]any, query findItemsQuery) bool {
	raw, ok := row["data"].(string)
	if !ok {
		return false
	}
	var decoded map[string]any
	if json.Unmarshal([]byte(raw), &decoded) != nil {
		return false
	}
	fields := decoded
	if inner, ok := decoded["data"].(map[string]any); ok {
		fields = inner
	}
	stringField := func(name string) string {
		value, _ := fields[name].(string)
		return value
	}
	d := struct {
		DOI             string
		ISBN            string
		ArchiveID       string
		ArchiveLocation string
		Repository      string
		CitationKey     string
		Title           string
		URL             string
		Extra           string
	}{
		DOI:             stringField("DOI"),
		ISBN:            stringField("ISBN"),
		ArchiveID:       stringField("archiveID"),
		ArchiveLocation: stringField("archiveLocation"),
		Repository:      stringField("repository"),
		CitationKey:     stringField("citationKey"),
		Title:           stringField("title"),
		URL:             stringField("url"),
		Extra:           stringField("extra"),
	}
	if query.DOI != "" && strings.EqualFold(normalizeDOI(d.DOI), query.DOI) {
		return true
	}
	if query.ISBN != "" && strings.TrimSpace(d.ISBN) == query.ISBN {
		return true
	}
	if query.ArXiv != "" {
		if normalizeFindArxivID(d.ArchiveID) == query.ArXiv ||
			normalizeFindArxivID(d.URL) == query.ArXiv ||
			extractArxivIDFromString(d.Extra) == query.ArXiv {
			return true
		}
		if extraContainsExactToken(d.Extra, "arXiv: ", query.ArXiv) ||
			extraContainsExactToken(d.Extra, "arXiv:", query.ArXiv) {
			return true
		}
	}
	if query.PMID != "" &&
		(extraContainsExactToken(d.Extra, "PMID: ", query.PMID) ||
			extraContainsExactToken(d.Extra, "PMID:", query.PMID)) {
		return true
	}
	if query.Citekey != "" {
		if strings.TrimSpace(d.CitationKey) == query.Citekey {
			return true
		}
		if extraContainsExactToken(d.Extra, "Citation Key: ", query.Citekey) ||
			extraContainsExactToken(d.Extra, "Citation Key:", query.Citekey) {
			return true
		}
	}
	if query.URL != "" && normalizeFindURL(d.URL) == query.URL {
		return true
	}
	if query.OpenAlex != "" {
		for _, value := range []string{d.URL, d.ArchiveID, d.ArchiveLocation, d.Repository} {
			if normalizeOpenAlexWorkID(value) == query.OpenAlex {
				return true
			}
		}
		if extraContainsExactTokenFold(d.Extra, "OpenAlex: ", query.OpenAlex) ||
			extraContainsExactTokenFold(d.Extra, "OpenAlex:", query.OpenAlex) ||
			extraContainsExactTokenFold(d.Extra, "OpenAlex ID: ", query.OpenAlex) ||
			extraContainsExactTokenFold(d.Extra, "OpenAlex ID:", query.OpenAlex) {
			return true
		}
	}
	// normalizeExactTitle, not the SQL grouping key: items_duplicates.go groups
	// exact title duplicates on LOWER(TRIM(...)) in SQL, which is lowercase and
	// trim only. That folding is too narrow for a title a person typed or
	// pasted, so `--title "Attention is all you need."` — one trailing full
	// stop, exactly what a reference list gives you — missed, and the near
	// block then reported the same paper under a heading saying "different
	// titles".
	if query.Title != "" && normalizeExactTitle(d.Title) == normalizeExactTitle(query.Title) {
		return true
	}
	return false
}

func extraContainsExactTokenFold(extra, prefix, token string) bool {
	return extraContainsExactToken(strings.ToLower(extra), strings.ToLower(prefix), strings.ToLower(token))
}

func extraContainsExactToken(extra, prefix, token string) bool {
	if token == "" {
		return false
	}
	// Extra is line-oriented. Require both the label and token to end at a
	// whitespace boundary so labels embedded in words and token prefixes do not
	// match.
	needle := prefix + token
	searchFrom := 0
	for {
		idx := strings.Index(extra[searchFrom:], needle)
		if idx < 0 {
			return false
		}
		pos := searchFrom + idx
		beforeOK := pos == 0 || isFindTokenBoundary(extra[pos-1])
		after := pos + len(needle)
		afterOK := after >= len(extra) || isFindTokenBoundary(extra[after])
		if beforeOK && afterOK {
			return true
		}
		searchFrom = pos + 1
	}
}

func isFindTokenBoundary(ch byte) bool {
	switch ch {
	case '\n', '\r', ' ', '\t':
		return true
	default:
		return false
	}
}
