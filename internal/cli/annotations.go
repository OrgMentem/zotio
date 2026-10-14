// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"zotio/internal/client"
	"zotio/internal/cliutil"
)

func newAnnotationsCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "annotations",
		Short: "Search and export annotations from your Zotero library",
	}
	cmd.AddCommand(newAnnotationsExportCmd(flags))
	cmd.AddCommand(newAnnotationsTimelineCmd(flags))
	cmd.AddCommand(newAnnotationsSearchCmd(flags))
	return cmd
}

// annotationScopeExpr lowers --scope and the legacy cohort flags of one
// annotations command to a single scope expression ("" means the whole
// library). The legacy flags stay supported: with no --scope they join into an
// intersection, and with --scope they must name a cohort the scope already
// requires.
func annotationScopeExpr(flagScope string, sugar ...scopeSugarFlag) (string, error) {
	expr, err := reconcileScopeFlags(flagScope, sugar...)
	if err != nil || expr != "" {
		return expr, err
	}
	arms := make([]string, 0, len(sugar))
	for _, s := range sugar {
		if s.set {
			arms = append(arms, s.expr)
		}
	}
	return strings.Join(arms, scopeAndToken), nil
}

// validateAnnotationScope parses a scope expression and checks its key arms,
// so a malformed scope exits 2 before any read.
func validateAnnotationScope(expr string) error {
	if strings.TrimSpace(expr) == "" {
		return nil
	}
	spec, err := parseScopeSpec(expr)
	if err != nil {
		return err
	}
	return validateScopeKeys(spec)
}

// annotationReadQuery describes one annotation read shared by export and
// timeline. Scope selects top-level items: --item and --collection and every
// other arm name the item that owns the annotations, never the attachment that
// holds them.
type annotationReadQuery struct {
	Scope       string
	Limit       int
	Since       time.Time
	HasSince    bool
	NewestFirst bool
}

// annotationRow is one annotation with the top-level item that owns it. Owner
// is nil when that item could not be read (for example a deleted parent).
type annotationRow struct {
	Summary annotationSummary
	Owner   map[string]any
}

type annotationReadResult struct {
	Rows []annotationRow
	Prov DataProvenance
}

type annotationCohortItem struct {
	Key  string
	Item map[string]any
}

// annotationReader reads annotations through the same source rule as every
// read command: --data-source auto tries the live API first and serves the
// local mirror when Zotero is unreachable. After the first read falls back to
// the mirror, the remaining reads go straight to it, so one run prints one
// fallback note.
type annotationReader struct {
	cmd   *cobra.Command
	flags *rootFlags
	c     *client.Client

	mu   sync.Mutex
	read *rootFlags
	prov DataProvenance
}

func newAnnotationReader(cmd *cobra.Command, flags, readFlags *rootFlags, c *client.Client) *annotationReader {
	return &annotationReader{cmd: cmd, flags: flags, c: c, read: readFlags}
}

func (r *annotationReader) current() *rootFlags {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.read
}

func (r *annotationReader) observe(prov DataProvenance) {
	if prov.Source == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.prov.Source == "" {
		r.prov = prov
	}
	if prov.Source == "local" && r.read.dataSource == "auto" {
		override := *r.read
		override.dataSource = "local"
		r.read = &override
	}
}

// provenance reports where the answer came from. A run that read nothing
// beyond the local mirror's scope arms is local.
func (r *annotationReader) provenance() DataProvenance {
	r.mu.Lock()
	defer r.mu.Unlock()
	prov := r.prov
	if prov.Source == "" {
		prov = DataProvenance{Source: "local", Reason: "scope_mirror"}
	}
	prov.ResourceType = "annotations"
	return prov
}

func (r *annotationReader) get(ctx context.Context, resourceType, path string, params map[string]string) (json.RawMessage, error) {
	data, prov, err := resolveRead(ctx, r.c, r.current(), resourceType, false, path, params, nil)
	if err != nil {
		return nil, err
	}
	r.observe(prov)
	return data, nil
}

func (r *annotationReader) fetchAll(ctx context.Context, path string, params map[string]string, maxItems int) ([]map[string]any, error) {
	items, prov, err := fetchResolvedZoteroItems(ctx, r.c, r.current(), path, params, maxItems)
	if err != nil {
		return nil, err
	}
	r.observe(prov)
	return items, nil
}

