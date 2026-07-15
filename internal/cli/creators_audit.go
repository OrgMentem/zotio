// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type creatorVariantTier string

const (
	creatorVariantTierExact     creatorVariantTier = "creator_variant_exact"
	creatorVariantTierInitials  creatorVariantTier = "creator_variant_initials"
	creatorVariantTierAmbiguous creatorVariantTier = "creator_variant_ambiguous"

	creatorAuditItemKeyCap                   = 10
	creatorAuditCommonSurnameMinDistinctLast = 20
	creatorAuditCommonSurnameMinItems        = 3
)

type creatorVariantAlias struct {
	Name      string   `json:"name"`
	ItemCount int      `json:"item_count"`
	ItemKeys  []string `json:"item_keys"`
}

type creatorVariantGroup struct {
	Tier               creatorVariantTier    `json:"tier"`
	Canonical          string                `json:"canonical"`
	CanonicalItemCount int                   `json:"canonical_item_count"`
	TotalItems         int                   `json:"total_items"`
	Aliases            []creatorVariantAlias `json:"aliases"`
	Evidence           map[string]any        `json:"evidence,omitempty"`

	members []*creatorNameVariant
}

type creatorsAuditSummary struct {
	Scope        string                     `json:"scope"`
	CreatorNames int                        `json:"creator_names"`
	ItemCount    int                        `json:"item_count"`
	GroupsByTier map[creatorVariantTier]int `json:"groups_by_tier"`
	ORCID        *creatorsAuditORCIDSummary `json:"orcid,omitempty"`
}

type creatorsAuditORCIDSummary struct {
	Enabled  bool `json:"enabled"`
	Lookups  int  `json:"lookups"`
	Captured int  `json:"captured"`
	Failed   int  `json:"failed"`
}

type creatorsAuditReport struct {
	Summary creatorsAuditSummary  `json:"summary"`
	Groups  []creatorVariantGroup `json:"groups"`
	FindingsReport
}

type creatorOccurrence struct {
	ItemKey      string
	Title        string
	DOI          string
	CreatorIndex int
	CreatorType  string
	FirstName    string
	LastName     string
	Name         string
	DisplayName  string
	NormFull     string
	NormFirst    string
	NormLast     string
	FirstTokens  []string
	NameHash     string
	ORCIDs       []creatorORCIDAttribution
}

type creatorORCIDAttribution struct {
	ORCID        string `json:"orcid"`
	Source       string `json:"source"`
	ItemKey      string `json:"item_key"`
	CreatorIndex int    `json:"creator_index"`
}

type creatorNameVariant struct {
	Name        string
	NormFull    string
	NormLast    string
	FirstTokens []string
	Occurrences []*creatorOccurrence
	itemKeys    map[string]bool
}

func newCreatorsAuditCmd(flags *rootFlags) *cobra.Command {
	var flagScope string
	var flagORCID bool

	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Audit synced creator names for variant candidates",
		Long: `Audit synced creator names for variant candidates without mutating Zotero.

The audit reads item creators from the local synced store and groups likely name
variants into three confidence tiers: exact-after-normalization, initial/full-name
compatibility, and ambiguous same-surname diagnostics. Use --orcid to fetch
CrossRef author ORCIDs for DOI-bearing tier-2/3 candidates and store them in the
local creator_orcids sidecar table as corroboration evidence. That sidecar is
local-only evidence; zotio never writes ORCIDs back to Zotero because Zotero has
no creator ORCID field.`,
		Example: `  zotio creators audit
  zotio creators audit --scope collection:ABCD1234
  zotio creators audit --orcid --json`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			report, ok, err := runCreatorsAudit(cmd.Context(), flags, flagScope, flagORCID)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			if flags.asJSON {
				return printCommandJSON(cmd.OutOrStdout(), report, flags)
			}
			return printCreatorsAuditReport(cmd, report)
		},
	}
	cmd.Flags().StringVar(&flagScope, "scope", "library", "Item scope: library, collection:<key>, tag:<tag>, item:<key>, or query:<text>")
	cmd.Flags().BoolVar(&flagORCID, "orcid", false, "Fetch CrossRef author ORCIDs into the local-only sidecar table; never writes ORCIDs to Zotero")
	cmd.AddCommand(newCreatorsAuditFixCmd(flags))
	return cmd
}

