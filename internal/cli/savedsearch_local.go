// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
// local evaluation of synced saved-search condition sets (ADR-0002).

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"zotio/internal/store"
)

// Saved-search local evaluation.
//
// Sync mirrors a saved search's DEFINITION (resource "searches", with
// data.conditions[] of {condition, operator, value}); Zotero alone evaluates
// it. This file replays an explicit, narrow subset of Zotero's search.js
// semantics against the mirrored items so a --scope saved-search:KEY cohort
// still resolves while the desktop local API is unavailable.
//
// Live stays authoritative. resolveScopeLive asks Zotero first and reaches
// this file only when the desktop cannot answer, because Zotero evaluates
// every condition type and this evaluator does not. Local evaluation is an
// offline fallback, never a partial one: a definition that carries any
// condition, operator, or value outside the supported subset is refused whole
// with the offending conditions named, so a cohort is either exactly what
// Zotero would return for the mirrored data or absent.
//
// Supported subset (everything else is named and refused):
//
//	conditions  title, publicationTitle, shortTitle, abstractNote, publisher,
//	            place, volume, number, pages, issue, DOI, ISBN, ISSN, url,
//	            extra, language, series, seriesTitle, edition, libraryCatalog,
//	            callNumber, archive, archiveLocation, rights
//	              is, isNot, contains, doesNotContain, isEmpty, isNotEmpty
//	              (+ beginsWith, except on pages and issue)
//	            creator, author, editor, bookAuthor
//	              is, isNot, contains, doesNotContain, isEmpty, isNotEmpty
//	            tag             is, isNot, contains, doesNotContain
//	            collection      is, isNot (the value is a collection key)
//	            itemType        is, isNot
//	            date, dateAdded, dateModified
//	                            is, isNot, isBefore, isAfter with a YYYY,
//	                            YYYY-MM, or YYYY-MM-DD value (dateAdded and
//	                            dateModified also take YYYY-MM-DD HH:MM:SS)
//	flags       joinMode any|all, noChildren, includeParentsAndChildren,
//	            includeParents, includeChildren, recursive (collection only)
//
// Not supported, by design: fulltextContent, note, anyField,
// quicksearch-*, savedSearch (nesting), unfiled, deleted, includeDeleted,
// libraryID, year, isInTheLast, isLessThan/isGreaterThan, condition groups
// (groupStart/groupEnd), resultLevel, and every other condition name.
//
// Semantics replayed from Zotero's search.js:
//   - Every condition selects the items that HAVE a matching value; a
//     negative operator (isNot, doesNotContain, isEmpty) selects the
//     complement, so an item with no value at all matches doesNotContain.
//     Annotations never match a negation.
//   - Text matching is case-, accent-, and quote/dash-insensitive, using
//     Zotero's normalizeForSearch. Matching is a substring test for
//     contains, a prefix test for beginsWith, equality for is.
//   - joinMode defaults to all; the first top-level joinMode wins.
//   - Trashed items, and children of trashed items, never match.
//   - noChildren drops every child item. The include* flags add the
//     parents and/or child notes and attachments of what each condition
//     matched, before the condition is negated or combined.
//   - Children (attachments, notes) can match on their own fields and tags,
//     so a raw result holds them. Scope consumers keep top-level items only,
//     matching the live arm (see resolveScopeLive).

// savedSearchCondition is one {condition, operator, value} entry of a saved
// search definition.
type savedSearchCondition struct {
	Condition string          `json:"condition"`
	Operator  string          `json:"operator"`
	Value     json.RawMessage `json:"value"`
}

// text returns the condition value as a string: a JSON string verbatim, null
// or absent as empty, any other JSON value as its literal text.
func (c savedSearchCondition) text() string {
	raw := strings.TrimSpace(string(c.Value))
	if raw == "" || raw == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(c.Value, &s); err == nil {
		return s
	}
	return raw
}

// savedSearchUnsupportedError reports that a definition (or the mirrored data
// it would run against) falls outside the supported subset. Reasons name each
// offending condition so the operator knows exactly what stopped local
// evaluation.
type savedSearchUnsupportedError struct {
	Reasons []string
}

func (e *savedSearchUnsupportedError) Error() string {
	return "the local evaluator does not support: " + strings.Join(e.Reasons, "; ")
}

// errSavedSearchNotMirrored means the saved-search definition is not in the
// local mirror.
var errSavedSearchNotMirrored = errors.New("saved search is not in the local mirror")

// errSavedSearchItemsNotSynced means no `items` sync has completed for this
// mirror, so an evaluation would run against an empty or partial item list
// and present it as a real (empty) cohort.
var errSavedSearchItemsNotSynced = errors.New("the local mirror has no completed items sync; run 'zotio sync'")

