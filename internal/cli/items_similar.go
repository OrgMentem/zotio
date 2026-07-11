// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"zotio/internal/store"

	"github.com/spf13/cobra"
)

// item-similarity weights are intentionally simple and explainable: every
// component is a deterministic local-store signal, and the sum is one.
const (
	itemSimilarCollectionWeight = 0.30
	itemSimilarTagWeight        = 0.25
	itemSimilarTextWeight       = 0.25
	itemSimilarCreatorWeight    = 0.10
	itemSimilarVenueWeight      = 0.10

	itemSimilarDefaultLimit             = 10
	itemSimilarMaxDistinctFulltextTerms = 20000
)

type itemSimilarOptions struct {
	Limit    int
	MinScore float64
}

type itemSimilarReport struct {
	Source  itemSimilarSummary `json:"source"`
	Similar []itemSimilarEntry `json:"similar"`
}

type itemSimilarSummary struct {
	Key      string `json:"key"`
	Title    string `json:"title,omitempty"`
	ItemType string `json:"item_type,omitempty"`
}

type itemSimilarEntry struct {
	Rank     int                  `json:"rank"`
	Key      string               `json:"key"`
	Title    string               `json:"title,omitempty"`
	ItemType string               `json:"item_type,omitempty"`
	Score    float64              `json:"score"`
	Signals  itemSimilarSignalSet `json:"signals"`
	Reasons  []string             `json:"reasons"`
}

type itemSimilarSignalSet struct {
	Collections itemSimilarSignal `json:"collections"`
	Tags        itemSimilarSignal `json:"tags"`
	Text        itemSimilarSignal `json:"text"`
	Creators    itemSimilarSignal `json:"creators"`
	Venue       itemSimilarSignal `json:"venue"`
}