func runCreatorsAudit(ctx context.Context, flags *rootFlags, scopeExpr string, withORCID bool) (creatorsAuditReport, bool, error) {
	rawDB, err := openStoreForRead(ctx, "zotio")
	if err != nil {
		return creatorsAuditReport{}, false, fmt.Errorf("opening local database: %w", err)
	}
	if rawDB == nil {
		return creatorsAuditReport{}, false, nil
	}
	defer rawDB.Close()

	db := localQueryStore{rawDB}
	spec, err := parseScopeSpec(scopeExpr)
	if err != nil {
		return creatorsAuditReport{}, false, usageErr(err)
	}
	scope, err := resolveScope(db, spec)
	if err != nil {
		return creatorsAuditReport{}, false, err
	}
	if scope.Precondition != "" {
		return creatorsAuditReport{}, false, preconditionErr(fmt.Errorf("%s required for scope %s", scope.Precondition, scope.Expr))
	}

	occurrences, err := queryCreatorAuditOccurrences(db, scope)
	if err != nil {
		return creatorsAuditReport{}, false, fmt.Errorf("querying creators: %w", err)
	}

	groups := buildCreatorVariantGroups(occurrences)
	summary := creatorAuditSummary(scope.Expr, occurrences, groups)
	if withORCID {
		orcidSummary, err := captureCreatorAuditORCIDs(ctx, db, occurrences, groups, flags.timeout)
		if err != nil {
			return creatorsAuditReport{}, false, err
		}
		groups = buildCreatorVariantGroups(occurrences)
		summary = creatorAuditSummary(scope.Expr, occurrences, groups)
		summary.ORCID = &orcidSummary
	}

	_, syncedAt, _, err := rawDB.GetSyncState("items")
	if err != nil {
		return creatorsAuditReport{}, false, fmt.Errorf("querying sync state: %w", err)
	}
	source := FindingSource{Kind: "local"}
	if !syncedAt.IsZero() {
		source.SyncedAt = &syncedAt
	}
	report := creatorsAuditReport{
		Summary:        summary,
		Groups:         groups,
		FindingsReport: FindingsReport{Findings: creatorVariantFindings(groups, source)},
	}
	if report.Groups == nil {
		report.Groups = []creatorVariantGroup{}
	}
	if report.Findings == nil {
		report.Findings = []Finding{}
	}
	return report, true, nil
}

func queryCreatorAuditOccurrences(db localQueryStore, scope scopeResult) ([]*creatorOccurrence, error) {
	if !scope.All && len(scope.Keys) == 0 {
		return []*creatorOccurrence{}, nil
	}
	query := `
SELECT
	i.id AS item_key,
	COALESCE(TRIM(json_extract(i.data,'$.data.title')),'') AS title,
	COALESCE(TRIM(json_extract(i.data,'$.data.DOI')),'') AS doi,
	CAST(creator.key AS INTEGER) AS creator_index,
	COALESCE(TRIM(json_extract(creator.value,'$.creatorType')),'') AS creator_type,
	COALESCE(TRIM(json_extract(creator.value,'$.firstName')),'') AS first_name,
	COALESCE(TRIM(json_extract(creator.value,'$.lastName')),'') AS last_name,
	COALESCE(TRIM(json_extract(creator.value,'$.name')),'') AS name
FROM resources i, json_each(json_extract(i.data,'$.data.creators')) AS creator
WHERE i.resource_type='items'
	AND json_extract(i.data,'$.data.itemType') NOT IN ('attachment','note','annotation')`
	args := make([]any, 0, len(scope.Keys))
	if !scope.All {
		query += "\n\tAND i.id IN (" + placeholders(len(scope.Keys)) + ")"
		for _, key := range scope.Keys {
			args = append(args, key)
		}
	}
	query += "\nORDER BY i.id, creator_index"
	rows, err := db.QueryRaw(query, args...)
	if err != nil {
		return nil, err
	}
	occurrences := make([]*creatorOccurrence, 0, len(rows))
	for _, row := range rows {
		firstName := collapseCreatorWhitespace(sqlStringValue(row["first_name"]))
		lastName := collapseCreatorWhitespace(sqlStringValue(row["last_name"]))
		name := collapseCreatorWhitespace(sqlStringValue(row["name"]))
		display, parsedFirst, parsedLast := creatorDisplayParts(firstName, lastName, name)
		if display == "" {
			continue
		}
		normFirst := normalizeCreatorAuditText(parsedFirst)
		normLast := normalizeCreatorAuditText(parsedLast)
		normFull := normalizeCreatorAuditText(display)
		if normFull == "" {
			continue
		}
		occ := &creatorOccurrence{
			ItemKey:      sqlStringValue(row["item_key"]),
			Title:        sqlStringValue(row["title"]),
			DOI:          strings.TrimSpace(sqlStringValue(row["doi"])),
			CreatorIndex: sqlIntValue(row["creator_index"]),
			CreatorType:  sqlStringValue(row["creator_type"]),
			FirstName:    parsedFirst,
			LastName:     parsedLast,
			Name:         name,
			DisplayName:  display,
			NormFull:     normFull,
			NormFirst:    normFirst,
			NormLast:     normLast,
			FirstTokens:  creatorAuditTokens(normFirst),
			NameHash:     creatorAuditNameHash(normFull),
		}
		occurrences = append(occurrences, occ)
	}
	return occurrences, nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}