// savedSearchLocalResult is a locally evaluated saved-search cohort.
type savedSearchLocalResult struct {
	// Keys holds every matched item key, sorted. Children (attachments,
	// notes, annotations) appear here exactly as Zotero's search returns them.
	Keys []string
	// TopLevelKeys is Keys minus child items, sorted.
	TopLevelKeys []string
	// Items holds the mirrored payload of every matched item, in the order
	// the item list endpoints use: dateModified descending, key ascending.
	Items []json.RawMessage
}

// evaluateSavedSearchLocal reads the mirrored definition of searchKey and
// evaluates it against the mirrored items. It returns errSavedSearchNotMirrored
// when the definition is absent and a *savedSearchUnsupportedError when the
// definition cannot be reproduced.
func evaluateSavedSearchLocal(ctx context.Context, db *store.Store, searchKey string) (savedSearchLocalResult, error) {
	definition, err := db.GetContext(ctx, "searches", searchKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return savedSearchLocalResult{}, fmt.Errorf("%w: %s", errSavedSearchNotMirrored, searchKey)
		}
		return savedSearchLocalResult{}, fmt.Errorf("reading saved search %s from the local mirror: %w", searchKey, err)
	}
	return evaluateSavedSearchDefinition(ctx, db, definition)
}

// evaluateSavedSearchDefinition evaluates a saved-search definition payload
// (the shape the API and the mirror both use) against the mirrored items.
func evaluateSavedSearchDefinition(ctx context.Context, db *store.Store, definition []byte) (savedSearchLocalResult, error) {
	ready, reason, stateErr := importItemsMirrorReady(ctx, db)
	if stateErr != nil {
		return savedSearchLocalResult{}, stateErr
	}
	if !ready {
		return savedSearchLocalResult{}, fmt.Errorf("%w (%s)", errSavedSearchItemsNotSynced, reason)
	}
	var row struct {
		Conditions *[]savedSearchCondition `json:"conditions"`
		Data       struct {
			Conditions *[]savedSearchCondition `json:"conditions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(definition, &row); err != nil {
		return savedSearchLocalResult{}, fmt.Errorf("decoding saved search definition: %w", err)
	}
	conditions := row.Data.Conditions
	if conditions == nil {
		conditions = row.Conditions
	}
	if conditions == nil {
		return savedSearchLocalResult{}, &savedSearchUnsupportedError{Reasons: []string{"the definition carries no conditions array"}}
	}
	plan := compileSavedSearch(*conditions)
	if len(plan.unsupported) > 0 {
		return savedSearchLocalResult{}, &savedSearchUnsupportedError{Reasons: plan.unsupported}
	}
	env, err := loadSavedSearchEnv(ctx, db)
	if err != nil {
		return savedSearchLocalResult{}, err
	}
	return plan.evaluate(env)
}

// --- the mirrored data the evaluator reads ---

type savedSearchCreator struct {
	creatorType string
	name        string // TRIM(firstName || ' ' || lastName), single-field names as lastName
}

type savedSearchItem struct {
	key          string
	parent       string
	itemType     string
	fields       map[string]string // non-empty string fields of data
	creators     []savedSearchCreator
	tags         []string
	collections  []string
	dateAdded    time.Time
	dateModified time.Time
	raw          json.RawMessage
}

// isChild reports whether the item hangs under a parent.
func (it *savedSearchItem) isChild() bool { return it.parent != "" }

// canHaveParentLink reports whether Zotero's parent/child expansion crosses
// this item: only child attachments and notes do (annotations are skipped).
func (it *savedSearchItem) canHaveParentLink() bool {
	return it.parent != "" && (it.itemType == "attachment" || it.itemType == "note")
}

type savedSearchEnv struct {
	// items is the universe a search runs over: every mirrored item that is
	// neither trashed nor under a trashed ancestor.
	items []*savedSearchItem
	byKey map[string]*savedSearchItem
	// collectionParent maps each mirrored collection key to its parent
	// collection key ("" for a top-level collection).
	collectionParent map[string]string
}

func loadSavedSearchEnv(ctx context.Context, db *store.Store) (*savedSearchEnv, error) {
	rawItems, err := db.ListContext(ctx, "items", 0)
	if err != nil {
		return nil, fmt.Errorf("reading mirrored items: %w", err)
	}
	rawTrash, err := db.ListContext(ctx, "items-trash", 0)
	if err != nil {
		return nil, fmt.Errorf("reading mirrored trash: %w", err)
	}
	rawCollections, err := db.ListContext(ctx, "collections", 0)
	if err != nil {
		return nil, fmt.Errorf("reading mirrored collections: %w", err)
	}

	trashed := make(map[string]bool, len(rawTrash))
	for _, raw := range rawTrash {
		var row struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(raw, &row); err == nil && row.Key != "" {
			trashed[row.Key] = true
		}
	}

	all := make(map[string]*savedSearchItem, len(rawItems))
	order := make([]*savedSearchItem, 0, len(rawItems))
	for _, raw := range rawItems {
		it, ok := decodeSavedSearchItem(raw)
		if !ok {
			continue
		}
		if _, dup := all[it.key]; dup {
			continue
		}
		all[it.key] = it
		order = append(order, it)
	}

	env := &savedSearchEnv{
		items:            make([]*savedSearchItem, 0, len(order)),
		byKey:            make(map[string]*savedSearchItem, len(order)),
		collectionParent: make(map[string]string, len(rawCollections)),
	}
	for _, it := range order {
		if savedSearchTrashed(it, all, trashed) {
			continue
		}
		env.items = append(env.items, it)
		env.byKey[it.key] = it
	}

	for _, raw := range rawCollections {
		var row struct {
			Key  string `json:"key"`
			Data struct {
				ParentCollection any `json:"parentCollection"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &row); err != nil || row.Key == "" {
			continue
		}
		parent, _ := row.Data.ParentCollection.(string)
		env.collectionParent[row.Key] = parent
	}
	return env, nil
}