type itemSimilarSignal struct {
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

type itemSimilarRecord struct {
	itemSimilarSummary
	Collections map[string]string
	Tags        map[string]string
	Creators    map[string]string
	Venue       string
	VenueLabel  string
	Fulltext    itemSimilarFulltext
}

type itemSimilarFulltext struct {
	Present bool
	Usable  bool
	Rare    map[string]struct{}
}

type itemSimilarFulltextCorpus struct {
	DocumentFrequency map[string]int
	DocumentCount     int
	Source            itemSimilarFulltext
}

func newItemsSimilarCmd(flags *rootFlags) *cobra.Command {
	opts := itemSimilarOptions{Limit: itemSimilarDefaultLimit}
	cmd := &cobra.Command{
		Use:   "similar <itemKey>",
		Short: "Rank locally similar items with explainable signals",
		Long: "Rank locally similar items using five weighted signals: collections (0.30), " +
			"tags (0.25), fulltext rare-word overlap (0.25), creators (0.10), and an exact, binary " +
			"venue match (0.10). Run 'zotio sync' first to populate the local mirror and " +
			"'zotio sync --fulltext' to include the fulltext signal. --min-score filters the " +
			"resulting weighted composite score.",
		Args:        cobra.ExactArgs(1),
		Example:     "  zotio items similar ABC12345\n  zotio items similar ABC12345 --limit 5 --min-score 0.2 --json",
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Limit < 0 {
				return usageErr(fmt.Errorf("--limit must be >= 0"))
			}
			if opts.MinScore < 0 || math.IsNaN(opts.MinScore) || math.IsInf(opts.MinScore, 0) {
				return usageErr(fmt.Errorf("--min-score must be a finite value >= 0"))
			}
			report, found, synced, err := itemSimilarReportFromLocalStore(cmd.Context(), args[0], opts)
			if err != nil {
				return err
			}
			if !synced {
				return preconditionErr(fmt.Errorf("run 'zotio sync' first to enable item similarity"))
			}
			if !found {
				if flags.asJSON {
					data, err := graphNotFoundJSON(args[0])
					if err != nil {
						return err
					}
					return printOutput(cmd.OutOrStdout(), data, true)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Item not found: %s\n", args[0])
				return nil
			}
			if flags.asJSON {
				return printCommandJSON(cmd.OutOrStdout(), report, flags)
			}
			return printItemSimilarReport(cmd, report)
		},
	}
	cmd.Flags().IntVar(&opts.Limit, "limit", itemSimilarDefaultLimit, "Maximum number of similar items to return")
	cmd.Flags().Float64Var(&opts.MinScore, "min-score", 0, "Minimum composite similarity score to include")
	return cmd
}

func itemSimilarReportFromLocalStore(ctx context.Context, key string, opts itemSimilarOptions) (itemSimilarReport, bool, bool, error) {
	db, err := openStoreForRead(ctx, "zotio")
	if err != nil {
		return itemSimilarReport{}, false, false, fmt.Errorf("opening local database: %w", err)
	}
	if db == nil {
		return itemSimilarReport{}, false, false, nil
	}
	defer db.Close()

	report, found, err := buildItemSimilarReport(localQueryStore{db}, key, opts)
	return report, found, true, err
}

func buildItemSimilarReport(db localQueryStore, key string, opts itemSimilarOptions) (itemSimilarReport, bool, error) {
	report := itemSimilarReport{Similar: []itemSimilarEntry{}}
	trashed, err := db.Get("items-trash", key)
	if err != nil {
		return report, false, fmt.Errorf("checking source item trash status: %w", err)
	}
	if trashed != nil {
		return report, false, fmt.Errorf("item is in trash: %s", key)
	}
	raw, err := db.Get("items", key)
	if err != nil {
		return report, false, fmt.Errorf("loading source item: %w", err)
	}
	if raw == nil {
		return report, false, nil
	}

	source, err := itemSimilarRecordFromRaw(raw)
	if err != nil {
		return report, false, fmt.Errorf("parsing source item: %w", err)
	}
	report.Source = source.itemSimilarSummary

	candidates, err := queryItemSimilarCandidates(db)
	if err != nil {
		return report, false, err
	}
	corpus, err := buildItemSimilarFulltextCorpus(db.Store, source.Key)
	if err != nil {
		return report, false, err
	}
	source.Fulltext = corpus.Source

	entries := make([]itemSimilarEntry, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Key == source.Key {
			continue
		}
		entries = append(entries, scoreItemSimilarCandidate(source, candidate))
	}
	if err := applyItemSimilarFulltextScores(db.Store, source, candidates, corpus, entries); err != nil {
		return report, false, err
	}

	filtered := entries[:0]
	for _, entry := range entries {
		if entry.Score <= 0 || entry.Score < opts.MinScore {
			continue
		}
		filtered = append(filtered, entry)
	}
	entries = filtered

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Score == entries[j].Score {
			return entries[i].Key < entries[j].Key
		}
		return entries[i].Score > entries[j].Score
	})
	if opts.Limit >= 0 && len(entries) > opts.Limit {
		entries = entries[:opts.Limit]
	}
	for i := range entries {
		entries[i].Rank = i + 1
	}
	report.Similar = entries
	return report, true, nil
}

func queryItemSimilarCandidates(db localQueryStore) ([]itemSimilarRecord, error) {
	rows, err := db.QuerySimilarityCandidates()
	if err != nil {
		return nil, fmt.Errorf("querying candidate items: %w", err)
	}

	out := make([]itemSimilarRecord, 0, len(rows))
	for _, row := range rows {
		rec, err := itemSimilarRecordFromRaw(row.Data)
		if err != nil {
			return nil, fmt.Errorf("parsing candidate item %s: %w", row.Key, err)
		}
		if rec.Key == "" {
			rec.Key = row.Key
		}
		out = append(out, rec)
	}
	return out, nil
}