func creatorDisplayParts(firstName, lastName, name string) (display, first, last string) {
	firstName = collapseCreatorWhitespace(firstName)
	lastName = collapseCreatorWhitespace(lastName)
	name = collapseCreatorWhitespace(name)
	if lastName != "" {
		if firstName != "" {
			return collapseCreatorWhitespace(firstName + " " + lastName), firstName, lastName
		}
		return lastName, "", lastName
	}
	if name != "" {
		parsedFirst, parsedLast := parseCreatorSingleName(name)
		if parsedLast != "" {
			if parsedFirst != "" {
				return collapseCreatorWhitespace(parsedFirst + " " + parsedLast), parsedFirst, parsedLast
			}
			return parsedLast, "", parsedLast
		}
		return name, "", name
	}
	if firstName != "" {
		return firstName, firstName, ""
	}
	return "", "", ""
}

func parseCreatorSingleName(name string) (first, last string) {
	if before, after, ok := strings.Cut(name, ","); ok {
		last = collapseCreatorWhitespace(before)
		first = collapseCreatorWhitespace(after)
		return first, last
	}
	parts := strings.Fields(name)
	if len(parts) == 0 {
		return "", ""
	}
	if len(parts) == 1 {
		return "", parts[0]
	}
	return strings.Join(parts[:len(parts)-1], " "), parts[len(parts)-1]
}