// savedSearchTrashed reports whether the item, or any ancestor, is in the
// trash. Zotero hides a trashed item's children together with it.
func savedSearchTrashed(it *savedSearchItem, all map[string]*savedSearchItem, trashed map[string]bool) bool {
	for depth := 0; it != nil && depth < 4; depth++ {
		if trashed[it.key] || (it.parent != "" && trashed[it.parent]) {
			return true
		}
		if it.parent == "" {
			return false
		}
		it = all[it.parent]
	}
	return false
}

func decodeSavedSearchItem(raw json.RawMessage) (*savedSearchItem, bool) {
	var row struct {
		Key  string         `json:"key"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, false
	}
	key := row.Key
	if key == "" {
		key, _ = row.Data["key"].(string)
	}
	if key == "" {
		return nil, false
	}
	it := &savedSearchItem{key: key, raw: raw, fields: make(map[string]string, len(row.Data))}
	for name, value := range row.Data {
		if s, ok := value.(string); ok && s != "" {
			it.fields[name] = s
		}
	}
	it.itemType = it.fields["itemType"]
	it.parent = it.fields["parentItem"]
	if list, ok := row.Data["creators"].([]any); ok {
		for _, entry := range list {
			m, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			first, _ := m["firstName"].(string)
			last, _ := m["lastName"].(string)
			if single, _ := m["name"].(string); single != "" && first == "" && last == "" {
				last = single
			}
			kind, _ := m["creatorType"].(string)
			it.creators = append(it.creators, savedSearchCreator{
				creatorType: kind,
				name:        strings.Trim(first+" "+last, " "),
			})
		}
	}
	if list, ok := row.Data["tags"].([]any); ok {
		for _, entry := range list {
			if m, ok := entry.(map[string]any); ok {
				if tag, _ := m["tag"].(string); tag != "" {
					it.tags = append(it.tags, tag)
				}
			}
		}
	}
	if list, ok := row.Data["collections"].([]any); ok {
		for _, entry := range list {
			if c, ok := entry.(string); ok && c != "" {
				it.collections = append(it.collections, c)
			}
		}
	}
	it.dateAdded, _ = time.Parse(time.RFC3339, it.fields["dateAdded"])
	it.dateModified, _ = time.Parse(time.RFC3339, it.fields["dateModified"])
	return it, true
}

// --- compilation ---

// savedSearchMatcher is one compiled primary condition. matches reports
// whether an item HAS a value satisfying the condition; negate flips the
// selection to the complement (Zotero's NOT IN).
type savedSearchMatcher struct {
	label  string
	negate bool
	// prepare binds data-dependent state (collection tree, date parsing) and
	// returns the reasons the mirrored data cannot be evaluated; nil when
	// ready. It may be nil.
	prepare func(env *savedSearchEnv) []string
	matches func(it *savedSearchItem) bool
}

type savedSearchPlan struct {
	any                bool
	noChildren         bool
	parentsAndChildren bool
	parents            bool
	children           bool
	recursive          bool
	matchers           []savedSearchMatcher
	unsupported        []string
}

func (p *savedSearchPlan) refuse(format string, args ...any) {
	reason := fmt.Sprintf(format, args...)
	for _, existing := range p.unsupported {
		if existing == reason {
			return
		}
	}
	p.unsupported = append(p.unsupported, reason)
}

// savedSearchTextFields maps a Zotero text-field condition to the item data
// fields it reads: the field itself plus the type-specific fields that map to
// it as a base field (Zotero schema baseField).
var savedSearchTextFields = map[string][]string{
	"title":            {"title", "caseName", "nameOfAct", "subject"},
	"publicationTitle": {"publicationTitle", "blogTitle", "bookTitle", "dictionaryTitle", "encyclopediaTitle", "forumTitle", "proceedingsTitle", "programTitle", "sessionTitle", "websiteTitle"},
	"publisher":        {"publisher", "company", "distributor", "institution", "label", "network", "repository", "studio", "university"},
	"place":            {"place", "repositoryLocation"},
	"volume":           {"volume", "codeVolume", "reporterVolume"},
	"number":           {"number", "archiveID", "billNumber", "docketNumber", "documentNumber", "episodeNumber", "identifier", "patentNumber", "publicLawNumber", "reportNumber"},
	"shortTitle":       {"shortTitle"},
	"abstractNote":     {"abstractNote"},
	"DOI":              {"DOI"},
	"ISBN":             {"ISBN"},
	"ISSN":             {"ISSN"},
	"url":              {"url"},
	"extra":            {"extra"},
	"language":         {"language"},
	"series":           {"series"},
	"seriesTitle":      {"seriesTitle"},
	"edition":          {"edition"},
	"libraryCatalog":   {"libraryCatalog"},
	"callNumber":       {"callNumber"},
	"archive":          {"archive"},
	"archiveLocation":  {"archiveLocation"},
	"rights":           {"rights"},
}

// savedSearchNumberFields are Zotero's "numberfield" conditions. They match
// without the accent folding text fields get and take no beginsWith.
var savedSearchNumberFields = map[string][]string{
	"pages": {"pages", "codePages", "firstPage"},
	"issue": {"issue"},
}

var savedSearchTextOps = map[string]bool{
	"is": true, "isNot": true, "contains": true, "doesNotContain": true,
	"beginsWith": true, "isEmpty": true, "isNotEmpty": true,
}

var savedSearchCreatorOps = map[string]bool{
	"is": true, "isNot": true, "contains": true, "doesNotContain": true,
	"isEmpty": true, "isNotEmpty": true,
}

var savedSearchTagOps = map[string]bool{
	"is": true, "isNot": true, "contains": true, "doesNotContain": true,
}

var savedSearchIsOps = map[string]bool{"is": true, "isNot": true}

var savedSearchDateOps = map[string]bool{
	"is": true, "isNot": true, "isBefore": true, "isAfter": true,
}

func savedSearchNegative(op string) bool {
	return op == "isNot" || op == "doesNotContain" || op == "isEmpty"
}

func compileSavedSearch(conditions []savedSearchCondition) *savedSearchPlan {
	p := &savedSearchPlan{}
	sawJoinMode := false
	for _, c := range conditions {
		name, op, value := c.Condition, c.Operator, c.text()
		switch name {
		case "joinMode":
			if op != "any" && op != "all" {
				p.refuse("joinMode: operator %q is not any or all", op)
				continue
			}
			if !sawJoinMode {
				p.any = op == "any"
				sawJoinMode = true
			}
		case "noChildren", "includeParentsAndChildren", "includeParents", "includeChildren", "recursive":
			if op != "true" && op != "false" {
				p.refuse("%s: operator %q is not true or false", name, op)
				continue
			}
			on := op == "true"
			switch name {
			case "noChildren":
				p.noChildren = on
			case "includeParentsAndChildren":
				p.parentsAndChildren = on
			case "includeParents":
				p.parents = on
			case "includeChildren":
				p.children = on
			case "recursive":
				p.recursive = on
			}
		case "creator", "author", "editor", "bookAuthor":
			p.addCreator(name, op, value)
		case "tag":
			p.addTag(op, value)
		case "collection":
			p.addCollection(op, value)
		case "itemType":
			p.addItemType(op, value)
		case "date", "dateAdded", "dateModified":
			p.addDate(name, op, value)
		default:
			if fields, ok := savedSearchTextFields[name]; ok {
				p.addText(name, fields, false, op, value)
			} else if fields, ok := savedSearchNumberFields[name]; ok {
				p.addText(name, fields, true, op, value)
			} else {
				p.refuse("%s: condition is not supported", name)
			}
		}
	}
	// Zotero treats a search without a primary condition as every item. A
	// definition like that is almost always malformed, and a silent
	// whole-library cohort is the worst failure a scope can have, so refuse.
	if len(p.unsupported) == 0 && len(p.matchers) == 0 {
		p.refuse("the definition has no match conditions, which Zotero would treat as every item")
	}
	return p
}

// savedSearchNormalize is Zotero.Utilities.Internal.normalizeForSearch: strip
// the supported formatting tags, NFKD-decompose and drop combining marks,
// lower-case, fold a few non-decomposing letters, fold typographic quotes and
// dashes to ASCII, and recompose.
var savedSearchFormattingTags = regexp.MustCompile(`</?(?:i|b|sub|sup)>|<span (?:style="font-variant:small-caps;"|class="nocase")>|</span>`)

var savedSearchFolds = strings.NewReplacer(
	"ø", "o", "œ", "oe", "æ", "ae", "ł", "l", "đ", "d", "ð", "d", "þ", "th",
	"ß", "ss", "ı", "i", "\u2044", "/",
	"\u2018", "'", "\u2019", "'", "\u201a", "'", "\u201b", "'", "\u2032", "'",
	"\u201c", `"`, "\u201d", `"`, "\u201e", `"`, "\u201f", `"`, "\u2033", `"`,
	"\u2010", "-", "\u2011", "-", "\u2012", "-", "\u2013", "-", "\u2014", "-", "\u2015", "-", "\u2212", "-",
)

func savedSearchNormalize(s string) string {
	if s == "" {
		return s
	}
	if strings.Contains(s, "<") {
		s = savedSearchFormattingTags.ReplaceAllString(s, "")
	}
	decomposed := norm.NFKD.String(s)
	var b strings.Builder
	b.Grow(len(decomposed))
	for _, r := range decomposed {
		if r >= 0x0300 && r <= 0x036f {
			continue
		}
		b.WriteRune(r)
	}
	return norm.NFC.String(savedSearchFolds.Replace(strings.ToLower(b.String())))
}

// savedSearchASCIILower is SQLite's case folding, which number fields use.
func savedSearchASCIILower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// savedSearchHasWildcard reports a needle that reaches a SQL LIKE in Zotero:
// % and _ are wildcards there, which this evaluator does not replay.
func savedSearchHasWildcard(op, needle string) bool {
	switch op {
	case "contains", "doesNotContain", "beginsWith":
		return strings.ContainsAny(needle, "%_")
	}
	return false
}

// textMatches applies a text operator to an item's candidate values, already
// folded the same way as needle. It reports whether the item HAS a matching
// value (isEmpty/isNotEmpty: any value at all).
func savedSearchTextMatches(op string, values []string, needle string) bool {
	for _, v := range values {
		switch op {
		case "contains", "doesNotContain":
			if strings.Contains(v, needle) {
				return true
			}
		case "is", "isNot":
			if v == needle {
				return true
			}
		case "beginsWith":
			if strings.HasPrefix(v, needle) {
				return true
			}
		case "isEmpty", "isNotEmpty":
			return true
		}
	}
	return false
}

func (p *savedSearchPlan) addText(name string, fields []string, numeric bool, op, value string) {
	allowed := savedSearchTextOps[op]
	if numeric && op == "beginsWith" {
		allowed = false
	}
	if !allowed {
		p.refuse("%s: operator %q is not supported", name, op)
		return
	}
	if savedSearchHasWildcard(op, value) {
		p.refuse("%s: value %q contains a LIKE wildcard (%% or _)", name, value)
		return
	}
	fold := savedSearchNormalize
	if numeric {
		fold = savedSearchASCIILower
	}
	needle := fold(value)
	p.matchers = append(p.matchers, savedSearchMatcher{
		label:  name + " " + op,
		negate: savedSearchNegative(op),
		matches: func(it *savedSearchItem) bool {
			var values []string
			for _, f := range fields {
				if v, ok := it.fields[f]; ok {
					values = append(values, fold(v))
				}
			}
			return savedSearchTextMatches(op, values, needle)
		},
	})
}

func (p *savedSearchPlan) addCreator(name, op, value string) {
	if !savedSearchCreatorOps[op] {
		p.refuse("%s: operator %q is not supported", name, op)
		return
	}
	if savedSearchHasWildcard(op, value) {
		p.refuse("%s: value %q contains a LIKE wildcard (%% or _)", name, value)
		return
	}
	needle := savedSearchNormalize(value)
	p.matchers = append(p.matchers, savedSearchMatcher{
		label:  name + " " + op,
		negate: savedSearchNegative(op),
		matches: func(it *savedSearchItem) bool {
			var values []string
			for _, c := range it.creators {
				if name != "creator" && c.creatorType != name {
					continue
				}
				values = append(values, savedSearchNormalize(c.name))
			}
			return savedSearchTextMatches(op, values, needle)
		},
	})
}

func (p *savedSearchPlan) addTag(op, value string) {
	if !savedSearchTagOps[op] {
		p.refuse("tag: operator %q is not supported", op)
		return
	}
	if savedSearchHasWildcard(op, value) {
		p.refuse("tag: value %q contains a LIKE wildcard (%% or _)", value)
		return
	}
	needle := savedSearchNormalize(value)
	p.matchers = append(p.matchers, savedSearchMatcher{
		label:  "tag " + op,
		negate: savedSearchNegative(op),
		matches: func(it *savedSearchItem) bool {
			values := make([]string, len(it.tags))
			for i, tag := range it.tags {
				values[i] = savedSearchNormalize(tag)
			}
			return savedSearchTextMatches(op, values, needle)
		},
	})
}

func (p *savedSearchPlan) addItemType(op, value string) {
	if !savedSearchIsOps[op] {
		p.refuse("itemType: operator %q is not supported", op)
		return
	}
	p.matchers = append(p.matchers, savedSearchMatcher{
		label:   "itemType " + op,
		negate:  op == "isNot",
		matches: func(it *savedSearchItem) bool { return strings.EqualFold(it.itemType, value) },
	})
}

func (p *savedSearchPlan) addCollection(op, value string) {
	if !savedSearchIsOps[op] {
		p.refuse("collection: operator %q is not supported", op)
		return
	}
	// recursive is a flag that may follow the condition, so the descendant
	// set is built in prepare, after every condition has been read.
	var members map[string]bool
	p.matchers = append(p.matchers, savedSearchMatcher{
		label:  "collection " + op,
		negate: op == "isNot",
		prepare: func(env *savedSearchEnv) []string {
			if _, ok := env.collectionParent[value]; !ok {
				return []string{fmt.Sprintf("collection: key %q is not in the local mirror (Zotero would match nothing, which a stale mirror cannot confirm)", value)}
			}
			members = map[string]bool{value: true}
			if p.recursive {
				for changed := true; changed; {
					changed = false
					for key, parent := range env.collectionParent {
						if !members[key] && members[parent] {
							members[key] = true
							changed = true
						}
					}
				}
			}
			return nil
		},
		matches: func(it *savedSearchItem) bool {
			for _, c := range it.collections {
				if members[c] {
					return true
				}
			}
			return false
		},
	})
}

var (
	savedSearchDateOnly     = regexp.MustCompile(`^(\d{4})(?:-(\d{2})(?:-(\d{2}))?)?$`)
	savedSearchSQLDateTime  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$`)
	savedSearchItemISO      = regexp.MustCompile(`^(\d{4})(?:-(\d{1,2})(?:-(\d{1,2}))?)?$`)
	savedSearchItemMDY      = regexp.MustCompile(`^([A-Za-z]{3,9})\.? (\d{1,2}),? (\d{4})$`)
	savedSearchItemDMY      = regexp.MustCompile(`^(\d{1,2}) ([A-Za-z]{3,9})\.?,? (\d{4})$`)
	savedSearchItemMY       = regexp.MustCompile(`^([A-Za-z]{3,9})\.?,? (\d{4})$`)
	savedSearchMonthNumbers = map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}
)

// savedSearchMonth resolves an English month name or abbreviation.
func savedSearchMonth(name string) (int, bool) {
	name = strings.ToLower(name)
	if len(name) < 3 {
		return 0, false
	}
	month, ok := savedSearchMonthNumbers[name[:3]]
	if !ok {
		return 0, false
	}
	full := []string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"}[month-1]
	if name == name[:3] || name == "sept" || strings.HasPrefix(full, name) {
		return month, true
	}
	return 0, false
}

// savedSearchItemDate converts a freeform item date to Zotero's stored
// "YYYY-MM-DD" prefix (00 for an unknown month or day). The second result is
// false when the text is not one of the formats this evaluator parses.
func savedSearchItemDate(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	build := func(y string, m, d int) (string, bool) {
		if m < 0 || m > 12 || d < 0 || d > 31 {
			return "", false
		}
		return fmt.Sprintf("%s-%02d-%02d", y, m, d), true
	}
	atoi := func(v string) int {
		n := 0
		for _, r := range v {
			n = n*10 + int(r-'0')
		}
		return n
	}
	if m := savedSearchItemISO.FindStringSubmatch(s); m != nil {
		return build(m[1], atoi(m[2]), atoi(m[3]))
	}
	if m := savedSearchItemMDY.FindStringSubmatch(s); m != nil {
		if month, ok := savedSearchMonth(m[1]); ok {
			return build(m[3], month, atoi(m[2]))
		}
	}
	if m := savedSearchItemDMY.FindStringSubmatch(s); m != nil {
		if month, ok := savedSearchMonth(m[2]); ok {
			return build(m[3], month, atoi(m[1]))
		}
	}
	if m := savedSearchItemMY.FindStringSubmatch(s); m != nil {
		if month, ok := savedSearchMonth(m[1]); ok {
			return build(m[2], month, 0)
		}
	}
	return "", false
}

// savedSearchLike matches stored against a Zotero date pattern where '_' is
// SQLite's single-character wildcard.
func savedSearchLike(stored, pattern string) bool {
	if len(stored) != len(pattern) {
		return false
	}
	for i := range len(pattern) {
		if pattern[i] != '_' && pattern[i] != stored[i] {
			return false
		}
	}
	return true
}

func (p *savedSearchPlan) addDate(name, op, value string) {
	if !savedSearchDateOps[op] {
		p.refuse("%s: operator %q is not supported", name, op)
		return
	}
	datetime := savedSearchSQLDateTime.MatchString(value)
	if datetime && name == "date" {
		p.refuse("date: a full date-time value %q is not supported", value)
		return
	}
	var floor, pattern string
	if !datetime {
		m := savedSearchDateOnly.FindStringSubmatch(value)
		if m == nil {
			p.refuse("%s: value %q is not YYYY, YYYY-MM, or YYYY-MM-DD", name, value)
			return
		}
		month, day := "00", "00"
		monthPattern, dayPattern := "__", "__"
		if m[2] != "" {
			if m[2] < "01" || m[2] > "12" {
				p.refuse("%s: value %q has an invalid month", name, value)
				return
			}
			month, monthPattern = m[2], m[2]
		}
		if m[3] != "" {
			if m[3] < "01" || m[3] > "31" {
				p.refuse("%s: value %q has an invalid day", name, value)
				return
			}
			day, dayPattern = m[3], m[3]
		}
		floor = m[1] + "-" + month + "-" + day
		pattern = m[1] + "-" + monthPattern + "-" + dayPattern
	}

	// stored holds the per-item comparison string, computed once in prepare so
	// an unparseable item date is reported instead of guessed.
	stored := map[string]string{}
	p.matchers = append(p.matchers, savedSearchMatcher{
		label:  name + " " + op,
		negate: op == "isNot",
		prepare: func(env *savedSearchEnv) []string {
			var bad []string
			for _, it := range env.items {
				switch name {
				case "date":
					raw := ""
					for _, f := range []string{"date", "dateDecided", "dateEnacted", "issueDate"} {
						if v, ok := it.fields[f]; ok {
							raw = v
							break
						}
					}
					if raw == "" {
						continue
					}
					sqlDate, ok := savedSearchItemDate(raw)
					if !ok {
						if len(bad) < 3 {
							bad = append(bad, fmt.Sprintf("%s (%q)", it.key, raw))
						}
						continue
					}
					stored[it.key] = sqlDate
				default:
					t := it.dateAdded
					if name == "dateModified" {
						t = it.dateModified
					}
					if t.IsZero() {
						continue
					}
					if datetime {
						stored[it.key] = t.UTC().Format("2006-01-02 15:04:05")
					} else {
						stored[it.key] = t.Local().Format("2006-01-02")
					}
				}
			}
			if len(bad) > 0 {
				return []string{fmt.Sprintf("date: mirrored item date(s) the evaluator cannot parse, e.g. %s", strings.Join(bad, ", "))}
			}
			return nil
		},
		matches: func(it *savedSearchItem) bool {
			s, ok := stored[it.key]
			if !ok {
				return false
			}
			if datetime {
				switch op {
				case "is", "isNot":
					return strings.EqualFold(s, value)
				case "isBefore":
					return s < value
				default:
					return s > value
				}
			}
			switch op {
			case "is", "isNot":
				return savedSearchLike(s, pattern)
			case "isBefore":
				// A date without a sortable year is stored as 0000-..., which
				// isBefore never matches.
				return s < floor && (name != "date" || s > "0000-00-00")
			default:
				return s > pattern
			}
		},
	})
}

// --- evaluation ---

func (p *savedSearchPlan) evaluate(env *savedSearchEnv) (savedSearchLocalResult, error) {
	var reasons []string
	for _, m := range p.matchers {
		if m.prepare != nil {
			reasons = append(reasons, m.prepare(env)...)
		}
	}
	if len(reasons) > 0 {
		return savedSearchLocalResult{}, &savedSearchUnsupportedError{Reasons: reasons}
	}

	var result map[string]bool
	for i, m := range p.matchers {
		matched := p.matcherSet(env, m)
		switch {
		case i == 0:
			result = matched
		case p.any:
			for key := range matched {
				result[key] = true
			}
		default:
			for key := range result {
				if !matched[key] {
					delete(result, key)
				}
			}
		}
	}

	keys := make([]string, 0, len(result))
	top := make([]string, 0, len(result))
	for key := range result {
		it := env.byKey[key]
		if it == nil || (p.noChildren && it.isChild()) {
			continue
		}
		keys = append(keys, key)
		if !it.isChild() {
			top = append(top, key)
		}
	}
	sort.Strings(keys)
	sort.Strings(top)
	ordered := make([]*savedSearchItem, len(keys))
	for i, key := range keys {
		ordered[i] = env.byKey[key]
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].fields["dateModified"] > ordered[j].fields["dateModified"]
	})
	items := make([]json.RawMessage, len(ordered))
	for i, it := range ordered {
		items[i] = it.raw
	}
	return savedSearchLocalResult{Keys: keys, TopLevelKeys: top, Items: items}, nil
}

// matcherSet is the per-condition selection Zotero builds: the items with a
// matching value, widened by the include* flags, then complemented for a
// negative operator.
func (p *savedSearchPlan) matcherSet(env *savedSearchEnv, m savedSearchMatcher) map[string]bool {
	hits := make(map[string]bool)
	for _, it := range env.items {
		if m.matches(it) {
			hits[it.key] = true
		}
	}
	expanded := make(map[string]bool, len(hits))
	for key := range hits {
		expanded[key] = true
	}
	if p.parentsAndChildren || p.parents {
		for key := range hits {
			if it := env.byKey[key]; it != nil && it.canHaveParentLink() {
				expanded[it.parent] = true
			}
		}
	}
	if p.parentsAndChildren || p.children {
		for _, it := range env.items {
			if it.canHaveParentLink() && hits[it.parent] {
				expanded[it.key] = true
			}
		}
	}
	if !m.negate {
		return expanded
	}
	complement := make(map[string]bool, len(env.items))
	for _, it := range env.items {
		if it.itemType != "annotation" && !expanded[it.key] {
			complement[it.key] = true
		}
	}
	return complement
}

// --- scope integration ---

// savedSearchLocalFallback tries to resolve a saved-search scope from the
// mirror after the live path could not answer. result arrives with
// Precondition and PreconditionDetail describing why the live path failed.
// On success the precondition is cleared and result.Keys holds the cohort's
// top-level items. Otherwise result keeps the unmet precondition and its
// detail additionally says why the mirror could not take over; the fallback
// itself never fails the scope.
func savedSearchLocalFallback(ctx context.Context, flags *rootFlags, db localQueryStore, searchKey string, result scopeResult) scopeResult {
	if flags != nil && flags.dataSource == "live" {
		return result
	}
	decline := func(why string) scopeResult {
		if result.PreconditionDetail != "" {
			result.PreconditionDetail += "; "
		}
		result.PreconditionDetail += "the local mirror could not evaluate it either: " + why
		return result
	}

	mirror := db.Store
	if mirror == nil {
		opened, err := openStoreForRead(ctx, "zotio")
		if err != nil || opened == nil {
			return result
		}
		defer opened.Close()
		mirror = opened
	}

	local, err := evaluateSavedSearchLocal(ctx, mirror, searchKey)
	if err != nil {
		var unsupported *savedSearchUnsupportedError
		switch {
		case errors.Is(err, errSavedSearchNotMirrored):
			return decline(fmt.Sprintf("saved search %s is not in the mirror; run 'zotio sync'", searchKey))
		case errors.As(err, &unsupported):
			return decline(unsupported.Error())
		default:
			return decline(err.Error())
		}
	}

	_, syncedAt, _, stateErr := mirror.GetSyncStateContext(ctx, "items")
	synced := "an unknown time"
	if stateErr == nil && !syncedAt.IsZero() {
		synced = syncedAt.UTC().Format(time.RFC3339)
	}
	reason := result.PreconditionDetail
	if reason == "" {
		reason = "the Zotero desktop local API did not answer"
	}
	result.Keys = local.TopLevelKeys
	if result.Keys == nil {
		result.Keys = []string{}
	}
	result.Precondition = ""
	result.PreconditionDetail = ""
	result.LocalEvaluation = fmt.Sprintf("saved search %s was evaluated against the local mirror (items synced %s) because %s", searchKey, synced, reason)
	if flags != nil && !flags.quiet {
		fmt.Fprintln(os.Stderr, "notice: "+result.LocalEvaluation)
	}
	return result
}