func itemSimilarRecordFromRaw(raw json.RawMessage) (itemSimilarRecord, error) {
	var obj struct {
		Key  string `json:"key"`
		Data struct {
			Key              string   `json:"key"`
			ItemType         string   `json:"itemType"`
			Title            string   `json:"title"`
			PublicationTitle string   `json:"publicationTitle"`
			Collections      []string `json:"collections"`
			Tags             []struct {
				Tag string `json:"tag"`
			} `json:"tags"`
			Creators []struct {
				FirstName string `json:"firstName"`
				LastName  string `json:"lastName"`
				Name      string `json:"name"`
			} `json:"creators"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return itemSimilarRecord{}, err
	}
	key := strings.TrimSpace(obj.Data.Key)
	if key == "" {
		key = strings.TrimSpace(obj.Key)
	}
	rec := itemSimilarRecord{
		itemSimilarSummary: itemSimilarSummary{
			Key:      key,
			Title:    strings.TrimSpace(obj.Data.Title),
			ItemType: strings.TrimSpace(obj.Data.ItemType),
		},
		Collections: make(map[string]string),
		Tags:        make(map[string]string),
		Creators:    make(map[string]string),
		VenueLabel:  strings.TrimSpace(obj.Data.PublicationTitle),
	}
	rec.Venue = normalizeItemSimilarToken(rec.VenueLabel)
	for _, coll := range obj.Data.Collections {
		addItemSimilarSetValue(rec.Collections, coll)
	}
	for _, tag := range obj.Data.Tags {
		addItemSimilarSetValue(rec.Tags, tag.Tag)
	}
	for _, creator := range obj.Data.Creators {
		identity, display := normalizeItemSimilarCreator(creator.FirstName, creator.LastName, creator.Name)
		if identity != "" {
			rec.Creators[identity] = display
		}
	}
	return rec, nil
}

func scoreItemSimilarCandidate(source, candidate itemSimilarRecord) itemSimilarEntry {
	collections, sharedCollections := itemSimilarJaccard(source.Collections, candidate.Collections)
	tags, sharedTags := itemSimilarJaccard(source.Tags, candidate.Tags)
	creators, sharedCreators := itemSimilarJaccard(source.Creators, candidate.Creators)
	venue := itemSimilarVenueScore(source, candidate)
	text, sharedTextTerms := itemSimilarTextScore(source.Fulltext, candidate.Fulltext)

	entry := itemSimilarEntry{
		Key:      candidate.Key,
		Title:    candidate.Title,
		ItemType: candidate.ItemType,
		Score: collections*itemSimilarCollectionWeight +
			tags*itemSimilarTagWeight +
			text*itemSimilarTextWeight +
			creators*itemSimilarCreatorWeight +
			venue*itemSimilarVenueWeight,
	}
	entry.Signals = itemSimilarSignalSet{
		Collections: itemSimilarSignal{Score: collections, Reason: itemSimilarSharedReason("collection", "collections", sharedCollections, source.Collections, candidate.Collections)},
		Tags:        itemSimilarSignal{Score: tags, Reason: itemSimilarSharedReason("tag", "tags", sharedTags, source.Tags, candidate.Tags)},
		Text:        itemSimilarSignal{Score: text, Reason: itemSimilarTextReason(text, sharedTextTerms, source.Fulltext, candidate.Fulltext)},
		Creators:    itemSimilarSignal{Score: creators, Reason: itemSimilarSharedReason("creator", "creators", sharedCreators, source.Creators, candidate.Creators)},
		Venue:       itemSimilarSignal{Score: venue, Reason: itemSimilarVenueReason(venue, source, candidate)},
	}
	entry.Reasons = itemSimilarEntryReasons(entry)
	return entry
}

func itemSimilarJaccard(a, b map[string]string) (float64, []string) {
	if len(a) == 0 || len(b) == 0 {
		return 0, nil
	}
	shared := make([]string, 0)
	for key, display := range a {
		if _, ok := b[key]; ok {
			shared = append(shared, display)
		}
	}
	if len(shared) == 0 {
		return 0, nil
	}
	sort.Strings(shared)
	union := len(a) + len(b) - len(shared)
	return float64(len(shared)) / float64(union), shared
}

func itemSimilarVenueScore(source, candidate itemSimilarRecord) float64 {
	if source.Venue == "" || candidate.Venue == "" || source.Venue != candidate.Venue {
		return 0
	}
	return 1
}

func itemSimilarTextScore(source, candidate itemSimilarFulltext) (float64, int) {
	if !source.Present || !candidate.Present || len(source.Rare) == 0 || len(candidate.Rare) == 0 {
		return 0, 0
	}
	shared := 0
	for token := range source.Rare {
		if _, ok := candidate.Rare[token]; ok {
			shared++
		}
	}
	if shared == 0 {
		return 0, 0
	}
	denom := len(source.Rare)
	if len(candidate.Rare) > denom {
		denom = len(candidate.Rare)
	}
	return float64(shared) / float64(denom), shared
}

func buildItemSimilarFulltextCorpus(db *store.Store, sourceKey string) (itemSimilarFulltextCorpus, error) {
	corpus := itemSimilarFulltextCorpus{DocumentFrequency: make(map[string]int)}
	err := db.VisitSimilarityFulltextDocuments(func(doc store.SimilarityFulltextDocument) error {
		corpus.DocumentCount++
		terms := tokenizeItemSimilarFulltext(fulltextContent(doc.Data))
		for token := range terms {
			corpus.DocumentFrequency[token]++
		}
		if doc.ParentItemKey == sourceKey {
			corpus.Source.Present = true
			if len(terms) > 0 {
				corpus.Source.Usable = true
				if corpus.Source.Rare == nil {
					corpus.Source.Rare = make(map[string]struct{})
				}
				for token := range terms {
					corpus.Source.Rare[token] = struct{}{}
				}
			}
		}
		return nil
	})
	if err != nil {
		return corpus, fmt.Errorf("querying fulltext rows: %w", err)
	}
	for token := range corpus.Source.Rare {
		if corpus.DocumentFrequency[token]*2 >= corpus.DocumentCount {
			delete(corpus.Source.Rare, token)
		}
	}
	return corpus, nil
}

func applyItemSimilarFulltextScores(db *store.Store, source itemSimilarRecord, candidates []itemSimilarRecord, corpus itemSimilarFulltextCorpus, entries []itemSimilarEntry) error {
	var parent string
	var fulltext itemSimilarFulltext
	candidateIndex := -1
	scoreParent := func() {
		if candidateIndex < 0 {
			return
		}
		entryIndex := sort.Search(len(entries), func(i int) bool { return entries[i].Key >= parent })
		if entryIndex == len(entries) || entries[entryIndex].Key != parent {
			return
		}
		candidate := candidates[candidateIndex]
		candidate.Fulltext = fulltext
		entries[entryIndex] = scoreItemSimilarCandidate(source, candidate)
	}

	err := db.VisitSimilarityFulltextDocuments(func(doc store.SimilarityFulltextDocument) error {
		if doc.ParentItemKey != parent {
			scoreParent()
			parent = doc.ParentItemKey
			candidateIndex = sort.Search(len(candidates), func(i int) bool { return candidates[i].Key >= parent })
			if parent == source.Key || candidateIndex == len(candidates) || candidates[candidateIndex].Key != parent {
				candidateIndex = -1
			}
			fulltext = itemSimilarFulltext{Present: candidateIndex >= 0}
		}
		if candidateIndex < 0 {
			return nil
		}
		terms := tokenizeItemSimilarFulltext(fulltextContent(doc.Data))
		if len(terms) > 0 {
			fulltext.Usable = true
		}
		for token := range terms {
			if corpus.DocumentFrequency[token]*2 < corpus.DocumentCount {
				if fulltext.Rare == nil {
					fulltext.Rare = make(map[string]struct{})
				}
				fulltext.Rare[token] = struct{}{}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("querying fulltext rows: %w", err)
	}
	scoreParent()
	return nil
}

func tokenizeItemSimilarFulltext(content string) map[string]struct{} {
	terms := make(map[string]struct{})
	var b strings.Builder
	flush := func() bool {
		if b.Len() >= 3 {
			terms[b.String()] = struct{}{}
		}
		b.Reset()
		return len(terms) >= itemSimilarMaxDistinctFulltextTerms
	}
	for _, r := range content {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		if flush() {
			return terms
		}
	}
	flush()
	return terms
}

func addItemSimilarSetValue(set map[string]string, value string) {
	display := strings.TrimSpace(value)
	key := normalizeItemSimilarToken(display)
	if key != "" {
		set[key] = display
	}
}

func normalizeItemSimilarCreator(first, last, name string) (string, string) {
	display := strings.TrimSpace(name)
	if display == "" {
		first = strings.TrimSpace(first)
		last = strings.TrimSpace(last)
		display = strings.TrimSpace(first + " " + last)
	}
	tokens := strings.Fields(strings.ToLower(display))
	sort.Strings(tokens)
	identity := strings.Join(tokens, " ")
	if identity == "" {
		return "", ""
	}
	return identity, display
}

func normalizeItemSimilarToken(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func itemSimilarSharedReason(singular, plural string, shared []string, source, candidate map[string]string) string {
	if len(source) == 0 {
		return "source has no " + plural
	}
	if len(candidate) == 0 {
		return "candidate has no " + plural
	}
	if len(shared) == 0 {
		return "no shared " + plural
	}
	label := plural
	if len(shared) == 1 {
		label = singular
	}
	return fmt.Sprintf("%d shared %s (%s)", len(shared), label, strings.Join(shared, ", "))
}

func itemSimilarVenueReason(score float64, source, candidate itemSimilarRecord) string {
	if source.Venue == "" {
		return "source has no publicationTitle"
	}
	if candidate.Venue == "" {
		return "candidate has no publicationTitle"
	}
	if score == 0 {
		return "different venue"
	}
	return fmt.Sprintf("same venue (%s)", source.VenueLabel)
}

func itemSimilarTextReason(score float64, shared int, source, candidate itemSimilarFulltext) string {
	if !source.Present {
		return "source has no synced fulltext"
	}
	if !candidate.Present {
		return "candidate has no synced fulltext"
	}
	if !source.Usable {
		return "source fulltext has no usable terms"
	}
	if !candidate.Usable {
		return "candidate fulltext has no usable terms"
	}
	if len(source.Rare) == 0 {
		return "source fulltext has no rare terms"
	}
	if len(candidate.Rare) == 0 {
		return "candidate fulltext has no rare terms"
	}
	if score == 0 {
		return "no rare-word text overlap"
	}
	return fmt.Sprintf("%.0f%% text overlap (%d shared rare terms)", score*100, shared)
}

func itemSimilarEntryReasons(entry itemSimilarEntry) []string {
	reasons := make([]string, 0, 5)
	if entry.Signals.Collections.Score > 0 {
		reasons = append(reasons, entry.Signals.Collections.Reason)
	}
	if entry.Signals.Tags.Score > 0 {
		reasons = append(reasons, entry.Signals.Tags.Reason)
	}
	if entry.Signals.Venue.Score > 0 {
		reasons = append(reasons, entry.Signals.Venue.Reason)
	}
	if entry.Signals.Text.Score > 0 || strings.Contains(entry.Signals.Text.Reason, "no synced fulltext") {
		reasons = append(reasons, entry.Signals.Text.Reason)
	}
	if entry.Signals.Creators.Score > 0 {
		reasons = append(reasons, entry.Signals.Creators.Reason)
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "low-confidence local similarity")
	}
	return reasons
}

func printItemSimilarReport(cmd *cobra.Command, report itemSimilarReport) error {
	if len(report.Similar) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No similar items found for %s.\n", report.Source.Key)
		return nil
	}
	tw := newTabWriter(cmd.OutOrStdout())
	fmt.Fprintln(tw, "RANK\tSCORE\tKEY\tTITLE\tWHY")
	for _, entry := range report.Similar {
		fmt.Fprintf(tw, "%d\t%.2f\t%s\t%s\t%s\n", entry.Rank, entry.Score, entry.Key, entry.Title, strings.Join(entry.Reasons, "; "))
	}
	return tw.Flush()
}