func decodeZoteroItem(data json.RawMessage) (map[string]any, error) {
	var item map[string]any
	if err := json.Unmarshal(data, &item); err != nil {
		return nil, fmt.Errorf("expected a Zotero item object: %w", err)
	}
	return item, nil
}

// collect reads the annotations a query selects. A restricted scope walks
// down from each top-level item through its attachments to their annotations;
// an unrestricted query lists annotations directly, newest first, and then
// looks up the owners of the rows it keeps.
func (r *annotationReader) collect(ctx context.Context, q annotationReadQuery) (annotationReadResult, error) {
	cohort, restricted, err := r.resolveCohort(ctx, q.Scope)
	if err != nil {
		return annotationReadResult{}, err
	}
	var rows []annotationRow
	if restricted {
		rows, err = r.cohortRows(ctx, cohort)
	} else {
		rows, err = r.libraryRows(ctx, q)
	}
	if err != nil {
		return annotationReadResult{}, err
	}
	if q.HasSince {
		kept := rows[:0]
		for _, row := range rows {
			added, perr := parseZoteroTime(row.Summary.DateAdded)
			if perr != nil || !added.After(q.Since) {
				continue
			}
			kept = append(kept, row)
		}
		rows = kept
	}
	if q.NewestFirst {
		rows = sortByAnnotationInstantDesc(rows, func(row annotationRow) annotationSummary { return row.Summary })
	}
	if q.Limit > 0 && len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	if !restricted {
		rows, err = r.attachOwners(ctx, rows)
		if err != nil {
			return annotationReadResult{}, err
		}
	}
	return annotationReadResult{Rows: rows, Prov: r.provenance()}, nil
}

func (r *annotationReader) libraryRows(ctx context.Context, q annotationReadQuery) ([]annotationRow, error) {
	// The requested limit is applied after the client-side --since filter, so
	// page the full set when a filter is active instead of capping the fetch
	// at the first page.
	maxItems := q.Limit
	if q.HasSince {
		maxItems = 0
	}
	items, err := r.fetchAll(ctx, "/items", map[string]string{
		"itemType":  "annotation",
		"sort":      "dateAdded",
		"direction": "desc",
	}, maxItems)
	if err != nil {
		return nil, classifyAPIError(err, r.flags)
	}
	summaries := annotationSummariesFromItems(items)
	rows := make([]annotationRow, 0, len(summaries))
	for _, summary := range summaries {
		rows = append(rows, annotationRow{Summary: summary})
	}
	return rows, nil
}