func collapseCreatorWhitespace(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func normalizeCreatorAuditText(value string) string {
	value = norm.NFC.String(strings.TrimSpace(value))
	value = cases.Fold().String(value)
	var b strings.Builder
	lastSpace := true
	for _, r := range value {
		switch {
		case unicode.IsPunct(r) || unicode.IsSymbol(r):
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		case unicode.IsSpace(r):
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		default:
			b.WriteRune(r)
			lastSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}

func creatorAuditTokens(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Fields(value)
}

func creatorAuditNameHash(normFull string) string {
	sum := sha256.Sum256([]byte(normFull))
	return hex.EncodeToString(sum[:])
}

func buildCreatorVariantGroups(occurrences []*creatorOccurrence) []creatorVariantGroup {
	variants := aggregateCreatorVariants(occurrences)
	byLast := creatorVariantsByLastName(variants)
	commonLastNames := creatorCommonLastNames(occurrences)
	groups := make([]creatorVariantGroup, 0)

	for _, lastKey := range sortedCreatorLastKeys(byLast) {
		cluster := byLast[lastKey]
		if len(cluster) < 2 {
			continue
		}
		exactByNorm := make(map[string][]*creatorNameVariant)
		for _, variant := range cluster {
			exactByNorm[variant.NormFull] = append(exactByNorm[variant.NormFull], variant)
		}
		for _, exact := range exactByNorm {
			if len(exact) < 2 {
				continue
			}
			groups = append(groups, makeCreatorVariantGroup(creatorVariantTierExact, exact))
		}
		for _, component := range creatorVariantComponents(cluster, func(a, b *creatorNameVariant) bool {
			return a.NormFull != b.NormFull && creatorFirstNamesCompatible(a.FirstTokens, b.FirstTokens)
		}) {
			if len(component) < 2 {
				continue
			}
			tier := creatorVariantTierInitials
			if !creatorComponentPairwiseCompatible(component) {
				tier = creatorVariantTierAmbiguous
			}
			groups = append(groups, makeCreatorVariantGroup(tier, component))
		}
		for _, component := range creatorVariantComponents(cluster, func(a, b *creatorNameVariant) bool {
			return creatorVariantAmbiguousPair(a, b, commonLastNames[lastKey])
		}) {
			if len(component) < 2 {
				continue
			}
			if creatorComponentCoveredByTier(groups, creatorVariantTierAmbiguous, component) {
				continue
			}
			groups = append(groups, makeCreatorVariantGroup(creatorVariantTierAmbiguous, component))
		}
	}

	for i := range groups {
		applyCreatorORCIDCorroboration(&groups[i])
	}
	sortCreatorVariantGroups(groups)
	return groups
}

func aggregateCreatorVariants(occurrences []*creatorOccurrence) []*creatorNameVariant {
	byName := make(map[string]*creatorNameVariant)
	for _, occ := range occurrences {
		if occ.DisplayName == "" || occ.NormLast == "" {
			continue
		}
		key := occ.DisplayName
		variant := byName[key]
		if variant == nil {
			variant = &creatorNameVariant{
				Name:        occ.DisplayName,
				NormFull:    occ.NormFull,
				NormLast:    occ.NormLast,
				FirstTokens: occ.FirstTokens,
				itemKeys:    make(map[string]bool),
			}
			byName[key] = variant
		}
		variant.Occurrences = append(variant.Occurrences, occ)
		if occ.ItemKey != "" {
			variant.itemKeys[occ.ItemKey] = true
		}
	}
	variants := make([]*creatorNameVariant, 0, len(byName))
	for _, variant := range byName {
		variants = append(variants, variant)
	}
	return variants
}

func creatorVariantsByLastName(variants []*creatorNameVariant) map[string][]*creatorNameVariant {
	byLast := make(map[string][]*creatorNameVariant)
	for _, variant := range variants {
		if variant.NormLast == "" {
			continue
		}
		byLast[variant.NormLast] = append(byLast[variant.NormLast], variant)
	}
	for _, cluster := range byLast {
		sort.Slice(cluster, func(i, j int) bool { return cluster[i].Name < cluster[j].Name })
	}
	return byLast
}

func sortedCreatorLastKeys(byLast map[string][]*creatorNameVariant) []string {
	keys := make([]string, 0, len(byLast))
	for key := range byLast {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func creatorVariantComponents(cluster []*creatorNameVariant, related func(*creatorNameVariant, *creatorNameVariant) bool) [][]*creatorNameVariant {
	parent := make([]int, len(cluster))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}
	for i := range cluster {
		for j := i + 1; j < len(cluster); j++ {
			if related(cluster[i], cluster[j]) {
				union(i, j)
			}
		}
	}
	byRoot := make(map[int][]*creatorNameVariant)
	for i, variant := range cluster {
		root := find(i)
		byRoot[root] = append(byRoot[root], variant)
	}
	components := make([][]*creatorNameVariant, 0, len(byRoot))
	for _, component := range byRoot {
		sort.Slice(component, func(i, j int) bool { return component[i].Name < component[j].Name })
		components = append(components, component)
	}
	sort.Slice(components, func(i, j int) bool { return components[i][0].Name < components[j][0].Name })
	return components
}

func creatorComponentPairwiseCompatible(component []*creatorNameVariant) bool {
	for i := range component {
		for j := i + 1; j < len(component); j++ {
			if component[i].NormFull == component[j].NormFull {
				continue
			}
			if !creatorFirstNamesCompatible(component[i].FirstTokens, component[j].FirstTokens) {
				return false
			}
		}
	}
	return true
}

func creatorComponentCoveredByTier(groups []creatorVariantGroup, tier creatorVariantTier, component []*creatorNameVariant) bool {
	want := make(map[string]bool, len(component))
	for _, member := range component {
		want[member.Name] = true
	}
	for _, group := range groups {
		if group.Tier != tier || len(group.members) < len(component) {
			continue
		}
		have := make(map[string]bool, len(group.members))
		for _, member := range group.members {
			have[member.Name] = true
		}
		covered := true
		for name := range want {
			if !have[name] {
				covered = false
				break
			}
		}
		if covered {
			return true
		}
	}
	return false
}

func creatorFirstNamesCompatible(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}
	for i := range minLen {
		if !creatorFirstTokenCompatible(a[i], b[i]) {
			return false
		}
	}
	return true
}

func creatorFirstTokenCompatible(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if len([]rune(a)) == 1 && strings.HasPrefix(b, a) {
		return true
	}
	if len([]rune(b)) == 1 && strings.HasPrefix(a, b) {
		return true
	}
	return false
}

func creatorVariantAmbiguousPair(a, b *creatorNameVariant, commonLastName bool) bool {
	if a.NormFull == b.NormFull || creatorFirstNamesCompatible(a.FirstTokens, b.FirstTokens) {
		return false
	}
	if commonLastName {
		return true
	}
	if len(a.FirstTokens) == 0 || len(b.FirstTokens) == 0 {
		return true
	}
	return creatorFirstTokenInitial(a.FirstTokens[0]) == creatorFirstTokenInitial(b.FirstTokens[0])
}

func creatorFirstTokenInitial(token string) string {
	for _, r := range token {
		return string(r)
	}
	return ""
}

func creatorCommonLastNames(occurrences []*creatorOccurrence) map[string]bool {
	itemsByLast := make(map[string]map[string]bool)
	for _, occ := range occurrences {
		if occ.NormLast == "" || occ.ItemKey == "" {
			continue
		}
		if itemsByLast[occ.NormLast] == nil {
			itemsByLast[occ.NormLast] = make(map[string]bool)
		}
		itemsByLast[occ.NormLast][occ.ItemKey] = true
	}
	if len(itemsByLast) < creatorAuditCommonSurnameMinDistinctLast {
		return map[string]bool{}
	}
	counts := make([]int, 0, len(itemsByLast))
	for _, keys := range itemsByLast {
		counts = append(counts, len(keys))
	}
	sort.Ints(counts)
	idx := (len(counts)*95+99)/100 - 1
	if idx < 0 {
		idx = 0
	}
	threshold := counts[idx]
	if threshold < creatorAuditCommonSurnameMinItems {
		threshold = creatorAuditCommonSurnameMinItems
	}
	common := make(map[string]bool)
	for last, keys := range itemsByLast {
		if len(keys) >= threshold {
			common[last] = true
		}
	}
	return common
}

func makeCreatorVariantGroup(tier creatorVariantTier, members []*creatorNameVariant) creatorVariantGroup {
	members = append([]*creatorNameVariant(nil), members...)
	sort.Slice(members, func(i, j int) bool { return creatorVariantCanonicalLess(members[i], members[j]) })
	canonical := members[0]
	aliases := make([]creatorVariantAlias, 0, len(members)-1)
	allItemKeys := make(map[string]bool)
	for _, member := range members {
		for key := range member.itemKeys {
			allItemKeys[key] = true
		}
		if member == canonical {
			continue
		}
		aliases = append(aliases, creatorVariantAlias{
			Name:      member.Name,
			ItemCount: member.itemCount(),
			ItemKeys:  member.cappedItemKeys(creatorAuditItemKeyCap),
		})
	}
	return creatorVariantGroup{
		Tier:               tier,
		Canonical:          canonical.Name,
		CanonicalItemCount: canonical.itemCount(),
		TotalItems:         len(allItemKeys),
		Aliases:            aliases,
		Evidence: map[string]any{
			"item_key_cap": creatorAuditItemKeyCap,
		},
		members: members,
	}
}

func creatorVariantCanonicalLess(a, b *creatorNameVariant) bool {
	if a.itemCount() != b.itemCount() {
		return a.itemCount() > b.itemCount()
	}
	if creatorFullnessScore(a.Name) != creatorFullnessScore(b.Name) {
		return creatorFullnessScore(a.Name) > creatorFullnessScore(b.Name)
	}
	if creatorCaseQualityScore(a.Name) != creatorCaseQualityScore(b.Name) {
		return creatorCaseQualityScore(a.Name) > creatorCaseQualityScore(b.Name)
	}
	if creatorPunctuationCount(a.Name) != creatorPunctuationCount(b.Name) {
		return creatorPunctuationCount(a.Name) < creatorPunctuationCount(b.Name)
	}
	return a.Name < b.Name
}

func creatorFullnessScore(name string) int {
	score := 0
	for _, token := range strings.Fields(normalizeCreatorAuditText(name)) {
		runes := []rune(token)
		score += len(runes)
		if len(runes) > 1 {
			score += 10
		}
	}
	return score
}

func creatorCaseQualityScore(name string) int {
	score := 0
	for _, r := range name {
		if unicode.IsUpper(r) {
			score += 2
		}
		if unicode.IsLower(r) {
			score++
		}
	}
	return score
}

func creatorPunctuationCount(name string) int {
	count := 0
	for _, r := range name {
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			count++
		}
	}
	return count
}

func (v *creatorNameVariant) itemCount() int {
	return len(v.itemKeys)
}

func (v *creatorNameVariant) cappedItemKeys(limit int) []string {
	keys := make([]string, 0, len(v.itemKeys))
	for key := range v.itemKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if limit > 0 && len(keys) > limit {
		return keys[:limit]
	}
	return keys
}

func sortCreatorVariantGroups(groups []creatorVariantGroup) {
	sort.Slice(groups, func(i, j int) bool {
		if creatorTierRank(groups[i].Tier) != creatorTierRank(groups[j].Tier) {
			return creatorTierRank(groups[i].Tier) < creatorTierRank(groups[j].Tier)
		}
		if groups[i].TotalItems != groups[j].TotalItems {
			return groups[i].TotalItems > groups[j].TotalItems
		}
		return groups[i].Canonical < groups[j].Canonical
	})
}

func creatorTierRank(tier creatorVariantTier) int {
	switch tier {
	case creatorVariantTierExact:
		return 0
	case creatorVariantTierInitials:
		return 1
	case creatorVariantTierAmbiguous:
		return 2
	default:
		return 3
	}
}

func captureCreatorAuditORCIDs(ctx context.Context, db localQueryStore, occurrences []*creatorOccurrence, groups []creatorVariantGroup, timeout time.Duration) (creatorsAuditORCIDSummary, error) {
	summary := creatorsAuditORCIDSummary{Enabled: true}
	candidateItems := make(map[string]bool)
	for _, group := range groups {
		if group.Tier != creatorVariantTierInitials && group.Tier != creatorVariantTierAmbiguous {
			continue
		}
		for _, member := range group.members {
			for _, occ := range member.Occurrences {
				if occ.ItemKey != "" && occ.DOI != "" {
					candidateItems[occ.ItemKey] = true
				}
			}
		}
	}
	if len(candidateItems) == 0 {
		return summary, nil
	}
	occurrencesByItem := make(map[string][]*creatorOccurrence)
	doiByItem := make(map[string]string)
	for _, occ := range occurrences {
		if !candidateItems[occ.ItemKey] {
			continue
		}
		occurrencesByItem[occ.ItemKey] = append(occurrencesByItem[occ.ItemKey], occ)
		if doiByItem[occ.ItemKey] == "" && occ.DOI != "" {
			doiByItem[occ.ItemKey] = occ.DOI
		}
	}
	itemKeys := make([]string, 0, len(candidateItems))
	for key := range candidateItems {
		itemKeys = append(itemKeys, key)
	}
	sort.Strings(itemKeys)
	httpClient := &http.Client{Timeout: timeout}
	capturedAt := time.Now().UTC()
	for _, itemKey := range itemKeys {
		doi := doiByItem[itemKey]
		if doi == "" {
			continue
		}
		summary.Lookups++
		work, ok := fetchCrossRefWorkByDOI(ctx, httpClient, doi)
		if !ok {
			summary.Failed++
			continue
		}
		for _, match := range matchCrossRefCreatorORCIDs(work.Author, occurrencesByItem[itemKey]) {
			if err := upsertCreatorORCID(ctx, db, match, capturedAt); err != nil {
				return summary, err
			}
			match.occurrence.ORCIDs = append(match.occurrence.ORCIDs, creatorORCIDAttribution{
				ORCID:        match.orcid,
				Source:       match.source,
				ItemKey:      match.occurrence.ItemKey,
				CreatorIndex: match.occurrence.CreatorIndex,
			})
			summary.Captured++
		}
	}
	return summary, nil
}

type creatorORCIDMatch struct {
	occurrence *creatorOccurrence
	orcid      string
	source     string
}

func matchCrossRefCreatorORCIDs(authors []crossRefAuthor, occurrences []*creatorOccurrence) []creatorORCIDMatch {
	matches := make([]creatorORCIDMatch, 0)
	for _, occ := range occurrences {
		if occ.NormLast == "" {
			continue
		}
		candidateORCIDs := make(map[string]bool)
		for _, author := range authors {
			orcid := normalizeORCID(author.ORCID)
			if orcid == "" {
				continue
			}
			family := normalizeCreatorAuditText(author.Family)
			if family == "" || family != occ.NormLast {
				continue
			}
			providerFirst := creatorAuditTokens(normalizeCreatorAuditText(author.Given))
			if len(providerFirst) > 0 && len(occ.FirstTokens) > 0 && !creatorFirstNamesCompatible(providerFirst, occ.FirstTokens) {
				continue
			}
			candidateORCIDs[orcid] = true
		}
		if len(candidateORCIDs) != 1 {
			continue
		}
		for orcid := range candidateORCIDs {
			matches = append(matches, creatorORCIDMatch{occurrence: occ, orcid: orcid, source: "crossref"})
		}
	}
	return matches
}

func upsertCreatorORCID(ctx context.Context, db localQueryStore, match creatorORCIDMatch, capturedAt time.Time) error {
	_, err := db.DB().ExecContext(ctx, `
INSERT INTO creator_orcids(item_key, creator_index, name_hash, orcid, source, captured_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(item_key, creator_index, source) DO UPDATE SET
	name_hash=excluded.name_hash,
	orcid=excluded.orcid,
	captured_at=excluded.captured_at`,
		match.occurrence.ItemKey,
		match.occurrence.CreatorIndex,
		match.occurrence.NameHash,
		match.orcid,
		match.source,
		capturedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("persisting creator ORCID evidence: %w", err)
	}
	return nil
}

func normalizeORCID(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, "/")
	if value == "" {
		return ""
	}
	if idx := strings.LastIndex(value, "/"); idx >= 0 {
		value = value[idx+1:]
	}
	value = strings.ToUpper(value)
	parts := strings.Split(value, "-")
	if len(parts) != 4 {
		return ""
	}
	for i, part := range parts {
		if len(part) != 4 {
			return ""
		}
		for j, r := range part {
			if i == 3 && j == 3 && r == 'X' {
				continue
			}
			if r < '0' || r > '9' {
				return ""
			}
		}
	}
	return "https://orcid.org/" + strings.Join(parts, "-")
}

func applyCreatorORCIDCorroboration(group *creatorVariantGroup) {
	matches := make([]map[string]any, 0)
	conflicts := make([]map[string]any, 0)
	orcidsByName := make(map[string][]string)
	for _, member := range group.members {
		set := creatorVariantORCIDSet(member)
		if len(set) == 0 {
			continue
		}
		values := make([]string, 0, len(set))
		for orcid := range set {
			values = append(values, orcid)
		}
		sort.Strings(values)
		orcidsByName[member.Name] = values
	}
	for i := range group.members {
		left := group.members[i]
		leftSet := creatorVariantORCIDSet(left)
		if len(leftSet) == 0 {
			continue
		}
		for j := i + 1; j < len(group.members); j++ {
			right := group.members[j]
			rightSet := creatorVariantORCIDSet(right)
			if len(rightSet) == 0 {
				continue
			}
			common := creatorORCIDIntersection(leftSet, rightSet)
			if len(common) > 0 {
				for _, orcid := range common {
					matches = append(matches, map[string]any{"names": []string{left.Name, right.Name}, "orcid": orcid})
				}
				continue
			}
			conflicts = append(conflicts, map[string]any{
				"names":  []string{left.Name, right.Name},
				"orcids": []any{sortedCreatorORCIDs(leftSet), sortedCreatorORCIDs(rightSet)},
			})
		}
	}
	if len(orcidsByName) == 0 && len(matches) == 0 && len(conflicts) == 0 {
		return
	}
	if group.Evidence == nil {
		group.Evidence = make(map[string]any)
	}
	if len(orcidsByName) > 0 {
		group.Evidence["orcids"] = orcidsByName
	}
	if len(matches) > 0 {
		group.Evidence["orcid_matches"] = matches
	}
	if len(conflicts) > 0 {
		group.Evidence["orcid_conflicts"] = conflicts
		group.Tier = creatorVariantTierAmbiguous
		return
	}
	if len(matches) > 0 && group.Tier == creatorVariantTierAmbiguous {
		group.Tier = creatorVariantTierInitials
	}
}

func creatorVariantORCIDSet(member *creatorNameVariant) map[string]bool {
	set := make(map[string]bool)
	for _, occ := range member.Occurrences {
		for _, attribution := range occ.ORCIDs {
			if attribution.ORCID != "" {
				set[attribution.ORCID] = true
			}
		}
	}
	return set
}

func creatorORCIDIntersection(a, b map[string]bool) []string {
	values := make([]string, 0)
	for orcid := range a {
		if b[orcid] {
			values = append(values, orcid)
		}
	}
	sort.Strings(values)
	return values
}

func sortedCreatorORCIDs(set map[string]bool) []string {
	values := make([]string, 0, len(set))
	for orcid := range set {
		values = append(values, orcid)
	}
	sort.Strings(values)
	return values
}

func creatorAuditSummary(scopeExpr string, occurrences []*creatorOccurrence, groups []creatorVariantGroup) creatorsAuditSummary {
	nameSet := make(map[string]bool)
	itemSet := make(map[string]bool)
	for _, occ := range occurrences {
		if occ.DisplayName != "" {
			nameSet[occ.DisplayName] = true
		}
		if occ.ItemKey != "" {
			itemSet[occ.ItemKey] = true
		}
	}
	byTier := map[creatorVariantTier]int{
		creatorVariantTierExact:     0,
		creatorVariantTierInitials:  0,
		creatorVariantTierAmbiguous: 0,
	}
	for _, group := range groups {
		byTier[group.Tier]++
	}
	return creatorsAuditSummary{
		Scope:        scopeExpr,
		CreatorNames: len(nameSet),
		ItemCount:    len(itemSet),
		GroupsByTier: byTier,
	}
}

func creatorVariantFindings(groups []creatorVariantGroup, source FindingSource) []Finding {
	findings := make([]Finding, 0, len(groups))
	for _, group := range groups {
		finding := Finding{
			Kind:        string(group.Tier),
			Severity:    sevInfo,
			Title:       fmt.Sprintf("Creator variants for %s", group.Canonical),
			Evidence:    creatorVariantFindingEvidence(group),
			Source:      source,
			Autofixable: group.Tier == creatorVariantTierExact,
		}
		switch group.Tier {
		case creatorVariantTierExact:
			finding.RecommendedAction = &RecommendedAction{Text: "Exact-normalized creator variants are safe candidates for creators audit fix when that mutation surface is enabled."}
		case creatorVariantTierInitials:
			finding.RecommendedAction = &RecommendedAction{Command: "zotio creators audit fix --map <alias>=<canonical>"}
		case creatorVariantTierAmbiguous:
			finding.RecommendedAction = &RecommendedAction{Text: "Review manually; ORCID conflicts or ambiguous same-surname evidence should not be auto-merged."}
		}
		findings = append(findings, finding)
	}
	return findings
}

func creatorVariantFindingEvidence(group creatorVariantGroup) map[string]any {
	aliases := make([]map[string]any, 0, len(group.Aliases))
	for _, alias := range group.Aliases {
		aliases = append(aliases, map[string]any{
			"name":       alias.Name,
			"item_count": alias.ItemCount,
			"item_keys":  alias.ItemKeys,
		})
	}
	evidence := map[string]any{
		"tier":                 string(group.Tier),
		"canonical":            group.Canonical,
		"canonical_item_count": group.CanonicalItemCount,
		"aliases":              aliases,
		"total_items":          group.TotalItems,
	}
	for key, value := range group.Evidence {
		evidence[key] = value
	}
	return evidence
}

func printCreatorsAuditReport(cmd *cobra.Command, report creatorsAuditReport) error {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "## Creator audit")
	fmt.Fprintf(out, "Scope: %s\n", report.Summary.Scope)
	fmt.Fprintf(out, "Creators: %d distinct displayed name(s) across %d item(s)\n", report.Summary.CreatorNames, report.Summary.ItemCount)
	if report.Summary.ORCID != nil {
		fmt.Fprintf(out, "ORCID: %d CrossRef lookup(s), %d sidecar row(s) captured, %d failed lookup(s). Rows are local-only and are never written to Zotero.\n", report.Summary.ORCID.Lookups, report.Summary.ORCID.Captured, report.Summary.ORCID.Failed)
	}
	if len(report.Groups) == 0 {
		fmt.Fprintln(out, "No creator variant groups found.")
		return nil
	}
	for _, tier := range []creatorVariantTier{creatorVariantTierExact, creatorVariantTierInitials, creatorVariantTierAmbiguous} {
		groups := groupsForCreatorTier(report.Groups, tier)
		fmt.Fprintf(out, "\n### %s (%d)\n", tier, len(groups))
		if len(groups) == 0 {
			fmt.Fprintln(out, "None.")
			continue
		}
		for _, group := range groups {
			fmt.Fprintf(out, "- %s (%d item(s); %d total with aliases)\n", group.Canonical, group.CanonicalItemCount, group.TotalItems)
			for _, alias := range group.Aliases {
				keys := strings.Join(alias.ItemKeys, ", ")
				if keys == "" {
					keys = "no item keys"
				}
				fmt.Fprintf(out, "  - %s (%d item(s): %s)\n", alias.Name, alias.ItemCount, keys)
			}
			if len(group.Evidence) > 0 {
				if matches, ok := group.Evidence["orcid_matches"]; ok {
					data, _ := json.Marshal(matches)
					fmt.Fprintf(out, "  ORCID matches: %s\n", data)
				}
				if conflicts, ok := group.Evidence["orcid_conflicts"]; ok {
					data, _ := json.Marshal(conflicts)
					fmt.Fprintf(out, "  ORCID conflicts: %s\n", data)
				}
			}
		}
	}
	return nil
}

func groupsForCreatorTier(groups []creatorVariantGroup, tier creatorVariantTier) []creatorVariantGroup {
	out := make([]creatorVariantGroup, 0)
	for _, group := range groups {
		if group.Tier == tier {
			out = append(out, group)
		}
	}
	return out
}