func (r *annotationReader) cohortRows(ctx context.Context, cohort []annotationCohortItem) ([]annotationRow, error) {
	results, errs := cliutil.FanoutRun(ctx, cohort,
		func(ci annotationCohortItem) string { return ci.Key },
		func(fctx context.Context, ci annotationCohortItem) ([]annotationRow, error) {
			return r.itemRows(fctx, ci)
		})
	if len(errs) > 0 {
		return nil, classifyAPIError(errs[0].Err, r.flags)
	}
	seen := make(map[string]bool)
	rows := make([]annotationRow, 0)
	for _, res := range results {
		for _, row := range res.Value {
			if key := row.Summary.Key; key != "" {
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// itemRows returns the annotations of one top-level item. Zotero attaches an
// annotation to the attachment (the PDF), not to the item, so this reads the
// item's children and then the annotation children of each attachment.
// Annotations that are direct children (the item is itself an attachment) are
// kept as they are.
func (r *annotationReader) itemRows(ctx context.Context, ci annotationCohortItem) ([]annotationRow, error) {
	item := ci.Item
	if item == nil {
		data, err := r.get(ctx, "items", "/items/"+url.PathEscape(ci.Key), nil)
		if err != nil {
			return nil, err
		}
		if item, err = decodeZoteroItem(data); err != nil {
			return nil, err
		}
	}
	if !zoteroItemHasChildren(item) {
		return nil, nil
	}
	// Page through the children: an item with many attachments spans pages.
	children, err := r.fetchAll(ctx, "/items/"+url.PathEscape(ci.Key)+"/children", nil, 0)
	if err != nil {
		return nil, err
	}
	summaries := annotationSummariesFromItems(directAnnotationChildren(children))
	for _, child := range children {
		attachmentKey := zoteroString(child, "key")
		if zoteroString(child, "itemType") != "attachment" || attachmentKey == "" {
			continue
		}
		// Page through the annotations: Zotero returns one page at a time.
		annotationItems, err := r.fetchAll(ctx, "/items/"+url.PathEscape(attachmentKey)+"/children", map[string]string{"itemType": "annotation"}, 0)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, annotationSummariesFromItems(annotationItems)...)
	}
	title := zoteroString(item, "title")
	rows := make([]annotationRow, 0, len(summaries))
	for _, summary := range summaries {
		summary.ItemKey = ci.Key
		summary.ItemTitle = title
		rows = append(rows, annotationRow{Summary: summary, Owner: item})
	}
	return rows, nil
}

// directAnnotationChildren keeps the children that are annotations.
func directAnnotationChildren(children []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(children))
	for _, child := range children {
		if zoteroString(child, "itemType") == "annotation" {
			out = append(out, child)
		}
	}
	return out
}

type annotationAttachmentInfo struct {
	parent string
	item   map[string]any
}

// attachOwners finds the top-level item behind each annotation's attachment
// and records it on the row. An attachment with no parent is its own owner.
// A deleted attachment or parent leaves the row without an owner.
func (r *annotationReader) attachOwners(ctx context.Context, rows []annotationRow) ([]annotationRow, error) {
	if len(rows) == 0 {
		return rows, nil
	}
	seen := make(map[string]bool)
	var attachmentKeys []string
	for _, row := range rows {
		key := row.Summary.ParentItem
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		attachmentKeys = append(attachmentKeys, key)
	}
	identity := func(key string) string { return key }
	attachments, errs := cliutil.FanoutRun(ctx, attachmentKeys, identity,
		func(fctx context.Context, key string) (annotationAttachmentInfo, error) {
			data, err := r.get(fctx, "items", "/items/"+url.PathEscape(key), nil)
			if err != nil {
				if apiStatus(err) == 404 {
					return annotationAttachmentInfo{}, nil
				}
				return annotationAttachmentInfo{}, err
			}
			item, err := decodeZoteroItem(data)
			if err != nil {
				return annotationAttachmentInfo{}, err
			}
			return annotationAttachmentInfo{parent: zoteroString(item, "parentItem"), item: item}, nil
		})
	if len(errs) > 0 {
		return nil, classifyAPIError(errs[0].Err, r.flags)
	}
	byAttachment := make(map[string]annotationAttachmentInfo, len(attachments))
	var parentKeys []string
	seenParent := make(map[string]bool)
	for _, res := range attachments {
		byAttachment[res.Source] = res.Value
		if p := res.Value.parent; p != "" && !seenParent[p] {
			seenParent[p] = true
			parentKeys = append(parentKeys, p)
		}
	}
	parents, errs := cliutil.FanoutRun(ctx, parentKeys, identity,
		func(fctx context.Context, key string) (map[string]any, error) {
			data, err := r.get(fctx, "items", "/items/"+url.PathEscape(key), nil)
			if err != nil {
				if apiStatus(err) == 404 {
					return nil, nil
				}
				return nil, err
			}
			return decodeZoteroItem(data)
		})
	if len(errs) > 0 {
		return nil, classifyAPIError(errs[0].Err, r.flags)
	}
	byParent := make(map[string]map[string]any, len(parents))
	for _, res := range parents {
		byParent[res.Source] = res.Value
	}
	for i := range rows {
		info, ok := byAttachment[rows[i].Summary.ParentItem]
		if !ok || info.item == nil {
			continue
		}
		owner, ownerKey := info.item, rows[i].Summary.ParentItem
		if info.parent != "" {
			owner, ownerKey = byParent[info.parent], info.parent
		}
		if owner == nil {
			continue
		}
		rows[i].Owner = owner
		rows[i].Summary.ItemKey = ownerKey
		rows[i].Summary.ItemTitle = zoteroString(owner, "title")
	}
	return rows, nil
}

// resolveCohort maps a scope expression to the top-level items it names.
// restricted is false for the whole library. collection, tag, and item arms
// read through the shared source rule; query and saved-search arms need the
// local mirror, as they do in the other scope adopters.
func (r *annotationReader) resolveCohort(ctx context.Context, expr string) ([]annotationCohortItem, bool, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, false, nil
	}
	spec, err := parseScopeSpec(expr)
	if err != nil {
		return nil, false, err
	}
	if err := validateScopeKeys(spec); err != nil {
		return nil, false, err
	}
	if spec.Type == "library" {
		return nil, false, nil
	}
	arms := []scopeSpec{spec}
	if spec.Type == scopeTypeIntersection {
		arms = spec.Arms
	}
	var cohort []annotationCohortItem
	for i, arm := range arms {
		items, err := r.armItems(ctx, arm)
		if err != nil {
			return nil, false, err
		}
		if i == 0 {
			cohort = items
			continue
		}
		cohort = intersectAnnotationCohorts(cohort, items)
	}
	if cohort == nil {
		cohort = []annotationCohortItem{}
	}
	return cohort, true, nil
}

func intersectAnnotationCohorts(a, b []annotationCohortItem) []annotationCohortItem {
	inB := make(map[string]annotationCohortItem, len(b))
	for _, item := range b {
		inB[item.Key] = item
	}
	out := make([]annotationCohortItem, 0, len(a))
	for _, item := range a {
		other, ok := inB[item.Key]
		if !ok {
			continue
		}
		if item.Item == nil {
			item.Item = other.Item
		}
		out = append(out, item)
	}
	return out
}

func (r *annotationReader) armItems(ctx context.Context, arm scopeSpec) ([]annotationCohortItem, error) {
	switch arm.Type {
	case "item":
		data, err := r.get(ctx, "items", "/items/"+url.PathEscape(arm.Value), nil)
		if err != nil {
			if apiStatus(err) == 404 {
				return nil, notFoundErr(fmt.Errorf("item %s not found", arm.Value))
			}
			return nil, classifyAPIError(err, r.flags)
		}
		item, err := decodeZoteroItem(data)
		if err != nil {
			return nil, err
		}
		return []annotationCohortItem{{Key: arm.Value, Item: item}}, nil
	case "collection":
		escaped := url.PathEscape(arm.Value)
		if _, err := r.get(ctx, "collections", "/collections/"+escaped, nil); err != nil {
			if apiStatus(err) == 404 {
				return nil, notFoundErr(fmt.Errorf("collection %s not found", arm.Value))
			}
			return nil, classifyAPIError(err, r.flags)
		}
		return r.listCohort(ctx, "/collections/"+escaped+"/items/top", nil)
	case "tag":
		return r.listCohort(ctx, "/items/top", map[string]string{"tag": arm.Value})
	default:
		return r.mirrorCohort(ctx, arm)
	}
}

func (r *annotationReader) listCohort(ctx context.Context, path string, params map[string]string) ([]annotationCohortItem, error) {
	items, err := r.fetchAll(ctx, path, params, 0)
	if err != nil {
		return nil, classifyAPIError(err, r.flags)
	}
	out := make([]annotationCohortItem, 0, len(items))
	for _, item := range items {
		if key := zoteroString(item, "key"); key != "" {
			out = append(out, annotationCohortItem{Key: key, Item: item})
		}
	}
	return out, nil
}

func (r *annotationReader) mirrorCohort(ctx context.Context, arm scopeSpec) ([]annotationCohortItem, error) {
	db, err := openStoreForRead(ctx, "zotio")
	if err != nil {
		return nil, fmt.Errorf("opening local database: %w", err)
	}
	if db == nil {
		return nil, errUnsyncedStoreFor(r.cmd, r.flags)
	}
	defer db.Close()
	result, err := resolveScopeLive(ctx, r.flags, localQueryStore{Store: db}, arm)
	if err != nil {
		return nil, err
	}
	if result.Precondition != "" {
		return nil, scopePreconditionErr(ctx, r.cmd.OutOrStdout(), r.flags, commandRegistryPath(r.cmd), result)
	}
	out := make([]annotationCohortItem, 0, len(result.Keys))
	for _, key := range result.Keys {
		out = append(out, annotationCohortItem{Key: key})
	}
	return out, nil
}

// noteEmptyLocalAnnotations tells the caller that an empty answer came from
// the local mirror, which may lag the library.
func noteEmptyLocalAnnotations(cmd *cobra.Command, prov DataProvenance, count int) {
	if count > 0 || prov.Source != "local" {
		return
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "note: no annotations matched in the local mirror; it can lag the library. Run 'zotio sync', or use --data-source live")
}
